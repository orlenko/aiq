package agy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/orlenko/aiq/internal/state"
)

// Usage is what one poll of an account yields.
type Usage struct {
	Windows  []state.Window
	Identity string
	Plan     string
}

// ErrNotLoggedIn is the poll's failure when the CLI has no login and would
// wait for a browser sign-in.
var ErrNotLoggedIn = errors.New("agy is not logged in — run: aiq account login")

// QuotaError is the poll's failure when the CLI refuses the run because the
// account is out of quota. Its status line then never reports the buckets,
// so the refusal is the only telemetry: it says the account is blocked and,
// usually, for how long.
type QuotaError struct {
	Message  string
	ResetsAt time.Time // zero when the CLI gave no reset
}

func (e *QuotaError) Error() string {
	if e.ResetsAt.IsZero() {
		return "agy: " + e.Message
	}
	return fmt.Sprintf("agy: %s (resets %s)", e.Message, e.ResetsAt.Local().Format("Mon 15:04"))
}

// quotaRefusal matches the CLI's refusal, as in its JSON result:
// "error":"Individual quota reached. Please upgrade your subscription to
// increase your limits. Resets in 113h17m1s."
var (
	quotaRefusal = regexp.MustCompile(`(?i)\b(\w+ )?quota reached`)
	quotaReset   = regexp.MustCompile(`(?i)resets in ((?:\d+(?:\.\d+)?[hms])+)`)
)

// ParseQuotaRefusal reads a quota refusal from the CLI's output. ok is false
// when the output is not one.
func ParseQuotaRefusal(out string, now time.Time) (*QuotaError, bool) {
	m := quotaRefusal.FindString(out)
	if m == "" {
		return nil, false
	}
	e := &QuotaError{Message: m}
	if r := quotaReset.FindStringSubmatch(out); r != nil {
		if d, err := time.ParseDuration(r[1]); err == nil {
			e.ResetsAt = now.Add(d)
		}
	}
	return e, true
}

// Poll reads quota through the CLI's read-only /usage command (agy 1.1.11+).
// It runs no model turn and needs no statusline. Since agy 1.4.7, print mode
// no longer invokes statusline commands at all.
func (p *Provider) Poll(home string, native bool, dir string, now time.Time, timeout time.Duration) (*Usage, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	cmd := p.Command([]string{"-p", "/usage", "--output-format", "json", "--print-timeout", timeout.String()}, p.Env(home, native, false))
	cmd.Dir = dir
	var output, diagnostics lockedBuffer
	cmd.Stdout, cmd.Stderr = &output, &diagnostics
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start agy: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	stop := func() {
		// TERM the whole group: the CLI starts a language server of its own.
		syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
	}
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			out := strings.TrimSpace(output.String() + "\n" + diagnostics.String())
			if q, ok := ParseQuotaRefusal(out, now); ok {
				return nil, q
			}
			if needsLogin(out) {
				return nil, ErrNotLoggedIn
			}
			if err != nil {
				return nil, fmt.Errorf("agy /usage: %v: %s", err, shortDiagnostic(out))
			}
			return parseUsage([]byte(output.String()), now)
		case <-tick.C:
			// Logged out, the CLI prints a sign-in URL and waits a minute for
			// the browser; there is nothing to wait for.
			if needsLogin(output.String() + diagnostics.String()) {
				stop()
				return nil, ErrNotLoggedIn
			}
			if time.Now().After(deadline) {
				stop()
				return nil, fmt.Errorf("agy /usage timed out after %s", timeout)
			}
		}
	}
}

func needsLogin(out string) bool {
	out = strings.ToLower(out)
	return strings.Contains(out, "authentication required") || strings.Contains(out, "authentication failed") || strings.Contains(out, "authentication expired")
}

func shortDiagnostic(out string) string {
	// Keep a failed poll readable on the quota screen, even if the CLI emits
	// a long JSON envelope. Successful output is never used as an error.
	out = strings.Join(strings.Fields(out), " ")
	runes := []rune(out)
	if len(runes) > 300 {
		out = "…" + string(runes[len(runes)-300:])
	}
	return out
}

// parseUsage reads command.data rather than the human-readable response.
// Reuse the statusline bucket mapping so scopes and window keys stay stable.
func parseUsage(input []byte, now time.Time) (*Usage, error) {
	var doc struct {
		Status   string `json:"status"`
		Error    string `json:"error"`
		NumTurns int    `json:"num_turns"`
		Command  struct {
			Name string `json:"name"`
			Data struct {
				Groups []struct {
					Buckets []struct {
						ID        string   `json:"id"`
						Remaining *float64 `json:"remaining_fraction"`
						ResetTime string   `json:"reset_time"`
					} `json:"buckets"`
				} `json:"groups"`
			} `json:"data"`
		} `json:"command"`
	}
	if err := json.Unmarshal(input, &doc); err != nil {
		return nil, fmt.Errorf("decode agy /usage: %w", err)
	}
	if doc.Status != "SUCCESS" {
		return nil, fmt.Errorf("agy /usage failed: %s", shortDiagnostic(doc.Error))
	}
	if doc.Command.Name != "usage" || doc.NumTurns != 0 {
		return nil, errors.New("agy did not return read-only /usage data — update agy to 1.1.11 or newer")
	}
	s := Status{Quota: make(map[string]Bucket)}
	for _, group := range doc.Command.Data.Groups {
		for _, b := range group.Buckets {
			if b.ID == "" || b.Remaining == nil || *b.Remaining < 0 || *b.Remaining > 1 {
				return nil, errors.New("agy /usage returned an invalid quota bucket")
			}
			s.Quota[b.ID] = Bucket{RemainingFraction: *b.Remaining, ResetTime: b.ResetTime}
		}
	}
	if len(s.Quota) == 0 {
		return nil, errors.New("agy /usage returned no quota buckets")
	}
	// /usage carries no identity or plan. Callers retain their last values;
	// interactive statusline telemetry can still supply them.
	return &Usage{Windows: s.Windows(now)}, nil
}

// lockedBuffer is a bytes.Buffer the poll may read while the CLI writes.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// ProbeDir is where polls run: a directory of aiq's own, so the CLI never
// records a real project as the probe's workspace.
func ProbeDir(dataDir string) string { return filepath.Join(dataDir, "agy-probe") }
