package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/orlenko/aiq/internal/longrun"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/tmux"
)

// aiq long <provider> [args...]   start (or attach to) a supervised session
// aiq long list                   long sessions and their drain state
// aiq long drain <lease-id|.>     ask a session to wrap up and move now
// aiq long attach [.]             attach to this workspace's session
// aiq long stop <lease-id|.>      end the session and its tmux session
func cmdLong(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: aiq long claude|codex [args...] | list | drain <id|.> | attach [.] | stop <id|.>")
	}
	a, err := openApp()
	if err != nil {
		return err
	}
	defer a.close()
	switch args[0] {
	case "claude", "codex":
		return a.longStart(args[0], args[1:])
	case "list":
		return a.longList()
	case "drain":
		if len(args) < 2 {
			return fmt.Errorf("usage: aiq long drain <lease-id|.>")
		}
		l, err := a.longFind(args[1])
		if err != nil {
			return err
		}
		longrun.ArchiveNote(l.Workspace, time.Now())
		if err := a.st.SetLeaseDrain(l.ID, state.DrainRequested, time.Now()); err != nil {
			return err
		}
		a.st.LogEvent(l.Provider, l.AccountID, "long", fmt.Sprintf("lease %d: drain requested by user", l.ID), time.Now())
		fmt.Printf("lease %d (%s in %s): wrap-up requested; the daemon moves it at the next safe point\n", l.ID, l.AccountID, l.Pane)
		return nil
	case "attach":
		ws := workspaceID()
		name := longrun.SessionName(a.cfg.Long.TmuxPrefix, ws)
		if !tmux.HasSession(name) {
			return fmt.Errorf("no long session for %s (tmux session %s)", ws, name)
		}
		return tmux.Attach(name)
	case "stop":
		if len(args) < 2 {
			return fmt.Errorf("usage: aiq long stop <lease-id|.>")
		}
		l, err := a.longFind(args[1])
		if err != nil {
			return err
		}
		name := longrun.SessionName(a.cfg.Long.TmuxPrefix, l.Workspace)
		a.st.ReleaseLease(l.ID)
		a.st.LogEvent(l.Provider, l.AccountID, "long", fmt.Sprintf("lease %d: stopped by user", l.ID), time.Now())
		if tmux.HasSession(name) {
			return tmux.Kill(name)
		}
		return nil
	}
	return fmt.Errorf("unknown long subcommand %q", args[0])
}

func (a *app) longLeases() ([]state.Lease, error) {
	leases, err := a.st.ListLeases()
	if err != nil {
		return nil, err
	}
	var out []state.Lease
	for _, l := range leases {
		if l.Mode == state.ModeLong {
			out = append(out, l)
		}
	}
	return out, nil
}

// longFind resolves "." (this workspace) or a lease id.
func (a *app) longFind(ref string) (state.Lease, error) {
	leases, err := a.longLeases()
	if err != nil {
		return state.Lease{}, err
	}
	if ref == "." {
		ws := workspaceID()
		for _, l := range leases {
			if l.Workspace == ws {
				return l, nil
			}
		}
		return state.Lease{}, fmt.Errorf("no long session for %s", ws)
	}
	id, err := strconv.ParseInt(ref, 10, 64)
	if err != nil {
		return state.Lease{}, fmt.Errorf("lease id or '.' expected, got %q", ref)
	}
	for _, l := range leases {
		if l.ID == id {
			return l, nil
		}
	}
	return state.Lease{}, fmt.Errorf("no long lease %d", id)
}

func (a *app) longStart(provider string, args []string) error {
	if !tmux.Available() {
		return fmt.Errorf("tmux is required for long sessions (not found on PATH)")
	}
	ws := workspaceID()
	name := longrun.SessionName(a.cfg.Long.TmuxPrefix, ws)
	if tmux.HasSession(name) {
		leases, _ := a.longLeases()
		for _, l := range leases {
			if l.Workspace == ws {
				fmt.Fprintf(os.Stderr, "aiq: long session already running for %s on %s (lease %d); attaching\n", ws, l.AccountID, l.ID)
				return tmux.Attach(name)
			}
		}
		// A stale session with no lease: replace it.
		tmux.Kill(name)
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	fallback := strings.Join(append([]string{provider}, a.cfg.Long.Fallback...), ",")
	cmdArgs := append([]string{self, "run", provider, "--long", "--fallback", fallback, "--"}, args...)
	pane, err := tmux.NewSession(name, ws, tmux.Quote(cmdArgs))
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "aiq: long %s session started in tmux %s (pane %s) for %s\n", provider, name, pane, ws)
	if term.IsTerminal(int(os.Stdout.Fd())) {
		return tmux.Attach(name)
	}
	fmt.Printf("attach with: tmux attach -t %s   (or: aiq long attach)\n", name)
	return nil
}

func (a *app) longList() error {
	leases, err := a.longLeases()
	if err != nil {
		return err
	}
	if len(leases) == 0 {
		fmt.Println("no long sessions — start one with: aiq long claude|codex [args]")
		return nil
	}
	now := time.Now()
	fmt.Printf("%-5s %-20s %-9s %-10s %-6s %-8s %s\n", "LEASE", "ACCOUNT", "PANE", "DRAIN", "TURN", "SINCE", "WORKSPACE")
	for _, l := range leases {
		drain := l.Drain
		if drain == "" {
			drain = "-"
		}
		turn := "idle"
		if l.InTurn() {
			turn = "busy"
		}
		fmt.Printf("%-5d %-20s %-9s %-10s %-6s %-8s %s\n", l.ID, l.AccountID, l.Pane, drain, turn, fmtAgo(l.StartedAt, now), l.Workspace)
	}
	return nil
}
