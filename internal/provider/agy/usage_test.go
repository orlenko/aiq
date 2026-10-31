package agy

import (
	"errors"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/state"
)

// The structured /usage envelope from agy 1.4.12, with all five buckets.
// response is deliberately unrelated: it is display text, not the API.
const usagePayload = `{"status":"SUCCESS","num_turns":0,"response":"human-readable output","command":{"name":"usage","data":{"groups":[
{"name":"Gemini Models","buckets":[
{"id":"gemini-weekly","remaining_fraction":1,"reset_time":"2026-11-07T15:41:04Z"},
{"id":"gemini-5h","remaining_fraction":0.9,"reset_time":"2026-10-31T20:41:04Z"}]},
{"name":"Claude and GPT models","buckets":[
{"id":"3p-weekly","remaining_fraction":0.71,"reset_time":"2026-11-07T15:41:04Z"},
{"id":"3p-5h","remaining_fraction":1,"reset_time":"2026-10-31T20:41:04Z"}]},
{"name":"Gemini 4 Argon","buckets":[
{"id":"gemini-argon-wtu-weekly","remaining_fraction":0,"reset_time":"2026-11-07T05:50:46Z"}]}]}}}`

func TestParseUsage(t *testing.T) {
	now := time.Date(2026, 10, 31, 15, 41, 4, 0, time.UTC)
	u, err := parseUsage([]byte(usagePayload), now)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]state.Window{}
	for _, w := range u.Windows {
		byKey[w.Key] = w
		if w.ObservedAt != now.Unix() || w.ResetsAt <= now.Unix() {
			t.Fatalf("window timestamps: %+v", w)
		}
	}
	if len(byKey) != 5 {
		t.Fatalf("windows: %+v", byKey)
	}
	if w := byKey["gemini:5h"]; w.Scope != "" || w.Kind != state.KindShort || w.UsedPct < 9.99 || w.UsedPct > 10.01 || w.WindowSeconds != 5*3600 {
		t.Fatalf("Gemini 5h: %+v", w)
	}
	if w := byKey["3p:weekly"]; w.Scope != "3p" || w.UsedPct < 28.99 || w.UsedPct > 29.01 || w.WindowSeconds != 7*86400 {
		t.Fatalf("third-party weekly: %+v", w)
	}
	if w := byKey["gemini:argon-wtu-weekly"]; w.UsedPct != 100 || w.Kind != state.KindWeekly || w.Severity != "critical" || w.ResetsAt != time.Date(2026, 11, 7, 5, 50, 46, 0, time.UTC).Unix() {
		t.Fatalf("Argon weekly: %+v", w)
	}
}

func TestParseUsageRejectsMissingOrInvalidTelemetry(t *testing.T) {
	for name, input := range map[string]string{
		"malformed":        `not JSON`,
		"model response":   `{"status":"SUCCESS","num_turns":1,"response":"Quota probe successful"}`,
		"unexpected turn":  strings.Replace(usagePayload, `"num_turns":0`, `"num_turns":1`, 1),
		"missing buckets":  `{"status":"SUCCESS","command":{"name":"usage","data":{"groups":[]}}}`,
		"missing fraction": strings.Replace(usagePayload, `"remaining_fraction":0,`, ``, 1),
		"invalid fraction": strings.Replace(usagePayload, `"remaining_fraction":0,`, `"remaining_fraction":1.5,`, 1),
		"failed command":   `{"status":"ERROR","error":"backend unavailable"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if u, err := parseUsage([]byte(input), time.Now()); err == nil || u != nil {
				t.Fatalf("accepted invalid telemetry: %+v %v", u, err)
			}
		})
	}
}

func TestPollUsesReadOnlyUsageWithoutStatusline(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	p := &Provider{Command: func(args, env []string) *exec.Cmd {
		want := []string{"-p", "/usage", "--output-format", "json", "--print-timeout", "2s"}
		if !reflect.DeepEqual(args, want) {
			t.Fatalf("argv = %q, want %q", args, want)
		}
		if !contains(env, "HOME="+home) {
			t.Fatal("poll lost the account home")
		}
		cmd := exec.Command("sh", "-c", `printf '%s' "$AIQ_TEST_OUTPUT"; printf 'diagnostic on stderr\n' >&2`)
		cmd.Env = append(env, "AIQ_TEST_OUTPUT="+usagePayload)
		return cmd
	}}
	u, err := p.Poll(home, false, dir, time.Now(), 2*time.Second)
	if err != nil || len(u.Windows) != 5 {
		t.Fatalf("poll: %+v %v", u, err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("poll left files: %v %v", entries, err)
	}
}

func TestPollFailures(t *testing.T) {
	for _, tt := range []struct {
		name, output, exit, want string
	}{
		{"logged out", "Authentication required", "0", ErrNotLoggedIn.Error()},
		{"expired", "Authentication expired. Please log back in.", "3", ErrNotLoggedIn.Error()},
		{"refused", "Individual quota reached. Resets in 2h.", "3", "Individual quota reached"},
		{"network", "network unreachable", "3", "network unreachable"},
		{"invalid JSON", "broken", "0", "decode agy /usage"},
		{"old CLI", `{"status":"SUCCESS","num_turns":1,"response":"success"}`, "0", "update agy"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := &Provider{Command: func(args, env []string) *exec.Cmd {
				cmd := exec.Command("sh", "-c", `printf '%s' "$AIQ_TEST_OUTPUT"; exit "$AIQ_TEST_EXIT"`)
				cmd.Env = append(env, "AIQ_TEST_OUTPUT="+tt.output, "AIQ_TEST_EXIT="+tt.exit)
				return cmd
			}}
			now := time.Now()
			u, err := p.Poll(t.TempDir(), false, t.TempDir(), now, time.Second)
			if u != nil || err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("poll: %+v %v", u, err)
			}
			if tt.name == "refused" {
				var q *QuotaError
				if !errors.As(err, &q) || !q.ResetsAt.Equal(now.Add(2*time.Hour)) {
					t.Fatalf("quota error: %v", err)
				}
			}
		})
	}
}

func TestPollTimeout(t *testing.T) {
	p := &Provider{Command: func(args, env []string) *exec.Cmd {
		return exec.Command("sh", "-c", "sleep 30")
	}}
	start := time.Now()
	_, err := p.Poll(t.TempDir(), false, t.TempDir(), start, 100*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") || time.Since(start) > 5*time.Second {
		t.Fatalf("timeout: %v after %s", err, time.Since(start))
	}
}
