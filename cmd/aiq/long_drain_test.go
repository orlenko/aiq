package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/state"
)

type drainTestNote struct {
	events    *[]string
	retireErr error
	archive   string
}

func (n *drainTestNote) Retire(time.Time) (string, error) {
	*n.events = append(*n.events, "retire")
	return n.archive, n.retireErr
}
func (n *drainTestNote) Close() { *n.events = append(*n.events, "note-close") }

func drainTestDeps(r *attachTestReader) longDrainDeps {
	d := attachTestDeps(r)
	return longDrainDeps{open: d.open, host: d.host, alive: d.alive, now: func() time.Time { return time.Unix(500, 0) }, prepare: func(workspace string) (longDrainNote, error) {
		*r.events = append(*r.events, "prepare")
		if workspace != r.target.Workspace {
			return nil, errors.New("wrong workspace")
		}
		return &drainTestNote{events: r.events}, nil
	}, request: func(target state.LeaseTarget, now time.Time, retire func() error) (bool, error) {
		*r.events = append(*r.events, "request")
		if target != r.target || now.Unix() != 500 {
			return false, errors.New("wrong target")
		}
		if err := retire(); err != nil {
			return false, err
		}
		return true, nil
	}}
}

func TestLongDrainGrammarAndDispatcherBeforeInitialization(t *testing.T) {
	for _, args := range [][]string{{"7", "--started=123"}, {"--started", "123", "7"}, {"--started=123", "--", "7"}} {
		o, err := parseLongDrainArgs(args, false)
		if err != nil || !o.guarded || o.id != 7 || o.started != 123 {
			t.Fatal("guard grammar", args, o, err)
		}
	}
	for _, args := range [][]string{{"."}, {"7"}, {"9223372036854775807"}} {
		o, err := parseLongDrainArgs(args, false)
		if err != nil || o.guarded {
			t.Fatal("legacy grammar", args, err)
		}
	}
	invalid := [][]string{{"drain", "7", "--started="}, {"drain", ".", "--started=123"}, {"drain", "7", "--started=0"}, {"drain", "7", "--started=1000000000000"}, {"drain", "10000000000", "--started=123"}, {"drain", "7", "--started=123", "--started=123"}, {"--backend=tmux", "drain", "7", "--started=123"}, {"drain", "7", "--started=123", "PRIVATE-invalid"}, {"drain", "7", "extra"}}
	root := t.TempDir()
	data := filepath.Join(root, "absent-data")
	cfg := filepath.Join(root, "invalid.toml")
	if err := os.WriteFile(cfg, []byte("deliberately invalid = ["), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIQ_DATA_DIR", data)
	t.Setenv("AIQ_CONFIG", cfg)
	saved := longDrainRuntime
	defer func() { longDrainRuntime = saved }()
	events := []string{}
	r := &attachTestReader{lookupErr: state.ErrLeaseTargetNotFound, events: &events}
	calls := 0
	longDrainRuntime = func() longDrainDeps { calls++; return drainTestDeps(r) }
	for _, args := range invalid {
		if err := cmdLong(args); err == nil || strings.Contains(err.Error(), "PRIVATE") {
			t.Fatal("invalid dispatch", args, err)
		}
	}
	if calls != 0 {
		t.Fatal("invalid syntax opened drain dependencies")
	}
	if err := cmdLong([]string{"drain", "7", "--started=123"}); err == nil {
		t.Fatal("missing lease accepted")
	}
	if calls != 1 || !reflect.DeepEqual(events, []string{"open", "get:7", "close"}) {
		t.Fatal("not read-only early dispatch", calls, events)
	}
	if _, err := os.Stat(data); !os.IsNotExist(err) {
		t.Fatal("dispatch initialized data")
	}
}

func TestLongDrainRefusalsNeverRetireAnUnmatchedLease(t *testing.T) {
	for _, kind := range []string{"success", "missing", "lookup", "close", "wrong-id", "mode", "start", "foreign", "dead", "empty-pane", "empty-workspace", "prepare-error", "prepare-partial", "changed", "note-changed", "commit-error", "archived-commit-error"} {
		t.Run(kind, func(t *testing.T) {
			events := []string{}
			r := &attachTestReader{target: attachTestTarget(), events: &events}
			d := drainTestDeps(r)
			switch kind {
			case "missing":
				r.lookupErr = state.ErrLeaseTargetNotFound
			case "lookup":
				r.lookupErr = errors.New("PRIVATE read")
			case "close":
				r.closeErr = errors.New("PRIVATE close")
			case "wrong-id":
				r.target.ID++
			case "mode":
				r.target.Mode = state.ModeWorker
			case "start":
				r.target.StartedAt++
			case "foreign":
				r.target.Hostname = "other-host"
			case "dead":
				d.alive = func(int) bool { events = append(events, "alive:2468"); return false }
			case "empty-pane":
				r.target.Pane = ""
			case "empty-workspace":
				r.target.Workspace = ""
			case "prepare-error":
				d.prepare = func(string) (longDrainNote, error) {
					events = append(events, "prepare")
					return nil, errors.New("PRIVATE preparation")
				}
			case "prepare-partial":
				d.prepare = func(string) (longDrainNote, error) {
					events = append(events, "prepare")
					return &drainTestNote{events: &events}, errors.New("PRIVATE preparation")
				}
			case "changed":
				d.request = func(state.LeaseTarget, time.Time, func() error) (bool, error) {
					events = append(events, "request")
					return false, nil
				}
			case "note-changed":
				d.prepare = func(string) (longDrainNote, error) {
					events = append(events, "prepare")
					return &drainTestNote{events: &events, retireErr: errors.New("PRIVATE changed")}, nil
				}
			case "commit-error", "archived-commit-error":
				d.request = func(_ state.LeaseTarget, _ time.Time, retire func() error) (bool, error) {
					events = append(events, "request")
					if err := retire(); err != nil {
						return false, err
					}
					return false, errors.New("PRIVATE commit")
				}
				if kind == "archived-commit-error" {
					d.prepare = func(string) (longDrainNote, error) {
						events = append(events, "prepare")
						return &drainTestNote{events: &events, archive: "/invented/archived.md"}, nil
					}
				}
			}
			var out bytes.Buffer
			err := drainLongExact(longDrainOptions{id: 7, started: 123, guarded: true}, d, &out)
			if (err == nil) != (kind == "success") {
				t.Fatal("result", err)
			}
			if err != nil && strings.Contains(err.Error(), "PRIVATE") {
				t.Fatal("private error reflected")
			}
			joined := strings.Join(events, ",")
			closes := 0
			for _, event := range events {
				if event == "close" {
					closes++
				}
			}
			if closes != 1 {
				t.Fatal("reader not closed once", events)
			}
			shouldPrepare := kind == "success" || strings.HasPrefix(kind, "prepare-") || kind == "changed" || kind == "note-changed" || strings.Contains(kind, "commit-error")
			if strings.Contains(joined, "prepare") != shouldPrepare {
				t.Fatal("unavailable lease reached note", events)
			}
			shouldRetire := kind == "success" || kind == "note-changed" || strings.Contains(kind, "commit-error")
			if strings.Contains(joined, "retire") != shouldRetire {
				t.Fatal("unmatched lease retired note", events)
			}
			if kind == "archived-commit-error" && !strings.Contains(err.Error(), "previous note was archived") {
				t.Fatal("partial outcome concealed", err)
			}
			if kind == "success" && out.String() != "wrap-up target \"/selected/work\" · \"codex/fixture\"\nlease 7: wrap-up requested; the daemon moves it at the next safe point\n" {
				t.Fatal("success output", out.String())
			}
			if shouldPrepare && !strings.HasPrefix(out.String(), "wrap-up target ") {
				t.Fatal("action without target echo")
			}
		})
	}
}

func TestLongDrainCannotActBeforeSuccessfulTargetEcho(t *testing.T) {
	for _, short := range []bool{false, true} {
		events := []string{}
		r := &attachTestReader{target: attachTestTarget(), events: &events}
		err := drainLongExact(longDrainOptions{id: 7, started: 123, guarded: true}, drainTestDeps(r), attachTestWriter{events: &events, short: short, err: io.ErrClosedPipe})
		if err == nil || !reflect.DeepEqual(events, []string{"open", "get:7", "close", "host", "alive:2468", "write"}) {
			t.Fatal("failed echo reached note/database", events, err)
		}
	}
}
