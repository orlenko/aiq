package pool

import (
	"os"
	"path/filepath"
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
