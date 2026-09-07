package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/orlenko/aiq/internal/daemon"
)

// cmdReset consumes one earned Codex rate-limit reset credit.
func cmdReset(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: aiq reset codex/<name>")
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
	if acc.Provider != "codex" {
		return fmt.Errorf("reset credits exist only for Codex accounts")
	}
	p, err := a.codexProvider()
	if err != nil {
		return err
	}
	if err := a.prepareHome(acc); err != nil {
		return err
	}
	var key [16]byte
	rand.Read(key[:])
	outcome, err := p.ConsumeResetCredit(acc.Home, acc.Native, hex.EncodeToString(key[:]))
	if err != nil {
		return err
	}
	now := time.Now()
	a.st.LogEvent("codex", acc.ID, "reset-credit", outcome, now)
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
