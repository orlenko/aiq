package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/provider/claude"
	"github.com/orlenko/aiq/internal/state"
)

// cmdClaudeStatusline is the status-line multiplexer: it records rate-limit
// telemetry for the active pool account, then renders either the user's
// original statusline or a default one. It must never fail — Claude Code
// invokes it on every status refresh.
func cmdClaudeStatusline() {
	defer func() { recover() }()
	input, _ := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))

	rl, ok := claude.ParseRateLimits(input)
	account := os.Getenv("AIQ_ACCOUNT")

	if ok && account != "" && os.Getenv("AIQ_PROVIDER") == "claude" {
		recordClaudeUsage(account, rl)
	}

	cfg, err := config.Load()
	prev := ""
	if err == nil {
		prev = cfg.Telemetry.ClaudeStatuslinePrevious
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
	fmt.Println(claude.DefaultStatusLine(account, rl, ok))
}

func recordClaudeUsage(account string, rl claude.RateLimits) {
	st, err := state.Open(paths.StateDB())
	if err != nil {
		return
	}
	defer st.Close()
	id := state.AccountID("claude", account)
	now := time.Now()
	if rl.FiveHourPct >= 0 {
		st.UpsertWindow(state.Window{
			AccountID: id, Key: "session", Label: "Session", Kind: state.KindShort,
			UsedPct: rl.FiveHourPct, ResetsAt: rl.FiveHourReset, WindowSeconds: 5 * 3600,
			Source: "statusline", ObservedAt: now.Unix(),
		})
	}
	if rl.SevenDayPct >= 0 {
		st.UpsertWindow(state.Window{
			AccountID: id, Key: "weekly", Label: "Weekly", Kind: state.KindWeekly,
			UsedPct: rl.SevenDayPct, ResetsAt: rl.SevenDayReset, WindowSeconds: 7 * 86400,
			Source: "statusline", ObservedAt: now.Unix(),
		})
	}
	if rl.FiveHourPct >= 100 && rl.FiveHourReset > now.Unix() {
		st.MarkExhausted(id, "5h window exhausted", time.Unix(rl.FiveHourReset, 0), now)
	}
	if rl.SevenDayPct >= 100 && rl.SevenDayReset > now.Unix() {
		st.MarkExhausted(id, "weekly window exhausted", time.Unix(rl.SevenDayReset, 0), now)
	}
}

// cmdStatusline installs or removes the Claude status-line multiplexer by hand.
func cmdStatusline(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: aiq statusline install|uninstall|status")
	}
	a, err := openApp()
	if err != nil {
		return err
	}
	defer a.close()
	configDir := paths.RealClaudeHome()
	switch args[0] {
	case "install":
		a.cfg.Telemetry.ClaudeStatusline = true
		a.cfg.Telemetry.ClaudeStatuslineInstalled = false
		a.maybeInstallStatusline()
		_, installed, err := claude.InstalledStatusline(configDir)
		if err != nil {
			return err
		}
		if !installed {
			return fmt.Errorf("statusLine in %s does not point at aiq after install", claude.SettingsPath(configDir))
		}
		fmt.Printf("statusLine in %s now runs `aiq claude-statusline`", claude.SettingsPath(configDir))
		if prev := a.cfg.Telemetry.ClaudeStatuslinePrevious; prev != "" {
			fmt.Printf(" and chains to %q", prev)
		}
		fmt.Println(". Sessions started from now on through the shim feed live quota numbers.")
		return nil
	case "uninstall":
		if err := claude.UninstallStatusline(configDir, a.cfg.Telemetry.ClaudeStatuslinePrevious); err != nil {
			return err
		}
		a.cfg.Telemetry.ClaudeStatuslineInstalled = false
		a.cfg.Telemetry.ClaudeStatusline = false
		if err := config.Save(a.cfg); err != nil {
			return err
		}
		fmt.Printf("statusLine in %s restored\n", claude.SettingsPath(configDir))
		return nil
	case "status":
		cmd, installed, err := claude.InstalledStatusline(configDir)
		if err != nil {
			return err
		}
		fmt.Printf("statusLine command: %q\ninstalled: %v\nchains to: %q\n", cmd, installed, a.cfg.Telemetry.ClaudeStatuslinePrevious)
		return nil
	}
	return fmt.Errorf("unknown statusline subcommand %q", args[0])
}

// maybeInstallStatusline points Claude Code's statusLine at the multiplexer,
// preserving the user's existing statusline as the chained renderer. The
// settings file is shared by every overlay home, so one install covers all.
func (a *app) maybeInstallStatusline() {
	if !a.cfg.Telemetry.ClaudeStatusline || a.cfg.Telemetry.ClaudeStatuslineInstalled {
		return
	}
	configDir := paths.RealClaudeHome()
	_, installed, err := claude.InstalledStatusline(configDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: cannot read Claude settings:", err)
		return
	}
	if installed {
		a.cfg.Telemetry.ClaudeStatuslineInstalled = true
		config.Save(a.cfg)
		return
	}
	self, err := os.Executable()
	if err != nil {
		return
	}
	previous, err := claude.InstallStatusline(configDir, self)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: cannot install statusline multiplexer:", err)
		return
	}
	a.cfg.Telemetry.ClaudeStatuslineInstalled = true
	if previous != "" {
		a.cfg.Telemetry.ClaudeStatuslinePrevious = previous
	}
	if err := config.Save(a.cfg); err != nil {
		fmt.Fprintln(os.Stderr, "warning: cannot save config:", err)
	}
	if previous != "" {
		fmt.Fprintf(os.Stderr, "Installed statusline multiplexer in %s (your statusline %q still renders).\n",
			claude.SettingsPath(configDir), previous)
	} else {
		fmt.Fprintf(os.Stderr, "Installed statusline multiplexer in %s — it records pool usage on every Claude status refresh.\n",
			claude.SettingsPath(configDir))
	}
}
