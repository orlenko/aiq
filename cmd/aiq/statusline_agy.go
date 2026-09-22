package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/fsutil"
	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/provider/agy"
	"github.com/orlenko/aiq/internal/state"
)

// cmdAgyStatusline is the Antigravity status-line multiplexer. In a session
// it records the quota buckets for the active pool account, then renders
// either the user's original statusline or aiq's own line. Inside a poll
// (AIQ_AGY_PROBE names a file) it hands the payload to the poller instead.
// It must never fail — the CLI invokes it on every state change.
func cmdAgyStatusline() {
	defer func() { recover() }()
	input, _ := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))

	st, ok := agy.ParseStatus(input)
	if probe := os.Getenv(agy.ProbeEnv); probe != "" {
		if ok {
			fsutil.WriteFileAtomic(probe, input)
		}
		fmt.Println("aiq probe")
		return
	}
	account := os.Getenv("AIQ_ACCOUNT")
	if ok && account != "" && os.Getenv("AIQ_PROVIDER") == "agy" {
		recordAgyUsage(account, st)
	}

	cfg, err := config.Load()
	prev := ""
	if err == nil {
		prev = cfg.Telemetry.AgyStatuslinePrevious
	}
	if prev != "" {
		cmd := exec.Command("sh", "-c", prev)
		cmd.Stdin = strings.NewReader(string(input))
		cmd.Stdout = os.Stdout
		cmd.Stderr = io.Discard
		if cmd.Run() == nil {
			return
		}
	}
	fmt.Println(agy.DefaultStatusLine(account, st, ok))
}

func recordAgyUsage(account string, st agy.Status) {
	store, err := state.Open(paths.StateDB())
	if err != nil {
		return
	}
	defer store.Close()
	id := state.AccountID("agy", account)
	now := time.Now()
	for _, w := range st.Windows(now) {
		w.AccountID, w.Source = id, "statusline"
		store.UpsertWindow(w)
		if w.Scope == "" && w.UsedPct >= 100 && w.ResetsAt > now.Unix() {
			store.MarkExhausted(id, w.Label+" window exhausted", time.Unix(w.ResetsAt, 0), now)
		}
	}
}

// maybeInstallAgyStatusline points the Antigravity CLI's statusLine at the
// multiplexer once an agy account exists, preserving the user's existing
// statusline as the chained renderer. Without it the account cannot be
// polled at all.
func (a *app) maybeInstallAgyStatusline() {
	if !a.cfg.Telemetry.AgyStatusline || a.cfg.Telemetry.AgyStatuslineInstalled {
		return
	}
	if accounts, _ := a.st.ListAccounts("agy"); len(accounts) == 0 {
		return
	}
	geminiDir := paths.RealAgyHome()
	_, installed, err := agy.InstalledStatusline(geminiDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: cannot read agy settings:", err)
		return
	}
	if installed {
		a.cfg.Telemetry.AgyStatuslineInstalled = true
		config.Save(a.cfg)
		return
	}
	self, err := os.Executable()
	if err != nil {
		return
	}
	previous, err := agy.InstallStatusline(geminiDir, self)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: cannot install agy statusline multiplexer:", err)
		return
	}
	a.cfg.Telemetry.AgyStatuslineInstalled = true
	if previous != "" {
		a.cfg.Telemetry.AgyStatuslinePrevious = previous
	}
	if err := config.Save(a.cfg); err != nil {
		fmt.Fprintln(os.Stderr, "warning: cannot save config:", err)
	}
	if previous != "" {
		fmt.Fprintf(os.Stderr, "Installed statusline multiplexer in %s (your statusline %q still renders).\n", agy.SettingsPath(geminiDir), previous)
	} else {
		fmt.Fprintf(os.Stderr, "Installed statusline multiplexer in %s — it records pool usage on every agy status refresh and is how aiq polls agy quota.\n", agy.SettingsPath(geminiDir))
	}
}

// cmdAgyStatuslineAdmin installs or removes the agy multiplexer by hand.
func (a *app) cmdAgyStatuslineAdmin(sub string) error {
	geminiDir := paths.RealAgyHome()
	switch sub {
	case "install":
		a.cfg.Telemetry.AgyStatusline = true
		a.cfg.Telemetry.AgyStatuslineInstalled = false
		if accounts, _ := a.st.ListAccounts("agy"); len(accounts) == 0 {
			return fmt.Errorf("no agy account registered; run: aiq account add agy <name>")
		}
		a.maybeInstallAgyStatusline()
		_, installed, err := agy.InstalledStatusline(geminiDir)
		if err != nil {
			return err
		}
		if !installed {
			return fmt.Errorf("statusLine in %s does not point at aiq after install", agy.SettingsPath(geminiDir))
		}
		fmt.Printf("statusLine in %s now runs `aiq agy-statusline`", agy.SettingsPath(geminiDir))
		if prev := a.cfg.Telemetry.AgyStatuslinePrevious; prev != "" {
			fmt.Printf(" and chains to %q", prev)
		}
		fmt.Println(". Polls and sessions started from now on feed live quota numbers.")
		return nil
	case "uninstall":
		if err := agy.UninstallStatusline(geminiDir, a.cfg.Telemetry.AgyStatuslinePrevious); err != nil {
			return err
		}
		a.cfg.Telemetry.AgyStatuslineInstalled = false
		a.cfg.Telemetry.AgyStatusline = false
		if err := config.Save(a.cfg); err != nil {
			return err
		}
		fmt.Printf("statusLine in %s restored (agy accounts can no longer be polled)\n", agy.SettingsPath(geminiDir))
		return nil
	case "status":
		cmd, installed, err := agy.InstalledStatusline(geminiDir)
		if err != nil {
			return err
		}
		fmt.Printf("statusLine command: %q\ninstalled: %v\nchains to: %q\n", cmd, installed, a.cfg.Telemetry.AgyStatuslinePrevious)
		return nil
	}
	return fmt.Errorf("unknown statusline subcommand %q", sub)
}
