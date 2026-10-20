package pool

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/provider/codex"
	"github.com/orlenko/aiq/internal/selector"
	"github.com/orlenko/aiq/internal/state"
)

func TestCandidatesExcludeCodexAuthenticationFailure(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	acc := state.Account{ID: "codex/usky", Provider: "codex", Name: "usky", Enabled: true, Home: home, Priority: 100}
	if err := st.AddAccount(acc); err != nil {
		t.Fatal(err)
	}
	if err := st.SetUsageMeta(acc.ID, "team", 0, codex.ErrAuthRequired.Error()+": token_revoked", time.Now()); err != nil {
		t.Fatal(err)
	}
	p := &Pool{Cfg: config.Default(), St: st}
	cands, err := p.Candidates("codex")
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].UnavailableReason != "authentication required" {
		t.Fatalf("candidate did not preserve auth failure: %+v", cands)
	}
	ranked := selector.Rank(p.Policy("codex", state.ModeInteractive, time.Now()), cands)
	if len(ranked) != 1 || ranked[0].Eligible || ranked[0].Reason != "authentication required" {
		t.Fatalf("authentication failure remained routable: %+v", ranked)
	}
}

func TestCandidatesExcludeClaudeCLILoginProblems(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	add := func(name, identity string) state.Account {
		home := t.TempDir()
		if err := os.WriteFile(filepath.Join(home, ".credentials.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		acc := state.Account{ID: "claude/" + name, Provider: "claude", Name: name, Enabled: true, Home: home, Identity: identity, Priority: 100}
		if err := st.AddAccount(acc); err != nil {
			t.Fatal(err)
		}
		return acc
	}
	out := add("out", "out@example.com")
	st.SetCLILogin(out.ID, false, "", now)
	wrong := add("wrong", "wrong@example.com")
	st.SetCLILogin(wrong.ID, true, "Other@Example.com", now)
	good := add("good", "good@example.com")
	st.SetCLILogin(good.ID, true, "GOOD@example.com", now)
	unasked := add("unasked", "unasked@example.com")

	p := &Pool{Cfg: config.Default(), St: st}
	cands, err := p.Candidates("claude")
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string]string{}
	for _, c := range cands {
		reasons[c.ID] = c.UnavailableReason
	}
	if want := "CLI not logged in — run: aiq account login claude/out"; reasons[out.ID] != want {
		t.Errorf("logged out: reason %q, want %q", reasons[out.ID], want)
	}
	if want := "CLI logged in as Other@Example.com, not wrong@example.com — run: aiq account login claude/wrong"; reasons[wrong.ID] != want {
		t.Errorf("mismatch: reason %q, want %q", reasons[wrong.ID], want)
	}
	if reasons[good.ID] != "" || reasons[unasked.ID] != "" {
		t.Errorf("routable accounts held: good %q, unasked %q", reasons[good.ID], reasons[unasked.ID])
	}

	v, err := p.View(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, av := range v.Accounts {
		switch av.ID {
		case out.ID:
			if av.CLILogin != state.LoginOut || av.Eligible {
				t.Errorf("view of logged-out account: %+v", av)
			}
		case wrong.ID:
			if !av.CLIMismatch || av.CLIIdentity != "Other@Example.com" || av.Eligible {
				t.Errorf("view of mismatched account: %+v", av)
			}
		case good.ID:
			if av.CLIMismatch || av.CLILogin != state.LoginIn {
				t.Errorf("view of good account: %+v", av)
			}
		}
	}
}

func TestProbeClaudeLoginRecordsAndLogsOnce(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("the keychain answers first on macOS")
	}
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	home := t.TempDir()
	acc := state.Account{ID: "claude/fourth", Provider: "claude", Name: "fourth", Enabled: true, Home: home, Identity: "c4@example.com", Priority: 100}
	if err := st.AddAccount(acc); err != nil {
		t.Fatal(err)
	}
	write := func(s string) {
		if err := os.WriteFile(filepath.Join(home, ".credentials.json"), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p := &Pool{Cfg: config.Default(), St: st}
	now := time.Now()

	// What the CLI leaves after a refused refresh.
	write(`{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0}}`)
	p.ProbeClaudeLogin(acc, now)
	p.ProbeClaudeLogin(acc, now)
	u, _, _ := st.GetUsage(acc.ID)
	if u.CLILogin != state.LoginOut || u.CLICheckedAt != now.Unix() {
		t.Fatalf("logged-out probe recorded as %+v", u)
	}
	events, _ := st.ListEvents(10)
	logged := 0
	for _, e := range events {
		if e.Type == "login" {
			logged++
		}
	}
	if logged != 1 {
		t.Fatalf("want one login event for a problem seen twice, got %d: %+v", logged, events)
	}

	// Logged in again, with an access token too old to read the email with:
	// the last verified email stays.
	st.SetCLILogin(acc.ID, true, "c4@example.com", now)
	write(fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"at","refreshToken":"rt","expiresAt":%d}}`, now.Add(-time.Hour).UnixMilli()))
	if err := p.ProbeClaudeLogin(acc, now); err != nil {
		t.Fatal(err)
	}
	if u, _, _ := st.GetUsage(acc.ID); u.CLILogin != state.LoginIn || u.CLIIdentity != "c4@example.com" || CLILoginProblem(acc, u) != "" {
		t.Fatalf("lapsed but valid login recorded as %+v", u)
	}
}
