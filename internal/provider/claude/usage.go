package claude

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
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
	oauthUsageURL    = "https://api.anthropic.com/api/oauth/usage?cedar_ember=1&skip_spend=1"
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

func grantFromTokenResponse(prev *Grant, body []byte) (*Grant, error) {
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil || tr.AccessToken == "" {
		return nil, fmt.Errorf("token response without access_token: %s", truncate(string(body), 200))
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
	if status == 401 {
		return nil, fmt.Errorf("usage: HTTP 401 — the poll grant was revoked; run: aiq account authorize")
	}
	if status != 200 {
		return nil, fmt.Errorf("usage: HTTP %d %s", status, truncate(errorDetail(body), 140))
	}
	u := &Usage{}
	u.Windows = NormalizeUsage(body, now)
	u.ResetCredits, u.ResetCreditExpiry, u.ResetCreditID = NormalizeResetCredits(body, now)
	if pstatus, profile, err := getJSON(oauthProfileURL, headers); err == nil && pstatus == 200 {
		u.Identity, u.Plan = ParseProfile(profile)
	}
	return u, nil
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
		return "", fmt.Errorf("reset: unreadable response: %s", truncate(string(body), 140))
	}
	if res.Reason != "" && res.Result != "reset" {
		return res.Result + " (" + res.Reason + ")", nil
	}
	return res.Result, nil
}

// NormalizeUsage converts the usage payload into windows with the same keys
// aiquota uses: "session", "weekly", "weekly:<model>".
func NormalizeUsage(body []byte, now time.Time) []state.Window {
	var doc struct {
		FiveHour *struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    string   `json:"resets_at"`
		} `json:"five_hour"`
		SevenDay *struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    string   `json:"resets_at"`
		} `json:"seven_day"`
		Limits []struct {
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
		} `json:"limits"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil
	}
	var out []state.Window
	obs := now.Unix()
	if doc.FiveHour != nil && doc.FiveHour.Utilization != nil {
		out = append(out, state.Window{
			Key: "session", Label: "Session", Kind: state.KindShort, UsedPct: *doc.FiveHour.Utilization,
			ResetsAt: parseISO(doc.FiveHour.ResetsAt), WindowSeconds: 5 * 3600, ObservedAt: obs,
		})
	}
	if doc.SevenDay != nil && doc.SevenDay.Utilization != nil {
		out = append(out, state.Window{
			Key: "weekly", Label: "Weekly", Kind: state.KindWeekly, UsedPct: *doc.SevenDay.Utilization,
			ResetsAt: parseISO(doc.SevenDay.ResetsAt), WindowSeconds: 7 * 86400, ObservedAt: obs,
		})
	}
	for _, l := range doc.Limits {
		if l.Kind != "weekly_scoped" || l.Percent == nil {
			continue
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
	return out
}

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
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d %s", resp.StatusCode, truncate(errorDetail(body), 160))
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

func errorDetail(body []byte) string {
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
