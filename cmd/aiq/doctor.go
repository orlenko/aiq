package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/daemon"
	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/provider/claude"
)

func cmdDoctor(args []string) error {
	a, err := openApp()
	if err != nil {
		return err
	}
	defer a.close()
	problems := 0
	check := func(ok bool, format string, v ...any) {
		mark := "ok  "
		if !ok {
			mark = "FAIL"
			problems++
		}
		fmt.Printf("%s %s\n", mark, fmt.Sprintf(format, v...))
	}

	for _, provider := range pool.Providers {
		bin, err := a.binary(provider)
		check(err == nil, "%s binary: %s", provider, orErr(bin, err))
	}

	// Launchers: the command has to exist, and its shim has to be there for
	// the name to work in a shell.
	names := make([]string, 0, len(a.cfg.Launchers))
	for n := range a.cfg.Launchers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		l := a.cfg.Launchers[n]
		path, err := resolveLauncher(n, l)
		check(err == nil, "launcher %s → %s", n, orErr(path, err))
		if _, err := os.Stat(paths.ShimPath(n)); err != nil {
			check(false, "launcher %s has no shim; run: aiq shim install", n)
		}
	}

	shimDir := paths.ShimsDir()
	onPath := false
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if d == shimDir {
			onPath = true
			break
		}
	}
	_, shimErr := os.Stat(filepath.Join(shimDir, "claude"))
	check(shimErr == nil, "shims installed in %s", shimDir)
	check(onPath, "shim dir on PATH (export PATH=%q:$PATH)", shimDir)
	if which, err := exec.LookPath("claude"); err == nil {
		check(strings.HasPrefix(which, shimDir), "`claude` on PATH resolves to %s", which)
	}

	_, installed, _ := claude.InstalledStatusline(paths.RealClaudeHome())
	check(installed, "Claude statusline multiplexer installed in %s", claude.SettingsPath(paths.RealClaudeHome()))

	accounts, _ := a.st.ListAccounts("")
	check(len(accounts) > 0, "%d accounts registered", len(accounts))
	now := time.Now()
	for _, acc := range accounts {
		check(pool.HasCredential(acc), "%s: CLI login present (%s)%s", acc.ID, shortHome(acc.Home), hint(!pool.HasCredential(acc), "aiq account login "+acc.ID))
		if acc.Provider == "claude" {
			has := claude.HasGrant(acc.Home)
			check(has, "%s: poll grant present%s", acc.ID, hint(!has, "aiq account authorize "+acc.ID))
		}
		u, ok, _ := a.st.GetUsage(acc.ID)
		ws, _ := a.st.ListWindows(acc.ID)
		switch {
		case ok && u.PollError != "":
			check(false, "%s: last poll failed: %s", acc.ID, u.PollError)
		case len(ws) == 0:
			check(false, "%s: no telemetry yet (aiq account poll)", acc.ID)
		default:
			age := now.Sub(time.Unix(ws[0].ObservedAt, 0))
			stale := age > 2*time.Duration(a.cfg.Poll.IntervalSeconds)*time.Second+time.Minute
			check(!stale, "%s: telemetry %s old", acc.ID, age.Round(time.Second))
		}
		if !acc.Native {
			_, err := os.Lstat(filepath.Join(acc.Home, "settings.json"))
			if acc.Provider == "codex" {
				_, err = os.Lstat(filepath.Join(acc.Home, "config.toml"))
			}
			check(err == nil, "%s: overlay synced", acc.ID)
			if acc.Provider == "claude" && claude.CredentialDrifted(acc.Home) {
				check(false, "%s: the credential file a launcher refreshed is newer than the keychain copy; run: aiq account login %s", acc.ID, acc.ID)
			}
		}
	}

	_, svc := daemon.Installed()
	check(svc, "daemon service installed")
	check(daemon.Alive(a.cfg.Daemon.Listen), "daemon answering on http://%s/", a.cfg.Daemon.Listen)

	fmt.Println()
	if problems == 0 {
		fmt.Println("all checks passed")
	} else {
		fmt.Printf("%d problem(s)\n", problems)
	}
	return nil
}

func hint(show bool, cmd string) string {
	if !show {
		return ""
	}
	return " — run: " + cmd
}

func orErr(s string, err error) string {
	if err != nil {
		return err.Error()
	}
	return s
}
