package main

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/orlenko/aiq/internal/herdr"
	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/proc"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/tmux"
)

const longAttachUsage = "usage: aiq long attach [.|<lease-id> [--started <unix-seconds>]]"

type longAttachOptions struct {
	legacy       bool
	id, started  int64
	checkStarted bool
}

func attachDigits(s string, max int) bool {
	if len(s) == 0 || len(s) > max {
		return false
	}
	for i := range s {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func parseLongAttachArgs(args []string, explicitBackend bool) (longAttachOptions, error) {
	var o longAttachOptions
	var positional []string
	ended := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !ended && a == "--" {
			ended = true
			continue
		}
		if !ended && (a == "--started" || strings.HasPrefix(a, "--started=")) {
			if o.checkStarted {
				return o, errors.New("--started may be given only once")
			}
			value, eq := strings.CutPrefix(a, "--started=")
			if !eq {
				i++
				if i == len(args) {
					return o, errors.New("--started requires Unix seconds")
				}
				value = args[i]
			}
			if !attachDigits(value, 12) {
				return o, errors.New("--started requires 1 to 12 decimal digits")
			}
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return o, errors.New("invalid copied start")
			}
			o.started, o.checkStarted = n, true
			continue
		}
		if !ended && strings.HasPrefix(a, "-") || len(positional) != 0 {
			return o, errors.New(longAttachUsage)
		}
		positional = append(positional, a)
	}
	if len(positional) == 0 || positional[0] == "." {
		if o.checkStarted {
			return o, errors.New("--started requires an explicit numeric lease id")
		}
		o.legacy = true
		return o, nil
	}
	if !attachDigits(positional[0], 10) {
		return o, errors.New("lease id requires 1 to 10 decimal digits")
	}
	id, err := strconv.ParseInt(positional[0], 10, 64)
	if err != nil || id <= 0 {
		return o, errors.New("lease id must be positive")
	}
	if explicitBackend {
		return o, errors.New("exact lease attachment uses its saved backend; omit --backend")
	}
	o.id = id
	return o, nil
}

type longAttachReader interface {
	GetLeaseTarget(int64) (state.LeaseTarget, error)
	Close() error
}

type longAttachDeps struct {
	open    func() (longAttachReader, error)
	host    func() (string, error)
	alive   func(int) bool
	prepare func(state.LeaseTarget) (func() error, error)
}

// Only the adapter construction is replaceable in dispatch tests. A runtime
// selection never opens the writable app or creates pool/configuration state.
var longAttachRuntime = defaultLongAttachDeps

func defaultLongAttachDeps() longAttachDeps {
	return longAttachDeps{
		open: func() (longAttachReader, error) {
			s, err := state.OpenReadOnly(paths.StateDB())
			if err != nil {
				return nil, err
			}
			return s, nil
		},
		host:  func() (string, error) { return pool.Hostname(), nil },
		alive: proc.Alive,
		prepare: func(t state.LeaseTarget) (func() error, error) {
			if herdr.IsRef(t.Pane) {
				return herdr.PrepareAttachExact(t.Pane, t.Workspace, t.PID)
			}
			return tmux.PrepareAttach(t.Pane, t.PID)
		},
	}
}

func runLongAttach(o longAttachOptions, out io.Writer) error {
	return attachLongExact(o, longAttachRuntime(), out)
}

func attachLongExact(o longAttachOptions, d longAttachDeps, out io.Writer) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("exact lease attachment unavailable")
		}
	}()
	if o.legacy || o.id <= 0 || d.open == nil || d.host == nil || d.alive == nil || d.prepare == nil || out == nil {
		return errors.New("exact lease attachment unavailable")
	}
	r, err := d.open()
	if err != nil || r == nil {
		if r != nil {
			_ = r.Close()
		}
		return errors.New("long-session state unavailable; list long sessions with aiq long list")
	}
	closed := false
	defer func() {
		if !closed {
			closed = true
			_ = r.Close()
		}
	}()
	t, readErr := r.GetLeaseTarget(o.id)
	closed = true
	if err := r.Close(); err != nil {
		return errors.New("long-session state unavailable; list long sessions with aiq long list")
	}
	if errors.Is(readErr, state.ErrLeaseTargetNotFound) {
		return fmt.Errorf("no long lease %d; list long sessions with aiq long list", o.id)
	}
	if readErr != nil {
		return errors.New("long-session state unavailable; list long sessions with aiq long list")
	}
	if t.ID != o.id || t.Mode != state.ModeLong {
		return fmt.Errorf("lease %d is not an available long session; list long sessions with aiq long list", o.id)
	}
	if o.checkStarted && t.StartedAt != o.started {
		return fmt.Errorf("lease %d is not the one this command was copied for: it started at %d; list long sessions with aiq long list", o.id, t.StartedAt)
	}
	host, err := d.host()
	if err != nil || host == "" || t.Hostname != host || t.PID <= 0 {
		return fmt.Errorf("lease %d is not a live local long session; list long sessions with aiq long list", o.id)
	}
	if !d.alive(t.PID) || t.Workspace == "" || t.AccountID == "" {
		return fmt.Errorf("lease %d is not attach-ready; list long sessions with aiq long list", o.id)
	}
	attach, err := d.prepare(t)
	if err != nil || attach == nil {
		return errors.New("recorded terminal unavailable or changed in this server context; list long sessions and retry")
	}
	line := fmt.Sprintf("attaching to %q · %q\n", t.Workspace, t.AccountID)
	if n, err := io.WriteString(out, line); err != nil || n != len(line) {
		return errors.New("could not print attachment target")
	}
	if err := attach(); err != nil {
		return errors.New("recorded terminal unavailable or changed; list long sessions and retry")
	}
	return nil
}
