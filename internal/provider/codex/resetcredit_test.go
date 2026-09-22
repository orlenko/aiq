package codex

import (
	"encoding/json"
	"testing"
	"time"
)

func TestParseResetCreditExpiry(t *testing.T) {
	raw := json.RawMessage(`{
	  "rateLimits": {"primary": {"usedPercent": 10, "windowDurationMins": 10080, "resetsAt": 1790000000}},
	  "rateLimitResetCredits": {"availableCount": 3, "credits": [
	    {"id": "late", "status": "available", "grantedAt": 1, "resetType": "codexRateLimits", "expiresAt": 1790500000},
	    {"id": "soon", "status": "available", "grantedAt": 1, "resetType": "codexRateLimits", "expiresAt": 1790100000},
	    {"id": "used", "status": "redeemed", "grantedAt": 1, "resetType": "codexRateLimits", "expiresAt": 1780000000},
	    {"id": "never", "status": "available", "grantedAt": 1, "resetType": "codexRateLimits", "expiresAt": null}
	  ]}
	}`)
	rl, ok := ParseRateLimitsResult(raw, time.Unix(1789000000, 0))
	if !ok {
		t.Fatal("no windows parsed")
	}
	if rl.ResetCredits != 3 || rl.ResetCreditExpiry != 1790100000 || rl.ResetCreditID != "soon" {
		t.Fatalf("got credits=%d expiry=%d id=%q", rl.ResetCredits, rl.ResetCreditExpiry, rl.ResetCreditID)
	}
}
