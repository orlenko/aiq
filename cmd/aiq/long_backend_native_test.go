//go:build darwin || linux

package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/herdr"
	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/testenv"
)

// Exercise the real plain CLI in a private core PTY, with an invented Codex
// account/provider. Fake tmux observations cover inactive recovery targets;
// any unexpected provider creation through tmux fails and is recorded.
func TestLongAutoSelectsNativeInPrivateCore(t *testing.T) {
	if _, err := exec.LookPath("hc"); err != nil {
		t.Skip("hc unavailable; private native PTY check not run")
	}
	for _, fixture := range []string{"new", "dead only", "dead and user"} {
		t.Run(fixture, func(t *testing.T) {
			clearLongContext(t)
			root, err := os.MkdirTemp("/tmp", "aiqh-auto-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(root) })
			root, err = filepath.EvalSymlinks(root)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("AIQ_DATA_DIR", filepath.Join(root, "data"))
			t.Setenv("AIQ_CONFIG", filepath.Join(root, "config.toml"))
			t.Setenv("HC_SOCKET", filepath.Join(root, "core.sock"))
			t.Setenv("HC_SESSION", "aiq-auto-fixture")
			t.Cleanup(func() { stopFixtureCore(t) })
			panes := ""
			if fixture != "new" {
				panes = "%7:123:1:fixture:/invented/aiq run codex --long"
			}
			if fixture == "dead and user" {
				panes += "\n%8:456:0:fixture:sleep 30"
			}
			log := autoTmuxFixture(t, panes)
			bin := filepath.Join(root, "aiq")
			if out, err := testenv.GoCommand("build", "-o", bin, ".").CombinedOutput(); err != nil {
				t.Fatalf("fixture build: %v %s", err, out)
			}
			provider := filepath.Join(root, "fiction-provider")
			if err := os.WriteFile(provider, []byte("#!/bin/sh\nprintf 'AUTO_NATIVE_READY\\n'\nwhile IFS= read -r line; do [ \"$line\" = quit ] && exit 0; done\n"), 0700); err != nil {
				t.Fatal(err)
			}
			cfg := config.Default()
			cfg.Auto.Providers = []string{"codex"}
			cfg.Long.Fallback = []string{"codex"}
			cfg.Providers.Codex.Binary = provider
			cfg.Updates.Enabled = false
			if err := config.Save(cfg); err != nil {
				t.Fatal(err)
			}
			if err := paths.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			st, err := state.Open(paths.StateDB())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if err := st.AddAccount(state.Account{ID: "codex/fiction", Provider: "codex", Name: "fiction", Native: true, Enabled: true, Home: filepath.Join(root, "codex")}); err != nil {
				t.Fatal(err)
			}
			setFakeUsage(t, st, "codex/fiction", 10)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := fixtureHC(ctx, "run", "--bg", "--json", "--no-parent", "--cwd", root, "--", bin, "long", "auto")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("plain long auto: %v %s", err, out)
			}
			var started struct {
				Address string `json:"address"`
			}
			if json.Unmarshal(out, &started) != nil || started.Address == "" {
				t.Fatal("invalid private terminal reply")
			}
			waitFakeText(t, started.Address, "AUTO_NATIVE_READY")
			lease := nativeLease(t, st)
			record, err := herdr.Info(lease.Pane)
			if err != nil || record.Core.Terminal != started.Address || record.Workspace != root || record.Generation != 1 || record.PID != lease.PID {
				t.Fatalf("wrong native ownership: %v", err)
			}
			leases, err := st.ListLeases()
			if err != nil || len(leases) != 1 {
				t.Fatal("plain start duplicated leases", err)
			}
			unlock, err := herdr.LockWorkspace(root)
			if err != nil {
				t.Fatal("startup lock not released", err)
			}
			unlock()
			b, _ := os.ReadFile(log)
			for _, call := range strings.Fields(string(b)) {
				if call != "has-session" && call != "list-panes" && !(call == "kill-session" && fixture == "dead only") {
					t.Fatalf("unexpected tmux operation: %s", call)
				}
			}
			if got := strings.Count(string(b), "kill-session\n"); (fixture == "dead only" && got != 1) || (fixture != "dead only" && got != 0) {
				t.Fatal("stale cleanup changed user-pane preservation")
			}
			if err := herdr.Stop(lease.Pane, lease.PID); err != nil {
				t.Fatal(err)
			}
			awaitNativeAttempt(t, nativeAttempt{root, os.Getenv("HC_SOCKET"), started.Address, ""})
		})
	}
}
