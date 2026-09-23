package claude

import (
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
	ws := NormalizeUsage(body, time.Unix(1_700_000_000, 0))
	if len(ws) != 3 {
		t.Fatalf("got %d windows: %+v", len(ws), ws)
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
	if _, err := grantFromTokenResponse(nil, []byte(`{"error":"invalid_grant"}`)); err == nil {
		t.Fatal("expected error")
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
