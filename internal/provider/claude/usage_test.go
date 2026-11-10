package claude

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/state"
)

func TestNormalizeUsage(t *testing.T) {
	body := []byte(`{
	  "five_hour": {"utilization": 43, "resets_at": "2026-09-06T20:00:00.256879+00:00"},
	  "seven_day": {"utilization": 8, "resets_at": "2026-09-13T15:00:00.256904+00:00"},
	  "limits": [
	    {"kind": "weekly_scoped", "percent": 2, "resets_at": "2026-09-13T15:00:00Z", "severity": "normal",
	     "scope": {"model": {"display_name": "Fable"}}},
	    {"kind": "other", "percent": 50}
	  ],
	  "spend": {"percent": 0, "enabled": false}
	}`)
	ws, err := NormalizeUsage(body, time.Unix(1_700_000_000, 0))
	if err != nil || len(ws) != 3 {
		t.Fatalf("got %d windows: %+v %v", len(ws), ws, err)
	}
	if ws[0].Key != "session" || ws[0].Kind != state.KindShort || ws[0].UsedPct != 43 || ws[0].ResetsAt == 0 {
		t.Fatalf("session: %+v", ws[0])
	}
	if ws[1].Key != "weekly" || ws[1].Kind != state.KindWeekly || ws[1].UsedPct != 8 {
		t.Fatalf("weekly: %+v", ws[1])
	}
	if ws[2].Key != "weekly:fable" || ws[2].Scope != "Fable" || ws[2].Severity != "" || ws[2].UsedPct != 2 {
		t.Fatalf("scoped: %+v", ws[2])
	}
	want := time.Date(2026, 9, 6, 20, 0, 0, 0, time.UTC).Unix()
	if ws[0].ResetsAt != want {
		t.Fatalf("resets_at %d want %d", ws[0].ResetsAt, want)
	}
}

func TestParseProfile(t *testing.T) {
	email, plan := ParseProfile([]byte(`{"account":{"email_address":"a@example.com"},"organization":{"rate_limit_tier":"default_claude_max_20x"}}`))
	if email != "a@example.com" || plan != "max 20x" {
		t.Fatalf("%q %q", email, plan)
	}
	email, plan = ParseProfile([]byte(`{"organization":{"organization_type":"claude_pro"}}`))
	if email != "" || plan != "pro" {
		t.Fatalf("%q %q", email, plan)
	}
}

func TestGrantFromTokenResponse(t *testing.T) {
	prev := &Grant{}
	prev.OAuth.RefreshToken = "old"
	g, err := grantFromTokenResponse(prev, []byte(`{"access_token":"new-a","expires_in":3600}`))
	if err != nil {
		t.Fatal(err)
	}
	if g.OAuth.AccessToken != "new-a" || g.OAuth.RefreshToken != "old" || g.OAuth.ExpiresAt == 0 {
		t.Fatalf("%+v", g.OAuth)
	}
	_, err = grantFromTokenResponse(nil, []byte(`{"error":"invalid_grant"}`))
	if err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("the provider's code was lost: %v", err)
	}
	for _, body := range tokenBodiesNotToQuote {
		_, err := grantFromTokenResponse(nil, []byte(body))
		if err == nil || strings.Contains(err.Error(), "sentinel") {
			t.Errorf("%s gave %v", body, err)
		}
	}
}

var tokenBodiesNotToQuote = []string{
	`{"note":"sentinel"}`,
	`{"error":"sentinel text"}`,
	`{"error":"sentinel-SENTINEL"}`,
	`{"error":"` + strings.Repeat("sentinel", 9) + `"}`,
	`{"error":{"note":"sentinel"}}`,
	`{"error_description":"sentinel"}`,
	`{"detail":"sentinel"}`,
	`{"message":"sentinel"}`,
	`{"error":{"message":"sentinel"}}`,
	`sentinel`,
}

func TestTokenEndpointErrorsQuoteOnlyTheCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var c struct {
			Status int
			Body   string
		}
		json.NewDecoder(r.Body).Decode(&c)
		w.WriteHeader(c.Status)
		io.WriteString(w, c.Body)
	}))
	defer srv.Close()
	post := func(status int, body string) error {
		_, err := postJSON(srv.URL, map[string]any{"status": status, "body": body})
		return err
	}

	err := post(400, `{"error":"invalid_grant","error_description":"sentinel"}`)
	if err == nil || err.Error() != "HTTP 400 invalid_grant" {
		t.Errorf("got %v", err)
	}
	for _, status := range []int{201, 400, 500} {
		for _, body := range tokenBodiesNotToQuote {
			if err := post(status, body); err == nil || strings.Contains(err.Error(), "sentinel") {
				t.Errorf("%d %s gave %v", status, body, err)
			}
		}
	}
}

func TestTokenEndpointFollowsNoRedirect(t *testing.T) {
	reached := make(chan struct{}, 1)
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached <- struct{}{} }))
	defer target.Close()
	srv := httptest.NewServer(http.RedirectHandler(target.URL+"/sentinel", http.StatusTemporaryRedirect))
	_, err := postJSON(srv.URL, map[string]any{})
	if err == nil || err.Error() != "HTTP 307" {
		t.Errorf("got %v", err)
	}
	select {
	case <-reached:
		t.Error("the body went on to the new address")
	default:
	}
	srv.Close()
	_, err = postJSON(srv.URL, map[string]any{})
	if err == nil || strings.Contains(err.Error(), srv.URL) {
		t.Errorf("a failed connection gave %v", err)
	}
}

func TestErrorDetailIsRedacted(t *testing.T) {
	const secret = "abcdefghijklmnopqrstuvwxyz0123456789"
	got := errorDetail([]byte(`{"message":"sent Authorization: Bearer ` + secret + `"}`))
	if strings.Contains(got, secret) || !strings.Contains(got, "sent") {
		t.Errorf("got %q", got)
	}
}

func TestNormalizeResetCredits(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	body := []byte(`{"cedar_ember": {"eligible": true, "grants": [
	    {"id": "later", "resets_left": 2, "ends_at": "2026-11-01T00:00:00+00:00"},
	    {"id": "promo", "resets_left": 1, "ends_at": "2026-10-22T16:00:00+00:00"},
	    {"id": "spent", "resets_left": 0, "ends_at": "2026-10-01T00:00:00+00:00"},
	    {"id": "paused", "resets_left": 1, "ends_at": "2026-10-01T00:00:00+00:00", "paused": true},
	    {"id": "lapsed", "resets_left": 1, "ends_at": "2026-09-01T00:00:00+00:00"}
	  ], "next_grant_id": null}}`)
	credits, expiry, id := NormalizeResetCredits(body, now)
	if credits != 3 || id != "promo" || expiry != time.Date(2026, 10, 22, 16, 0, 0, 0, time.UTC).Unix() {
		t.Fatalf("got %d credits, expiry %d, id %q", credits, expiry, id)
	}
	// The backend's pick wins over the soonest expiry.
	withNext := []byte(strings.Replace(string(body), `"next_grant_id": null`, `"next_grant_id": "later"`, 1))
	if _, _, id := NormalizeResetCredits(withNext, now); id != "later" {
		t.Fatalf("next_grant_id ignored: %q", id)
	}
	// Ineligible (a client the backend does not offer resets to) or absent.
	for _, b := range []string{
		`{"cedar_ember": {"eligible": false, "ineligible_reason": "surface", "grants": []}}`,
		`{"cedar_ember": null}`,
		`{}`,
	} {
		if c, _, _ := NormalizeResetCredits([]byte(b), now); c != 0 {
			t.Fatalf("%s: got %d credits", b, c)
		}
	}
}

func TestParseOrganization(t *testing.T) {
	if got := ParseOrganization([]byte(`{"account": {"email": "a@b.c"}, "organization": {"uuid": "1593dbce-9dd4"}}`)); got != "1593dbce-9dd4" {
		t.Fatalf("got %q", got)
	}
}

// Shapes from the live endpoints on 2026-10-10: extra usage on with a
// monthly limit, and a prepaid balance with auto-reload off.
func TestParseExtraUsageAndPrepaid(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	usage := []byte(`{"extra_usage":{"is_enabled":true,"monthly_limit":70000,"used_credits":1234.0,"utilization":null,
		"currency":"CAD","decimal_places":2,"disabled_reason":null,"user_disabled":false,"spend_limit_reached":false}}`)
	e := ParseExtraUsage(usage, now)
	if e == nil || !e.Enabled || e.LimitMinor != 70000 || e.UsedMinor != 1234 || e.Currency != "CAD" || e.BalanceMinor != -1 {
		t.Fatalf("extra usage: %+v", e)
	}
	if ok, why := e.Spendable(now); ok || why != "prepaid balance unknown" {
		t.Fatalf("before the balance is read: %v %q", ok, why)
	}
	ParsePrepaid([]byte(`{"amount":39947,"currency":"CAD","balance":{"money":{"amount_minor":39947,"currency":"CAD","exponent":2},"credits":null},
		"auto_reload_settings":null}`), e, now)
	if e.BalanceMinor != 39947 || e.AutoReload || e.BalanceAt != now.Unix() {
		t.Fatalf("prepaid: %+v", e)
	}
	if ok, why := e.Spendable(now); !ok {
		t.Fatalf("should be spendable: %s", why)
	}
	if got := e.Money(e.BalanceMinor); got != "CAD 399.47" {
		t.Fatalf("money: %s", got)
	}
	if ok, why := e.Spendable(now.Add(state.ExtraUsageMaxAge + time.Minute)); ok || why != "extra usage reading is stale" {
		t.Fatalf("stale reading: %v %q", ok, why)
	}

	// Only an explicit false reads as auto-reload off; any other object,
	// whatever its keys, reads as on.
	for body, on := range map[string]bool{
		`{"amount":39947,"auto_reload_settings":{"enabled":true}}`:                       true,
		`{"amount":39947,"auto_reload_settings":{"is_enabled":true}}`:                    true,
		`{"amount":39947,"auto_reload_settings":{"threshold_cents":1000,"amount":5000}}`: true,
		`{"amount":39947,"auto_reload_settings":{"enabled":false}}`:                      false,
		`{"amount":39947,"auto_reload_settings":null}`:                                   false,
	} {
		ParsePrepaid([]byte(body), e, now)
		if e.AutoReload != on {
			t.Fatalf("%s: auto-reload %v, want %v", body, e.AutoReload, on)
		}
		if ok, _ := e.Spendable(now); ok == on {
			t.Fatalf("%s: spendable %v with auto-reload %v", body, ok, on)
		}
	}
	// A payload without a balance changes nothing.
	before := *e
	ParsePrepaid([]byte(`{"error":"nope","auto_reload_settings":{"enabled":true}}`), e, now.Add(time.Minute))
	if *e != before {
		t.Fatalf("balance-less payload changed the reading: %+v", e)
	}
	ParsePrepaid([]byte(`{"amount":0,"auto_reload_settings":null}`), e, now)
	if ok, why := e.Spendable(now); ok || why != "out of prepaid credits" {
		t.Fatalf("empty balance: %v %q", ok, why)
	}

	off := ParseExtraUsage([]byte(`{"extra_usage":{"is_enabled":false,"monthly_limit":null,"used_credits":null}}`), now)
	if ok, _ := off.Spendable(now); ok {
		t.Fatal("disabled extra usage must not be spendable")
	}
	capped := ParseExtraUsage([]byte(`{"extra_usage":{"is_enabled":true,"monthly_limit":70000,"used_credits":70000}}`), now)
	capped.BalanceMinor, capped.BalanceAt = 100, now.Unix()
	if ok, why := capped.Spendable(now); ok || why != "monthly spend limit reached" {
		t.Fatalf("capped: %v %q", ok, why)
	}
	if ParseExtraUsage([]byte(`{"extra_usage":null}`), now) != nil {
		t.Fatal("a null block (skip_spend) reads as unknown")
	}
}

// Only the usage document is a poll, and only a null field is no window: a
// field in a changed form is an error, so a format change cannot pass for
// "nothing running" and wipe the windows.
func TestNormalizeUsageRefusesAChangedFormat(t *testing.T) {
	now := time.Now()
	for body, want := range map[string]int{
		`{"five_hour":null,"seven_day":null,"extra_usage":{"is_enabled":false}}`: 0,
		`{"seven_day":{"utilization":3}}`:                                        1,
		`{"limits":[]}`:                                                          0,
		`{"limits":null,"five_hour":null}`:                                       0,
		`{"five_hour":null,"limits":[{"kind":"other"}]}`:                         0,
	} {
		if ws, err := NormalizeUsage([]byte(body), now); err != nil || len(ws) != want {
			t.Errorf("%s: %d windows, %v; want %d", body, len(ws), err, want)
		}
	}
	for _, body := range []string{
		`{}`, `{"error":"x"}`, `<html>sign in</html>`, `[]`, `null`, ``,
		`{"five_hour":{"utilization":40,"resets_at":1893456000},"seven_day":{"utilization":20}}`,
		`{"five_hour":{"utilization":"40"}}`,
		`{"seven_day":{"used":100}}`,
		`{"seven_day":{"utilization":null}}`,
		`{"five_hour":"x"}`,
		`{"seven_day":{"utilization":20},"limits":{"weekly_scoped":[]}}`,
		`{"seven_day":{"utilization":20},"limits":[{"kind":"weekly_scoped","percent":"5"}]}`,
		`{"seven_day":{"utilization":20},"limits":[{"kind":"weekly_scoped"}]}`,
	} {
		if ws, err := NormalizeUsage([]byte(body), now); err == nil {
			t.Errorf("%s: read as %d windows, want an error", body, len(ws))
		}
	}
}

func TestUsageFromReply(t *testing.T) {
	now := time.Now()
	if _, err := usageFromReply(200, []byte(`<html>sign in</html>`), now); err == nil || !strings.Contains(err.Error(), "not usage data") {
		t.Errorf("a page that is not usage data: %v", err)
	}
	if _, err := usageFromReply(200, []byte(`{"seven_day":{"utilization":100,"resets_at":1893456000}}`), now); err == nil || !strings.Contains(err.Error(), "seven_day") {
		t.Errorf("a changed format: %v", err)
	}
	if _, err := usageFromReply(401, nil, now); err == nil || !strings.Contains(err.Error(), "authorize") {
		t.Errorf("401: %v", err)
	}
	if _, err := usageFromReply(500, []byte(`{}`), now); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("500: %v", err)
	}
	u, err := usageFromReply(200, []byte(`{"five_hour":null,"seven_day":{"utilization":4,"resets_at":""}}`), now)
	if err != nil || len(u.Windows) != 1 || u.Windows[0].Key != "weekly" {
		t.Errorf("a usage reply: %+v %v", u, err)
	}
	if u, err := usageFromReply(200, []byte(`{"five_hour":null,"seven_day":null}`), now); err != nil || len(u.Windows) != 0 {
		t.Errorf("a reply with no window running: %+v %v", u, err)
	}
}
