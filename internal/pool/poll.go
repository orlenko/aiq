package pool

import (
	"fmt"
	"os/exec"
	"sync"
	"time"

	"github.com/orlenko/aiq/internal/provider/claude"
	"github.com/orlenko/aiq/internal/provider/codex"
	"github.com/orlenko/aiq/internal/state"
)

// Source tag for windows written by aiq's own poller.
const SourceAIQ = "aiq"

// PollResult is one account's outcome.
type PollResult struct {
	ID  string
	Err error
}

// Poll refreshes telemetry for every enabled account (or only ids) using
// aiq's own pollers: the aiq OAuth grant for Claude, the app-server RPC for
// Codex. Accounts run concurrently; each is bounded by timeout.
func (p *Pool) Poll(codexCmd func(args, env []string) *exec.Cmd, timeout time.Duration, ids ...string) []PollResult {
	accounts, err := p.St.ListAccounts("")
	if err != nil {
		return []PollResult{{Err: err}}
	}
	only := map[string]bool{}
	for _, id := range ids {
		only[id] = true
	}
	var wg sync.WaitGroup
	results := make(chan PollResult, len(accounts))
	for _, a := range accounts {
		if !a.Enabled || (len(only) > 0 && !only[a.ID]) {
			continue
		}
		wg.Add(1)
		go func(a state.Account) {
			defer wg.Done()
			done := make(chan error, 1)
			go func() { done <- p.pollOne(a, codexCmd) }()
			select {
			case err := <-done:
				results <- PollResult{ID: a.ID, Err: err}
			case <-time.After(timeout):
				results <- PollResult{ID: a.ID, Err: fmt.Errorf("poll timed out after %s", timeout)}
			}
		}(a)
	}
	wg.Wait()
	close(results)
	var out []PollResult
	for r := range results {
		out = append(out, r)
	}
	p.clearStaleCooldowns(accounts)
	return out
}

func (p *Pool) pollOne(a state.Account, codexCmd func(args, env []string) *exec.Cmd) error {
	now := time.Now()
	switch a.Provider {
	case "claude":
		path := claude.GrantPath(a.Home)
		if !claude.HasGrant(a.Home) {
			return p.recordError(a, "no poll grant — run: aiq account authorize "+a.ID)
		}
		u, err := claude.Poll(path, now)
		if err != nil {
			return p.recordError(a, err.Error())
		}
		if err := p.St.ReplaceWindows(a.ID, SourceAIQ, u.Windows); err != nil {
			return err
		}
		p.St.PruneWindowSources(a.ID, SourceAIQ, "statusline")
		prev, _, _ := p.St.GetUsage(a.ID)
		p.St.SetUsageMeta(a.ID, firstNonEmpty(u.Plan, prev.Plan), prev.ResetCredits, "", now)
		p.updateIdentity(a, u.Identity)
		return nil
	case "codex":
		if codexCmd == nil {
			return p.recordError(a, "codex binary not available")
		}
		prov := &codex.Provider{Command: codexCmd}
		u, err := prov.Poll(a.Home, a.Native, now)
		if err != nil {
			return p.recordError(a, err.Error())
		}
		if err := p.St.ReplaceWindows(a.ID, SourceAIQ, u.Windows); err != nil {
			return err
		}
		p.St.PruneWindowSources(a.ID, SourceAIQ)
		prev, _, _ := p.St.GetUsage(a.ID)
		p.St.SetUsageMeta(a.ID, firstNonEmpty(u.Plan, prev.Plan), u.ResetCredits, "", now)
		p.updateIdentity(a, u.Identity)
		return nil
	}
	return nil
}

// recordError keeps the last good windows and stores the failure reason.
func (p *Pool) recordError(a state.Account, msg string) error {
	prev, _, _ := p.St.GetUsage(a.ID)
	p.St.SetUsageMeta(a.ID, prev.Plan, prev.ResetCredits, msg, time.Now())
	return fmt.Errorf("%s", msg)
}

func (p *Pool) updateIdentity(a state.Account, identity string) {
	if identity != "" && identity != a.Identity {
		a.Identity = identity
		p.St.UpdateAccount(a)
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
