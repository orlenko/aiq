//go:build darwin || linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/herdr"
	"github.com/orlenko/aiq/internal/longrun"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/tmux"
)

func nativeTmuxCommand(role, ws, name string) []string {
	return []string{"/usr/bin/env", "GO_NATIVE_TMUX_ROLE=" + role, "GO_NATIVE_TMUX_WS=" + ws, "GO_NATIVE_TMUX_NAME=" + name, "AIQ_DATA_DIR=" + os.Getenv("AIQ_DATA_DIR"), os.Args[0], "-test.run=^TestNativeTmuxStartFixture$", "--", "run", "claude", "--long"}
}
func TestNativeTmuxStartFixture(t *testing.T) {
	role := os.Getenv("GO_NATIVE_TMUX_ROLE")
	if role == "" {
		return
	}
	ws, name := os.Getenv("GO_NATIVE_TMUX_WS"), os.Getenv("GO_NATIVE_TMUX_NAME")
	if role == "child" {
		os.WriteFile(filepath.Join(ws, "provider-ready"), []byte(strconv.Itoa(os.Getpid())), 0600)
		for {
			time.Sleep(time.Hour)
		}
	}
	release, err := herdr.LockWorkspace(ws)
	if err != nil {
		fmt.Println(err)
		os.Exit(2)
	}
	a := &app{cfg: config.Default()}
	err = a.longLaunchInPlace(longTarget{name: name, pane: os.Getenv("TMUX_PANE"), inside: true, release: release}, ws, ws, "claude", "", nativeTmuxCommand("child", ws, name))
	// A successful self-respawn kills this process; reaching here is failure.
	fmt.Println("SELF RESPAWN RETURNED", err)
	os.Exit(3)
}

func TestNativeAndTmuxStartRespectRealPreleasePane(t *testing.T) {
	if !tmux.Available() {
		t.Skip("tmux unavailable")
	}
	for _, self := range []bool{false, true} {
		t.Run(fmt.Sprint(self), func(t *testing.T) {
			t.Setenv("AIQ_DATA_DIR", t.TempDir())
			ws := t.TempDir()
			cfg := config.Default()
			cfg.Long.TmuxPrefix = "aiq-fixture-"
			name := longrun.SessionName(cfg.Long.TmuxPrefix, ws)
			t.Cleanup(func() { tmux.Kill(name) })
			st, err := state.Open(filepath.Join(os.Getenv("AIQ_DATA_DIR"), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			a := &app{cfg: cfg, st: st}
			var user string
			if self {
				if _, err := tmux.NewSession(name, ws, tmux.Quote(nativeTmuxCommand("self", ws, name))); err != nil {
					t.Fatal(err)
				}
			} else {
				pane, err := tmux.NewSession(name, ws, "exit 0")
				if err != nil {
					t.Fatal(err)
				}
				out, err := exec.Command("tmux", "split-window", "-d", "-P", "-F", "#{pane_id}", "-t", pane, "sleep 30").CombinedOutput()
				if err != nil {
					t.Fatalf("split user pane: %v %s", err, out)
				}
				user = strings.TrimSpace(string(out))
				release, err := herdr.LockWorkspace(ws)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
				if err := a.longLaunchInPlace(longTarget{name: name, pane: pane, release: release}, ws, ws, "claude", "", nativeTmuxCommand("child", ws, name)); err != nil {
					t.Fatal(err)
				}
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, err := os.Stat(filepath.Join(ws, "provider-ready")); err == nil {
					break
				}
				if time.Now().After(deadline) {
					panes, _ := tmux.SessionPanes(name)
					t.Fatalf("provider fixture not ready: %+v", panes)
				}
				time.Sleep(10 * time.Millisecond)
			}
			release, err := herdr.LockWorkspace(ws)
			if err != nil {
				t.Fatalf("accepted respawn did not release lock: %v", err)
			}
			release()
			before, err := tmux.SessionPanes(name)
			if err != nil {
				t.Fatal(err)
			}
			for _, backend := range []string{"tmux", "herdr"} {
				a.longBackend = backend
				target, attached, err := a.longExisting(ws)
				target.unlock()
				if attached || err == nil || !strings.Contains(err.Error(), "no lease holds yet") {
					t.Fatalf("%s ignored live prelease ownership: attached=%v err=%v", backend, attached, err)
				}
			}
			after, err := tmux.SessionPanes(name)
			if err != nil || len(after) != len(before) {
				t.Fatalf("competitor removed panes: %+v %v", after, err)
			}
			for i, p := range before {
				if after[i].ID != p.ID || after[i].PID != p.PID || after[i].Dead {
					t.Fatalf("winner/user pane changed: before=%+v after=%+v", before, after)
				}
			}
			if user != "" {
				exists, running, err := tmux.PaneState(user)
				if err != nil || !exists || !running {
					t.Fatalf("user pane lost: %v %v %v", exists, running, err)
				}
			}
		})
	}
}
