package claude

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/fsutil"
	"github.com/orlenko/aiq/internal/redact"
	"github.com/orlenko/aiq/internal/state"
)

// Quota polling uses a grant aiq owns, separate from the one Claude Code
// keeps in its credential store. Two reasons: Claude Code's grant is revoked
// server-side on /logout and rotated on refresh, so copying it is fragile;
// and reading it back requires per-platform credential-store code. The aiq
// grant is minted once per account through the same PKCE flow Claude Code
// itself uses, and lives as a private file inside the account home.

const (
	oauthClientID    = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	oauthTokenURL    = "https://api.anthropic.com/v1/oauth/token"
	oauthUsageURL    = "https://api.anthropic.com/api/oauth/usage?cedar_ember=1"
	oauthPrepaidURL  = "https://api.anthropic.com/api/oauth/organizations/%s/prepaid/credits"
	oauthResetURL    = "https://api.anthropic.com/api/organizations/%s/reset_rate_limits"
	oauthProfileURL  = "https://api.anthropic.com/api/oauth/profile"
	oauthAuthorize   = "https://claude.ai/oauth/authorize"
	oauthRedirectURI = "https://console.anthropic.com/oauth/code/callback"
	oauthScopes      = "user:profile user:inference user:sessions:claude_code user:mcp_servers"

	// refreshSkew: refresh when less than this remains. aiquota uses five
	// minutes; staying ahead of it means that when both tools share a grant
	// file, aiq refreshes first and aiquota reads the result.
	refreshSkew = 10 * time.Minute
	httpTimeout = 25 * time.Second
	grantFile   = ".aiq-grant.json"

	// cliUserAgent: the usage endpoint reports reset grants (cedar_ember)
	// only to Claude Code; any other client gets ineligible_reason
	// "surface". The version is the one this was verified against.
	cliUserAgent = "claude-cli/2.1.281 (external, cli)"
	// resetProgram is the reset-grant program Claude Code's /limit-reset
	// claims from.
	resetProgram = "cedar_ember"
)

// Grant is the on-disk shape, compatible with Claude Code's credential file
// and with aiquota's grant files.
type Grant struct {
	OAuth struct {
		AccessToken  string   `json:"accessToken"`
		RefreshToken string   `json:"refreshToken"`
		ExpiresAt    int64    `json:"expiresAt"` // unix milliseconds
		Scopes       []string `json:"scopes,omitempty"`
	} `json:"claudeAiOauth"`
}

// GrantPath returns where the poll grant for a home lives.
func GrantPath(home string) string { return filepath.Join(home, grantFile) }

// HasGrant reports whether a home has a poll grant.
func HasGrant(home string) bool {
	_, err := os.Stat(GrantPath(home))
	return err == nil
}

func LoadGrant(path string) (*Grant, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var g Grant
	if err := json.Unmarshal(data, &g); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if g.OAuth.AccessToken == "" {
		return nil, fmt.Errorf("%s has no access token", path)
	}
	return &g, nil
}

func SaveGrant(path string, g *Grant) error {
	data, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, append(data, '\n'))
}

func (g *Grant) expiresAt() time.Time {
	if g.OAuth.ExpiresAt == 0 {
		return time.Time{}
	}
	return time.UnixMilli(g.OAuth.ExpiresAt)
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func openBrowser(u string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	default:
		if _, err := exec.LookPath("xdg-open"); err != nil {
			return
		}
		cmd = exec.Command("xdg-open", u)
	}
	cmd.Start()
}

// Authorize runs the PKCE flow interactively: prints the URL, opens the
// browser when possible, reads the pasted code, and returns the grant.
func Authorize(in io.Reader, out io.Writer) (*Grant, error) {
	verifier := make([]byte, 32)
	rand.Read(verifier)
	stateBytes := make([]byte, 32)
	rand.Read(stateBytes)
	verifierStr := b64url(verifier)
	sum := sha256.Sum256([]byte(verifierStr))
	stateStr := b64url(stateBytes)
	q := url.Values{
		"code":                  {"true"},
		"client_id":             {oauthClientID},
		"response_type":         {"code"},
		"redirect_uri":          {oauthRedirectURI},
		"scope":                 {oauthScopes},
		"code_challenge":        {b64url(sum[:])},
		"code_challenge_method": {"S256"},
		"state":                 {stateStr},
	}
	authURL := oauthAuthorize + "?" + q.Encode()
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Authorize quota polling for this account. A browser page opens; sign in as the")
	fmt.Fprintln(out, "SAME account you just logged Claude Code into, then paste the code it shows.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  "+authURL)
	fmt.Fprintln(out)
	openBrowser(authURL)
	fmt.Fprint(out, "code: ")
	reader := bufio.NewReader(in)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return nil, fmt.Errorf("no code entered")
	}
	pasted := strings.TrimSpace(line)
	if pasted == "" {
		return nil, fmt.Errorf("no code entered")
	}
	code, returnedState, _ := strings.Cut(pasted, "#")
	if returnedState == "" {
		returnedState = stateStr
	}
	body, err := postJSON(oauthTokenURL, map[string]any{
		"grant_type":    "authorization_code",
		"code":          strings.TrimSpace(code),
		"redirect_uri":  oauthRedirectURI,
		"client_id":     oauthClientID,
		"code_verifier": verifierStr,
		"state":         strings.TrimSpace(returnedState),
	})
	if err != nil {
		return nil, fmt.Errorf("token exchange: %w", err)
	}
	return grantFromTokenResponse(nil, body)
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
}

var tokenErrorShape = regexp.MustCompile(`^[a-z_]{1,64}$`)

// tokenErrorCode is the short OAuth code in a token endpoint's answer, or "". It is
// the only part of that answer an error may show.
func tokenErrorCode(body []byte) string {
	var r struct {
		Error any `json:"error"`
	}
	if json.Unmarshal(body, &r) == nil {
		if code, ok := r.Error.(string); ok && tokenErrorShape.MatchString(code) {
			return code
		}
	}
	return ""
}

func tokenResponseError(body []byte) error {
	if code := tokenErrorCode(body); code != "" {
		return fmt.Errorf("token response without access_token: %s", code)
	}
	return fmt.Errorf("token response could not be used, and aiq does not show its text")
}

func grantFromTokenResponse(prev *Grant, body []byte) (*Grant, error) {
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil || tr.AccessToken == "" {
		return nil, tokenResponseError(body)
	}
	g := &Grant{}
	if prev != nil {
		*g = *prev
	}
	g.OAuth.AccessToken = tr.AccessToken
	if tr.RefreshToken != "" {
		g.OAuth.RefreshToken = tr.RefreshToken
	}
	expires := tr.ExpiresIn
	if expires == 0 {
		expires = 3600
	}
	g.OAuth.ExpiresAt = time.Now().Add(time.Duration(expires) * time.Second).UnixMilli()
	if tr.Scope != "" {
		g.OAuth.Scopes = strings.Fields(tr.Scope)
	}
	return g, nil
}

// Refresh exchanges the refresh token for a new access token.
func (g *Grant) Refresh() (*Grant, error) {
	if g.OAuth.RefreshToken == "" {
		return nil, fmt.Errorf("grant has no refresh token")
	}
	body, err := postJSON(oauthTokenURL, map[string]any{
		"grant_type":    "refresh_token",
		"refresh_token": g.OAuth.RefreshToken,
		"client_id":     oauthClientID,
	})
	if err != nil {
		return nil, fmt.Errorf("token refresh: %w", err)
	}
	return grantFromTokenResponse(g, body)
}

// Usage is what one poll of an account yields.
type Usage struct {
	Windows  []state.Window
	Identity string
	Plan     string
	Notes    []string
	// ResetCredits counts usage-limit resets Anthropic granted the account
	// (see NormalizeResetCredits); ResetCreditExpiry is the soonest grant's
	// end (unix seconds, 0 = none), ResetCreditID the grant to claim first.
	ResetCredits      int
	ResetCreditExpiry int64
	ResetCreditID     string
	// Extra is the account's paid extra usage; nil when the poll could not
	// read it.
	Extra *state.ExtraUsage
}

// freshGrant loads the grant at path, refreshing and writing it back when
// it is close to expiry.
func freshGrant(path string, now time.Time) (*Grant, error) {
	g, err := LoadGrant(path)
	if err != nil {
		return nil, err
	}
	if exp := g.expiresAt(); !exp.IsZero() && exp.Sub(now) < refreshSkew {
		refreshed, err := g.Refresh()
		if err != nil {
			if exp.Before(now) {
				return nil, fmt.Errorf("grant expired and %v — run: aiq account authorize", err)
			}
			// Still valid for a few minutes; use it and try again next time.
		} else {
			if err := SaveGrant(path, refreshed); err != nil {
				return nil, fmt.Errorf("save refreshed grant: %w", err)
			}
			g = refreshed
		}
	}
	return g, nil
}

func authHeaders(g *Grant) map[string]string {
	return map[string]string{
		"Authorization":  "Bearer " + g.OAuth.AccessToken,
		"anthropic-beta": "oauth-2025-04-20",
		"Accept":         "application/json",
		"User-Agent":     cliUserAgent,
		"x-app":          "cli",
	}
}

// Poll refreshes the grant at path when needed (writing it back) and fetches
// usage. It never touches Claude Code's own credential.
func Poll(path string, now time.Time) (*Usage, error) {
	g, err := freshGrant(path, now)
	if err != nil {
		return nil, err
	}
	headers := authHeaders(g)
	status, body, err := getJSON(oauthUsageURL, headers)
	if err != nil {
		return nil, fmt.Errorf("usage: %w", err)
	}
	u, err := usageFromReply(status, body, now)
	if err != nil {
		return nil, err
	}
	if pstatus, profile, err := getJSON(oauthProfileURL, headers); err == nil && pstatus == 200 {
		u.Identity, u.Plan = ParseProfile(profile)
		// The prepaid balance lives under the organization. Read it only
		// when extra usage is on: it is what that usage draws on.
		if org := ParseOrganization(profile); org != "" && u.Extra != nil && u.Extra.Enabled {
			cstatus, credits, err := getJSON(fmt.Sprintf(oauthPrepaidURL, url.PathEscape(org)), headers)
			if err == nil && cstatus == 200 {
				ParsePrepaid(credits, u.Extra, now)
			}
		}
	}
	return u, nil
}

// ParseExtraUsage reads the extra_usage block of the usage payload: whether
// paid overflow is on, and this month's spend against its limit. The
// prepaid balance is not in it (see ParsePrepaid), so it starts unknown.
func ParseExtraUsage(body []byte, now time.Time) *state.ExtraUsage {
	var doc struct {
		Extra *struct {
			Enabled        bool     `json:"is_enabled"`
			MonthlyLimit   *float64 `json:"monthly_limit"`
			UsedCredits    *float64 `json:"used_credits"`
			Currency       string   `json:"currency"`
			DisabledReason any      `json:"disabled_reason"`
			LimitReached   bool     `json:"spend_limit_reached"`
		} `json:"extra_usage"`
	}
	if json.Unmarshal(body, &doc) != nil || doc.Extra == nil {
		return nil
	}
	e := &state.ExtraUsage{
		Enabled:      doc.Extra.Enabled,
		Currency:     doc.Extra.Currency,
		BalanceMinor: -1,
		LimitReached: doc.Extra.LimitReached,
		ObservedAt:   now.Unix(),
	}
	if doc.Extra.MonthlyLimit != nil {
		e.LimitMinor = int64(*doc.Extra.MonthlyLimit + 0.5)
	}
	if doc.Extra.UsedCredits != nil {
		e.UsedMinor = int64(*doc.Extra.UsedCredits + 0.5)
	}
	if doc.Extra.DisabledReason != nil {
		e.DisabledReason = fmt.Sprint(doc.Extra.DisabledReason)
	}
	return e
}

// ParsePrepaid fills in the prepaid credit balance and the auto-reload
// setting from the organization's prepaid/credits payload. Both are set,
// with BalanceAt, only when the payload has a balance; any
// auto_reload_settings object other than one saying enabled false counts
// as auto-reload on, since only null has been seen for "off".
func ParsePrepaid(body []byte, e *state.ExtraUsage, now time.Time) {
	var doc struct {
		Amount   *int64 `json:"amount"`
		Currency string `json:"currency"`
		Balance  *struct {
			Money *struct {
				AmountMinor *int64 `json:"amount_minor"`
				Currency    string `json:"currency"`
			} `json:"money"`
		} `json:"balance"`
		AutoReload map[string]any `json:"auto_reload_settings"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return
	}
	var balance *int64
	currency := doc.Currency
	switch {
	case doc.Balance != nil && doc.Balance.Money != nil && doc.Balance.Money.AmountMinor != nil:
		balance, currency = doc.Balance.Money.AmountMinor, doc.Balance.Money.Currency
	case doc.Amount != nil:
		balance = doc.Amount
	}
	if balance == nil {
		return
	}
	e.BalanceMinor = max(*balance, 0)
	if e.Currency == "" {
		e.Currency = currency
	}
	e.AutoReload = doc.AutoReload != nil
	for _, key := range []string{"enabled", "is_enabled"} {
		if v, ok := doc.AutoReload[key].(bool); ok {
			e.AutoReload = v
			break
		}
	}
	e.BalanceAt = now.Unix()
}

// NormalizeResetCredits reads the cedar_ember block of the usage payload:
// usage-limit resets Anthropic grants an account (a model-launch promo, for
// one), which Claude Code offers through /limit-reset. Each reset clears the
// session and weekly windows. It counts the resets left on live grants and
// names the soonest-ending one; the backend's next_grant_id wins when it is
// among them.
func NormalizeResetCredits(body []byte, now time.Time) (credits int, expiry int64, id string) {
	var doc struct {
		CedarEmber *struct {
			Eligible bool `json:"eligible"`
			Grants   []struct {
				ID         string `json:"id"`
				ResetsLeft int    `json:"resets_left"`
				EndsAt     string `json:"ends_at"`
				Paused     bool   `json:"paused"`
			} `json:"grants"`
			NextGrantID string `json:"next_grant_id"`
		} `json:"cedar_ember"`
	}
	if json.Unmarshal(body, &doc) != nil || doc.CedarEmber == nil || !doc.CedarEmber.Eligible {
		return 0, 0, ""
	}
	next := ""
	for _, g := range doc.CedarEmber.Grants {
		end := parseISO(g.EndsAt)
		if g.ResetsLeft <= 0 || g.Paused || (end > 0 && end <= now.Unix()) {
			continue
		}
		credits += g.ResetsLeft
		if id == "" || (end > 0 && (expiry == 0 || end < expiry)) {
			id = g.ID
		}
		if end > 0 && (expiry == 0 || end < expiry) {
			expiry = end
		}
		if g.ID == doc.CedarEmber.NextGrantID {
			next = g.ID
		}
	}
	if next != "" {
		id = next
	}
	return credits, expiry, id
}

// ConsumeResetCredit claims one reset from grantID, the way Claude Code's
// /limit-reset does. requestID ([A-Za-z0-9_-]{1,64}) identifies the claim.
// outcome is the backend's result: reset, not_limited, cooldown,
// already_used, ineligible, unavailable, rate_limited.
func ConsumeResetCredit(path, grantID, requestID string) (outcome string, err error) {
	if grantID == "" {
		return "", fmt.Errorf("no reset grant to claim")
	}
	g, err := freshGrant(path, time.Now())
	if err != nil {
		return "", err
	}
	headers := authHeaders(g)
	status, profile, err := getJSON(oauthProfileURL, headers)
	if err != nil {
		return "", fmt.Errorf("profile: %w", err)
	}
	if status != 200 {
		return "", fmt.Errorf("profile: HTTP %d %s", status, truncate(errorDetail(profile), 140))
	}
	org := ParseOrganization(profile)
	if org == "" {
		return "", fmt.Errorf("profile has no organization")
	}
	status, body, err := sendJSON("POST", fmt.Sprintf(oauthResetURL, url.PathEscape(org)), headers, map[string]any{
		"program":    resetProgram,
		"grant_id":   grantID,
		"request_id": requestID,
	})
	if err != nil {
		return "", fmt.Errorf("reset: %w", err)
	}
	if status != 200 {
		return "", fmt.Errorf("reset: HTTP %d %s", status, truncate(errorDetail(body), 140))
	}
	var res struct {
		Result string `json:"result"`
		Reason string `json:"reason"`
	}
	if json.Unmarshal(body, &res) != nil || res.Result == "" {
		detail, _ := redact.Text(string(body))
		return "", fmt.Errorf("reset: unreadable response: %s", truncate(detail, 140))
	}
	if res.Reason != "" && res.Result != "reset" {
		return res.Result + " (" + res.Reason + ")", nil
	}
	return res.Result, nil
}

// usageFromReply reads the usage endpoint's reply. A status other than 200,
// or a 200 that is not the usage document, is an error.
func usageFromReply(status int, body []byte, now time.Time) (*Usage, error) {
	if status == 401 {
		return nil, fmt.Errorf("usage: HTTP 401 — the poll grant was revoked; run: aiq account authorize")
	}
	if status != 200 {
		return nil, fmt.Errorf("usage: HTTP %d %s", status, truncate(errorDetail(body), 140))
	}
	ws, err := NormalizeUsage(body, now)
	if err != nil {
		return nil, err
	}
	u := &Usage{Windows: ws}
	u.ResetCredits, u.ResetCreditExpiry, u.ResetCreditID = NormalizeResetCredits(body, now)
	u.Extra = ParseExtraUsage(body, now)
	return u, nil
}

// NormalizeUsage converts the usage payload into windows with the same keys
// aiquota uses: "session", "weekly", "weekly:<model>". It is an error
// when body is not the usage document (none of five_hour, seven_day and
// limits is there), or when one of those is there, not null, and not in the
// form aiq reads: a changed format must not pass for "no window running".
// A null field, or limits with no entry aiq reads, is no window.
func NormalizeUsage(body []byte, now time.Time) ([]state.Window, error) {
	var doc map[string]json.RawMessage
	if json.Unmarshal(body, &doc) != nil {
		return nil, errNotUsage
	}
	var out []state.Window
	obs := now.Unix()
	present := false
	for _, f := range []struct {
		field, key, label, kind string
		seconds                 int64
	}{
		{"five_hour", "session", "Session", state.KindShort, 5 * 3600},
		{"seven_day", "weekly", "Weekly", state.KindWeekly, 7 * 86400},
	} {
		raw, ok := doc[f.field]
		if !ok {
			continue
		}
		present = true
		if isNull(raw) {
			continue
		}
		var w struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    string   `json:"resets_at"`
		}
		if json.Unmarshal(raw, &w) != nil || w.Utilization == nil {
			return nil, fmt.Errorf("usage: %s is not in the form aiq reads", f.field)
		}
		out = append(out, state.Window{
			Key: f.key, Label: f.label, Kind: f.kind, UsedPct: *w.Utilization,
			ResetsAt: parseISO(w.ResetsAt), WindowSeconds: f.seconds, ObservedAt: obs,
		})
	}
	if raw, ok := doc["limits"]; ok {
		present = true
		var limits []struct {
			Kind     string   `json:"kind"`
			Percent  *float64 `json:"percent"`
			ResetsAt string   `json:"resets_at"`
			Severity string   `json:"severity"`
			Scope    struct {
				Surface string `json:"surface"`
				Model   struct {
					DisplayName string `json:"display_name"`
				} `json:"model"`
			} `json:"scope"`
		}
		if !isNull(raw) && json.Unmarshal(raw, &limits) != nil {
			return nil, errors.New("usage: limits is not in the form aiq reads")
		}
		for _, l := range limits {
			if l.Kind != "weekly_scoped" {
				continue
			}
			if l.Percent == nil {
				return nil, errors.New("usage: a weekly_scoped limit has no percent")
			}
			label := l.Scope.Model.DisplayName
			if label == "" {
				label = l.Scope.Surface
			}
			if label == "" {
				label = "Scoped"
			}
			severity := l.Severity
			if severity == "normal" {
				severity = ""
			}
			out = append(out, state.Window{
				Key: "weekly:" + strings.ToLower(label), Label: label, Kind: state.KindWeekly, Scope: label,
				UsedPct: *l.Percent, ResetsAt: parseISO(l.ResetsAt), WindowSeconds: 7 * 86400,
				Severity: severity, ObservedAt: obs,
			})
		}
	}
	if !present {
		return nil, errNotUsage
	}
	return out, nil
}

var errNotUsage = errors.New("usage: the reply is not usage data")

func isNull(raw json.RawMessage) bool { return string(bytes.TrimSpace(raw)) == "null" }

// ParseProfile extracts the account email and a short plan name
// ("default_claude_max_20x" → "max 20x").
func ParseProfile(body []byte) (email, plan string) {
	var doc map[string]any
	if json.Unmarshal(body, &doc) != nil {
		return "", ""
	}
	email = findEmail(doc, 0)
	if org, ok := doc["organization"].(map[string]any); ok {
		if tier, ok := org["rate_limit_tier"].(string); ok && tier != "" {
			name := strings.TrimPrefix(tier, "default_")
			name = strings.ReplaceAll(name, "claude_", "")
			plan = strings.TrimSpace(strings.ReplaceAll(name, "_", " "))
		} else if kind, ok := org["organization_type"].(string); ok {
			plan = strings.ReplaceAll(strings.ReplaceAll(kind, "claude_", ""), "_", " ")
		}
	}
	return email, plan
}

// ParseOrganization returns the account's organization uuid, which the
// reset endpoint is scoped to.
func ParseOrganization(body []byte) string {
	var doc struct {
		Organization struct {
			UUID string `json:"uuid"`
		} `json:"organization"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return ""
	}
	return doc.Organization.UUID
}

func findEmail(node any, depth int) string {
	if depth > 4 {
		return ""
	}
	switch v := node.(type) {
	case map[string]any:
		for k, val := range v {
			if s, ok := val.(string); ok && strings.Contains(strings.ToLower(k), "email") && strings.Contains(s, "@") {
				return s
			}
		}
		for _, val := range v {
			if e := findEmail(val, depth+1); e != "" {
				return e
			}
		}
	case []any:
		for _, val := range v {
			if e := findEmail(val, depth+1); e != "" {
				return e
			}
		}
	}
	return ""
}

var fracRe = regexp.MustCompile(`\.(\d{1,9})\d*`)

func parseISO(s string) int64 {
	if s == "" {
		return 0
	}
	s = strings.TrimSpace(s)
	s = fracRe.ReplaceAllString(s, ".$1")
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999999999"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Unix()
		}
	}
	return 0
}

// --- http helpers ---

var httpClient = &http.Client{Timeout: httpTimeout}

func postJSON(u string, payload any) ([]byte, error) {
	data, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", u, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "aiq/0.2")
	// A redirect is not followed, as that would send the body on to the new address.
	client := *httpClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		// Go's text here can quote the address or a header the server sent.
		var timeout interface{ Timeout() bool }
		if errors.As(err, &timeout) && timeout.Timeout() {
			return nil, fmt.Errorf("the token endpoint timed out")
		}
		return nil, fmt.Errorf("could not reach the token endpoint")
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		if code := tokenErrorCode(body); code != "" {
			return nil, fmt.Errorf("HTTP %d %s", resp.StatusCode, code)
		}
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return body, nil
}

func getJSON(u string, headers map[string]string) (int, []byte, error) {
	return sendJSON("GET", u, headers, nil)
}

// sendJSON makes an authenticated request; payload, when not nil, is sent
// as the JSON body.
func sendJSON(method, u string, headers map[string]string, payload any) (int, []byte, error) {
	var rd io.Reader
	if payload != nil {
		data, _ := json.Marshal(payload)
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		return 0, nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("User-Agent", "aiq/0.2")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body, nil
}

// errorDetail is the provider's explanation in an HTTP error, redacted.
func errorDetail(body []byte) string {
	detail, _ := redact.Text(rawErrorDetail(body))
	return detail
}

func rawErrorDetail(body []byte) string {
	var doc map[string]any
	if json.Unmarshal(body, &doc) == nil {
		for _, k := range []string{"error_description", "detail", "error", "message"} {
			switch v := doc[k].(type) {
			case string:
				return v
			case map[string]any:
				if m, ok := v["message"].(string); ok {
					return m
				}
			}
		}
	}
	s := strings.TrimSpace(string(body))
	if strings.HasPrefix(strings.ToLower(s), "<!doctype") || strings.HasPrefix(strings.ToLower(s), "<html") {
		return "HTML challenge page"
	}
	return s
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n]
	}
	return s
}
