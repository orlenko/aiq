package pool

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"sync"
	"time"

	"github.com/orlenko/aiq/internal/provider/codex"
	"github.com/orlenko/aiq/internal/state"
)

// resetCreditUrgency: a credit expiring within this long is spent on any
// block, even a short window that would lift on its own before then.
const resetCreditUrgency = 24 * time.Hour

// resetCreditRetry spaces out attempts that did not reset anything, so a
// block the backend will not reset is not retried on every poll.
const resetCreditRetry = 30 * time.Minute

var (
	resetTriedMu sync.Mutex
	resetTried   = map[string]time.Time{}
)

// ResetCreditTarget reports the window a reset credit should be spent on
// right now, if any. It spends only on a real block — a binding window at
// its cap that has not rolled over — and only when a credit is worth more
// spent than kept: the block is the weekly window, the credit expires
// before the block lifts, or the credit expires within resetCreditUrgency.
func ResetCreditTarget(u state.Usage, ws []state.Window, scope string, now time.Time) (state.Window, bool) {
	if u.ResetCredits <= 0 {
		return state.Window{}, false
	}
	var block state.Window
	found := false
	for _, w := range ws {
		if !isBinding(w, scope) || (w.ResetsAt > 0 && w.ResetsAt <= now.Unix()) {
			continue
		}
		if w.UsedPct < 100 && w.Severity != "critical" {
			continue
		}
		// The window that lifts last is the one that matters.
		if !found || w.Kind == state.KindWeekly || w.ResetsAt > block.ResetsAt {
			block, found = w, true
		}
	}
	if !found {
		return state.Window{}, false
	}
	exp := u.ResetCreditExpiry
	switch {
	case block.Kind == state.KindWeekly:
	case exp > 0 && block.ResetsAt > 0 && exp < block.ResetsAt:
	case exp > 0 && time.Unix(exp, 0).Sub(now) < resetCreditUrgency:
	default:
		return state.Window{}, false
	}
	return block, true
}

// SpendResetCredits redeems a Codex reset credit on every account that is
// blocked and holds one (see ResetCreditTarget), then re-polls the accounts
// it reset. The idempotency key is derived from the block and the credit, so
// the daemon and a CLI poll racing on the same block redeem only one.
func (p *Pool) SpendResetCredits(codexCmd func(args, env []string) *exec.Cmd) {
	if codexCmd == nil || !p.Cfg.Selection.AutoResetCredits {
		return
	}
	accounts, err := p.St.ListAccounts("codex")
	if err != nil {
		return
	}
	now := time.Now()
	scope := p.ModelScope("codex")
	var reset []string
	for _, a := range accounts {
		if !a.Enabled || !HasCredential(a) {
			continue
		}
		u, ok, _ := p.St.GetUsage(a.ID)
		if !ok {
			continue
		}
		ws, _ := p.St.ListWindows(a.ID)
		block, ok := ResetCreditTarget(u, ws, scope, now)
		if !ok {
			continue
		}
		resetTriedMu.Lock()
		last := resetTried[a.ID]
		resetTriedMu.Unlock()
		if now.Sub(last) < resetCreditRetry {
			continue
		}
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%d|%d", a.ID, u.ResetCreditID, block.Key, block.ResetsAt, u.ResetCredits)))
		key := hex.EncodeToString(sum[:16])
		prov := &codex.Provider{Command: codexCmd}
		outcome, err := prov.ConsumeResetCredit(a.Home, a.Native, u.ResetCreditID, key)
		label := block.Label
		if label == "" {
			label = block.Key
		}
		switch {
		case err != nil:
			resetTriedMu.Lock()
			resetTried[a.ID] = now
			resetTriedMu.Unlock()
			p.St.LogEvent("codex", a.ID, "reset-credit", fmt.Sprintf("auto: %s window blocked, redeem failed: %v", label, err), now)
		case outcome == "reset" || outcome == "alreadyRedeemed":
			p.St.MarkReady(a.ID, now)
			p.St.LogEvent("codex", a.ID, "reset-credit", fmt.Sprintf("auto: %s window blocked, %s (%d credit(s) before)", label, outcome, u.ResetCredits), now)
			reset = append(reset, a.ID)
		default:
			resetTriedMu.Lock()
			resetTried[a.ID] = now
			resetTriedMu.Unlock()
			p.St.LogEvent("codex", a.ID, "reset-credit", fmt.Sprintf("auto: %s window blocked, %s", label, outcome), now)
		}
	}
	for _, id := range reset {
		if a, err := p.St.GetAccount(id); err == nil {
			p.pollOne(a, codexCmd)
		}
	}
}
