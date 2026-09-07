package claude

import (
	"testing"
	"time"
)

func TestParseRateLimits(t *testing.T) {
	input := []byte(`{
		"session_id": "abc",
		"model": {"id": "claude-opus-5"},
		"rate_limits": {
			"five_hour": {"used_percentage": 42, "resets_at": 1234567890},
			"seven_day": {"used_percentage": 63.5, "resets_at": 1234567890}
		}
	}`)
	rl, ok := ParseRateLimits(input)
	if !ok {
		t.Fatal("expected ok")
	}
	if rl.FiveHourPct != 42 || rl.SevenDayPct != 63.5 {
		t.Fatalf("got %+v", rl)
	}
	if rl.FiveHourReset != 1234567890 {
		t.Fatalf("reset: %d", rl.FiveHourReset)
	}
}

func TestParseRateLimitsRFC3339(t *testing.T) {
	input := []byte(`{"rate_limits":{"five_hour":{"used_percentage":10,"resets_at":"2026-08-10T14:42:00Z"}}}`)
	rl, ok := ParseRateLimits(input)
	if !ok {
		t.Fatal("expected ok")
	}
	want := time.Date(2026, 8, 10, 14, 42, 0, 0, time.UTC).Unix()
	if rl.FiveHourReset != want {
		t.Fatalf("got %d want %d", rl.FiveHourReset, want)
	}
	if rl.SevenDayPct != -1 {
		t.Fatalf("seven_day should be unknown, got %f", rl.SevenDayPct)
	}
}

func TestParseRateLimitsAbsent(t *testing.T) {
	for _, input := range []string{
		`{}`,
		`{"session_id":"x"}`,
		`{"rate_limits":{}}`,
		`{"rate_limits":{"five_hour":{}}}`,
		`not json`,
		``,
	} {
		if _, ok := ParseRateLimits([]byte(input)); ok {
			t.Fatalf("expected !ok for %q", input)
		}
	}
}
