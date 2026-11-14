package main

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/longrun"
	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/proc"
	"github.com/orlenko/aiq/internal/state"
)

const longDrainUsage = "usage: aiq long drain <lease-id|.> [--started <unix-seconds>]"

type longDrainOptions struct {
	ref         string
	id, started int64
	guarded     bool
}

func parseLongDrainArgs(args []string, explicitBackend bool) (longDrainOptions, error) {
	guarded := false
	for _, arg := range args {
		guarded = guarded || arg == "--started" || strings.HasPrefix(arg, "--started=")
	}
	if guarded {
		o, err := parseLongAttachArgs(args, explicitBackend)
		if err != nil {
			return longDrainOptions{}, err
		}
		if o.legacy || !o.checkStarted || o.started <= 0 {
			return longDrainOptions{}, errors.New(longDrainUsage)
		}
		return longDrainOptions{ref: strconv.FormatInt(o.id, 10), id: o.id, started: o.started, guarded: true}, nil
	}
	if len(args) != 1 {
		return longDrainOptions{}, errors.New(longDrainUsage)
	}
	if args[0] == "." {
		return longDrainOptions{ref: "."}, nil
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil || id <= 0 {
		return longDrainOptions{}, errors.New(longDrainUsage)
	}
	return longDrainOptions{ref: args[0], id: id}, nil
}

type longDrainNote interface {
	Retire(time.Time) (string, error)
	Close()
}
type longDrainDeps struct {
	open    func() (longAttachReader, error)
	host    func() (string, error)
	alive   func(int) bool
	prepare func(string) (longDrainNote, error)
	request func(state.LeaseTarget, time.Time, func() error) (bool, error)
	now     func() time.Time
}

var longDrainRuntime = defaultLongDrainDeps

func defaultLongDrainDeps() longDrainDeps {
	return longDrainDeps{open: func() (longAttachReader, error) { return state.OpenReadOnly(paths.StateDB()) }, host: func() (string, error) { return pool.Hostname(), nil }, alive: proc.Alive, now: time.Now,
		prepare: func(workspace string) (longDrainNote, error) { return longrun.PrepareNoteRetirement(workspace) },
		request: func(target state.LeaseTarget, now time.Time, prepare func() error) (bool, error) {
			store, err := state.OpenHook(paths.StateDB())
			if err != nil {
				return false, err
			}
			defer store.Close()
			matched, err := store.RequestDrainTarget(target, now, prepare)
			if matched && err == nil {
				provider, _, _ := state.ParseAccountID(target.AccountID)
				store.LogEvent(provider, target.AccountID, "long", fmt.Sprintf("lease %d: drain requested by user", target.ID), now)
			}
			return matched, err
		},
	}
}

func runLongDrain(o longDrainOptions, out io.Writer) error {
	return drainLongExact(o, longDrainRuntime(), out)
}
func drainLongExact(o longDrainOptions, d longDrainDeps, out io.Writer) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("guarded drain unavailable")
		}
	}()
	if !o.guarded || o.id <= 0 || o.started <= 0 || out == nil || d.open == nil || d.host == nil || d.alive == nil || d.prepare == nil || d.request == nil || d.now == nil {
		return errors.New("guarded drain unavailable")
	}
	reader, err := d.open()
	if err != nil || reader == nil {
		if reader != nil {
			reader.Close()
		}
		return errors.New("long-session state unavailable; list long sessions with aiq long list")
	}
	closed := false
	defer func() {
		if !closed {
			reader.Close()
		}
	}()
	target, readErr := reader.GetLeaseTarget(o.id)
	closed = true
	closeErr := reader.Close()
	if closeErr != nil {
		return errors.New("long-session state unavailable; list long sessions with aiq long list")
	}
	if errors.Is(readErr, state.ErrLeaseTargetNotFound) {
		return fmt.Errorf("no long lease %d; list long sessions with aiq long list", o.id)
	}
	if readErr != nil {
		return errors.New("long-session state unavailable; list long sessions with aiq long list")
	}
	if target.ID != o.id || target.Mode != state.ModeLong {
		return fmt.Errorf("lease %d is not an available long session; list long sessions with aiq long list", o.id)
	}
	if target.StartedAt != o.started {
		return fmt.Errorf("lease %d is not the one this command was copied for: it started at %d; list long sessions with aiq long list", o.id, target.StartedAt)
	}
	host, err := d.host()
	if err != nil || host == "" || target.Hostname != host || target.PID <= 0 || target.Pane == "" || target.Workspace == "" || target.AccountID == "" || !d.alive(target.PID) {
		return errors.New("recorded long session is not drain-ready on this machine; list long sessions and retry")
	}
	line := fmt.Sprintf("wrap-up target %q · %q\n", target.Workspace, target.AccountID)
	if n, err := io.WriteString(out, line); err != nil || n != len(line) {
		return errors.New("could not print drain target")
	}
	note, err := d.prepare(target.Workspace)
	if err != nil || note == nil {
		if note != nil {
			note.Close()
		}
		return errors.New("handoff note could not be prepared; no drain requested")
	}
	defer note.Close()
	archive := ""
	now := d.now()
	matched, err := d.request(target, now, func() error { var err error; archive, err = note.Retire(now); return err })
	if err != nil {
		if archive != "" {
			return errors.New("drain was not recorded; the previous note was archived; list long sessions and retry")
		}
		return errors.New("drain could not be recorded or the note changed; list long sessions and retry")
	}
	if !matched {
		return fmt.Errorf("lease %d changed while this request was preparing; list long sessions with aiq long list", o.id)
	}
	_, err = fmt.Fprintf(out, "lease %d: wrap-up requested; the daemon moves it at the next safe point\n", o.id)
	return err
}
