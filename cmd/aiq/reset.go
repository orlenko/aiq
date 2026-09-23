package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/orlenko/aiq/internal/daemon"
)

// cmdReset consumes one reset credit: an earned Codex rate-limit reset or a
// Claude reset grant.
func cmdReset(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: aiq reset codex/<name> | claude/<name>")
	}
	a, err := openApp()
	if err != nil {
		return err
	}
	defer a.close()
	acc, err := a.st.GetAccount(args[0])
	if err != nil {
		return err
	}
	switch acc.Provider {
	case "codex":
		if _, err := a.codexProvider(); err != nil {
			return err
		}
		if err := a.prepareHome(acc); err != nil {
			return err
		}
	case "claude":
	default:
		return fmt.Errorf("reset credits exist only for Codex and Claude accounts")
	}
	var key [16]byte
	rand.Read(key[:])
	u, _, _ := a.st.GetUsage(acc.ID)
	outcome, err := a.pool.RedeemResetCredit(acc, u, hex.EncodeToString(key[:]))
	if err != nil {
		return err
	}
	now := time.Now()
	a.st.LogEvent(acc.Provider, acc.ID, "reset-credit", outcome, now)
	switch outcome {
	case "reset", "alreadyRedeemed":
		a.st.MarkReady(acc.ID, now)
		daemon.Notify(a.cfg.Daemon.Listen, acc.ID)
		fmt.Printf("%s: %s — telemetry refresh requested\n", acc.ID, outcome)
	default:
		fmt.Printf("%s: %s\n", acc.ID, outcome)
	}
	return nil
}
