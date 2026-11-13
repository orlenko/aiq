package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/herdr"
	"github.com/orlenko/aiq/internal/longrun"
	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/proc"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/terminal"
	"github.com/orlenko/aiq/internal/tmux"
)

func longBackendArgs(args []string) (string, []string, error) {
	backend := ""
	if len(args) > 0 && (args[0] == "--backend" || strings.HasPrefix(args[0], "--backend=")) {
		value, eq := strings.CutPrefix(args[0], "--backend=")
		if eq {
			args = args[1:]
		} else {
			if len(args) < 2 {
				return "", nil, errors.New("--backend requires tmux or herdr")
			}
			value, args = args[1], args[2:]
		}
		if value != "tmux" && value != "herdr" {
			return "", nil, errors.New("--backend must be tmux or herdr")
		}
		backend = value
	}
	return backend, args, nil
}

// Environment markers are only a creation preference. CurrentCore still has
// to verify native terminal ownership before a new Herdr session can start.
func longBackendForContext(requested string) string {
	if requested != "" {
		return requested
	}
	if os.Getenv("TMUX") != "" || os.Getenv("TMUX_PANE") != "" || os.Getenv("STY") != "" {
		return "tmux"
	}
	_, ownerPresent := os.LookupEnv("HC_TERMINAL_SOCKET")
	if os.Getenv("HC_TERMINAL_ID") != "" || ownerPresent {
		return "herdr"
	}
	return "tmux"
}

func (t longTarget) unlock() {
	if t.release != nil {
		t.release()
	}
}

func longLeaseStore(st *state.Store, l state.Lease) *state.Store {
	if herdr.IsRef(l.Pane) {
		return st.ForLease(l, 0)
	}
	return st
}

func (a *app) longExisting(ws string) (t longTarget, attached bool, err error) {
	release, err := herdr.LockWorkspace(ws)
	if err != nil {
		return t, false, err
	}
	t.release = release
	r, owned, err := herdr.Workspace(ws)
	if owned && err != nil && r.Ref != "" {
		if gone, e := herdr.Reclaim(r); e == nil && gone {
			owned, err = false, nil
		}
	}
	if err != nil {
		t.unlock()
		return t, false, fmt.Errorf("native Herdr ownership is uncertain; no second session started: %w", err)
	}
	if owned {
		t.unlock()
		longNote(os.Stderr, "aiq: long session already running in Herdr %s; attaching", r.Core.Terminal)
		return t, true, herdr.Attach(r.Ref)
	}
	// A missing native descriptor is not permission to ignore a live native
	// lease; attachment must fail closed instead of starting a second copy.
	leases, err := a.longLeases()
	if err != nil {
		t.unlock()
		return t, false, err
	}
	for _, l := range leases {
		if l.Workspace == ws && herdr.IsRef(l.Pane) {
			exists, _, e := terminal.State(l.Pane)
			if e == nil && !exists {
				a.st.ForLease(l, 0).ReleaseLease(l.ID)
				continue
			}
			t.unlock()
			if e != nil {
				return t, false, e
			}
			return t, true, herdr.Attach(l.Pane)
		}
	}
	name := longrun.SessionName(a.cfg.Long.TmuxPrefix, ws)
	backend := longBackendForContext(a.longBackend)
	hasTmux := backend == "herdr" && tmux.Available() && tmux.HasSession(name)
	// An automatic switch must not treat an unreachable tmux owner as absent.
	if backend == "herdr" && a.longBackend == "" && !hasTmux {
		for _, l := range leases {
			if l.Workspace != ws || herdr.IsRef(l.Pane) {
				continue
			}
			if l.Hostname != "" && l.Hostname == pool.Hostname() && l.PID > 0 && !proc.Alive(l.PID) {
				continue
			}
			t.unlock()
			return t, false, fmt.Errorf("tmux ownership is uncertain for long lease %d; no second session started (use the original tmux context to attach)", l.ID)
		}
	}
	if backend != "herdr" || hasTmux {
		t, attached, err = a.longExistingTmux(ws, release)
		if attached || err != nil || backend != "herdr" {
			if err != nil {
				t.unlock()
			}
			return t, attached, err
		}
	}
	core, err := herdr.CurrentCore()
	if err != nil {
		t.unlock()
		if a.longBackend == "" {
			err = fmt.Errorf("cannot use the detected Herdr terminal: %w (put --backend tmux before the subcommand, e.g. aiq long --backend tmux auto)", err)
		}
		return t, false, err
	}
	t.native, t.core, t.name = true, core, name
	return t, false, nil
}

func (a *app) longLaunchHerdr(t longTarget, ws, dir, what string, args []string) error {
	longNote(os.Stderr, "aiq: long %s session starting in Herdr %s for %s", what, t.core.Terminal, ws)
	a.close()
	code, err := herdr.Run(herdr.Options{Core: t.core, Workspace: ws, Session: t.name, Dir: dir, Args: args, Ready: t.release, Hook: nativeHook})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("native Herdr agent exited with status %d; resume explicitly to continue", code)
	}
	return nil
}

func (a *app) attachLongWorkspace(ws string) error {
	r, owned, err := herdr.Workspace(ws)
	if err != nil {
		return err
	}
	if owned {
		return herdr.Attach(r.Ref)
	}
	name := longrun.SessionName(a.cfg.Long.TmuxPrefix, ws)
	if !tmux.HasSession(name) {
		return fmt.Errorf("no long session for %s", oneLine(ws))
	}
	return tmux.Attach(name)
}

// Listing observes each family once, within one aggregate control budget.
// A timed-out or unqueried descriptor still reserves and displays its workspace.
type nativeListing struct {
	record herdr.Record
	err    error
}

func listNativeOwnership(leases []state.Lease) (map[string]nativeListing, []herdr.Record, error) {
	records, err := herdr.Records()
	if err != nil {
		return nil, nil, err
	}
	out := map[string]nativeListing{}
	deadline := time.Now().Add(2 * time.Second)
	observe := func(ref string, saved herdr.Record) {
		family, _, err := herdr.ParseRef(ref)
		if err != nil {
			out[ref] = nativeListing{saved, err}
			return
		}
		if _, exists := out[family]; exists {
			return
		}
		left := min(500*time.Millisecond, time.Until(deadline))
		if left <= 0 {
			out[family] = nativeListing{saved, errors.New("Herdr listing control budget exhausted")}
			return
		}
		current, err := herdr.InfoWithin(family, left)
		if err == nil {
			saved = current
		}
		out[family] = nativeListing{saved, err}
	}
	for _, r := range records {
		observe(r.Ref, r)
	}
	for _, l := range leases {
		if herdr.IsRef(l.Pane) {
			observe(l.Pane, herdr.Record{})
		}
	}
	return out, records, nil
}
func listingFamily(ref string) string {
	family, _, err := herdr.ParseRef(ref)
	if err != nil {
		return ref
	}
	return family
}
