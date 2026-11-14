package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/orlenko/aiq/internal/state"
)

func TestLongAttachGrammar(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		backend bool
		want    longAttachOptions
	}{
		{nil, false, longAttachOptions{legacy: true}}, {[]string{"."}, false, longAttachOptions{legacy: true}}, {[]string{"--"}, true, longAttachOptions{legacy: true}},
		{[]string{"7"}, false, longAttachOptions{id: 7}}, {[]string{"0000000007", "--started=000000000000"}, false, longAttachOptions{id: 7, checkStarted: true}},
		{[]string{"--started", "123", "7"}, false, longAttachOptions{id: 7, started: 123, checkStarted: true}},
		{[]string{"7", "--started", "123"}, false, longAttachOptions{id: 7, started: 123, checkStarted: true}},
		{[]string{"--started=999999999999", "--", "9999999999"}, false, longAttachOptions{id: 9999999999, started: 999999999999, checkStarted: true}},
		{[]string{"--", "7"}, false, longAttachOptions{id: 7}},
	} {
		got, err := parseLongAttachArgs(tc.args, tc.backend)
		if err != nil || got != tc.want {
			t.Errorf("grammar %q backend=%v: %+v %v", tc.args, tc.backend, got, err)
		}
	}
	invalid := [][]string{{"0"}, {"0000000000"}, {"10000000000"}, {"+1"}, {"-1"}, {" 7"}, {"7 "}, {"７"}, {"7\x00"}, {"7\n"}, {"1.0"}, {"1e1"}, {"7", "8"}, {".", "--started=0"}, {"--started=0"}, {"--started"}, {"7", "--started="}, {"7", "--started=-1"}, {"7", "--started=+1"}, {"7", "--started=1000000000000"}, {"7", "--started=1", "--started", "1"}, {"7", "--started=1", "--started=1"}, {"7", "--backend=tmux"}, {"7", "--unknown=PRIVATE"}, {"--", "7", "--started=0"}, {"--", "--started=0", "7"}}
	for _, args := range invalid {
		if _, err := parseLongAttachArgs(args, false); err == nil {
			t.Errorf("accepted %q", args)
		} else if strings.Contains(err.Error(), "PRIVATE") {
			t.Fatal("raw invalid token reflected")
		}
	}
	if _, err := parseLongAttachArgs([]string{"7"}, true); err == nil {
		t.Fatal("explicit backend accepted for exact ID")
	}
}

type attachTestReader struct {
	target              state.LeaseTarget
	lookupErr, closeErr error
	events              *[]string
}

func (r *attachTestReader) GetLeaseTarget(id int64) (state.LeaseTarget, error) {
	*r.events = append(*r.events, fmt.Sprintf("get:%d", id))
	return r.target, r.lookupErr
}
func (r *attachTestReader) Close() error { *r.events = append(*r.events, "close"); return r.closeErr }
func attachTestTarget() state.LeaseTarget {
	return state.LeaseTarget{ID: 7, StartedAt: 123, AccountID: "codex/fixture", Hostname: "machine-identity-fixture", Mode: state.ModeLong, Workspace: "/selected/work", Pane: "%17", PID: 2468}
}
func attachTestDeps(reader *attachTestReader) longAttachDeps {
	return longAttachDeps{open: func() (longAttachReader, error) { *reader.events = append(*reader.events, "open"); return reader, nil }, host: func() (string, error) {
		*reader.events = append(*reader.events, "host")
		return "machine-identity-fixture", nil
	}, alive: func(pid int) bool { *reader.events = append(*reader.events, fmt.Sprintf("alive:%d", pid)); return true }, prepare: func(target state.LeaseTarget) (func() error, error) {
		*reader.events = append(*reader.events, "prepare")
		if target != reader.target {
			return nil, errors.New("snapshot changed")
		}
		return func() error { *reader.events = append(*reader.events, "attach"); return nil }, nil
	}}
}

func TestLongAttachSnapshotOrderAndRefusals(t *testing.T) {
	for _, mode := range []string{"success", "unguarded", "open-partial", "missing", "lookup", "close", "wrong-id", "mode", "started", "host-error", "empty-host", "foreign", "zero-pid", "dead", "empty-workspace", "empty-account", "prepare", "action"} {
		t.Run(mode, func(t *testing.T) {
			events := []string{}
			reader := &attachTestReader{target: attachTestTarget(), events: &events}
			deps := attachTestDeps(reader)
			opts := longAttachOptions{id: 7, started: 123, checkStarted: true}
			switch mode {
			case "unguarded":
				opts.checkStarted = false
			case "open-partial":
				deps.open = func() (longAttachReader, error) {
					events = append(events, "open")
					return reader, errors.New("PRIVATE open detail")
				}
			case "missing":
				reader.lookupErr = state.ErrLeaseTargetNotFound
			case "lookup":
				reader.lookupErr = errors.New("PRIVATE database detail")
			case "close":
				reader.closeErr = errors.New("PRIVATE close detail")
			case "mode":
				reader.target.Mode = state.ModeWorker
			case "wrong-id":
				reader.target.ID = 8
			case "started":
				reader.target.StartedAt = 124
			case "host-error":
				deps.host = func() (string, error) { events = append(events, "host"); return "", errors.New("PRIVATE host") }
			case "empty-host":
				deps.host = func() (string, error) { events = append(events, "host"); return "", nil }
			case "foreign":
				reader.target.Hostname = "other-machine"
			case "zero-pid":
				reader.target.PID = 0
			case "dead":
				deps.alive = func(pid int) bool { events = append(events, fmt.Sprintf("alive:%d", pid)); return false }
			case "empty-workspace":
				reader.target.Workspace = ""
			case "empty-account":
				reader.target.AccountID = ""
			case "prepare":
				deps.prepare = func(state.LeaseTarget) (func() error, error) {
					events = append(events, "prepare")
					return nil, errors.New("PRIVATE pane error")
				}
			case "action":
				deps.prepare = func(state.LeaseTarget) (func() error, error) {
					events = append(events, "prepare")
					return func() error { events = append(events, "attach"); return errors.New("PRIVATE action") }, nil
				}
			}
			var out bytes.Buffer
			err := attachLongExact(opts, deps, &out)
			want := []string{"open", "get:7", "close"}
			switch mode {
			case "open-partial":
				want = []string{"open", "close"}
			case "success", "unguarded", "prepare", "action":
				want = append(want, "host", "alive:2468", "prepare")
				if mode != "prepare" {
					want = append(want, "attach")
				}
			case "host-error", "empty-host", "foreign", "zero-pid":
				want = append(want, "host")
			case "dead":
				want = append(want, "host", "alive:2468")
			case "empty-workspace", "empty-account":
				want = append(want, "host", "alive:2468")
			}
			if !reflect.DeepEqual(events, want) {
				t.Fatalf("order %v, want %v", events, want)
			}
			if (err == nil) != (mode == "success" || mode == "unguarded") {
				t.Fatalf("unexpected result %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "PRIVATE") {
				t.Fatal("dependency detail leaked")
			}
			if mode == "started" && err.Error() != "lease 7 is not the one this command was copied for: it started at 124; list long sessions with aiq long list" {
				t.Fatal("copied-start diagnostic lost numeric identity")
			}
			printed := mode == "success" || mode == "unguarded" || mode == "action"
			if printed {
				if out.String() != "attaching to \"/selected/work\" · \"codex/fixture\"\n" {
					t.Fatal("attempt line mismatch")
				}
			} else if out.Len() != 0 {
				t.Fatal("attempt printed before readiness")
			}
			if mode == "empty-workspace" || mode == "empty-account" {
				if strings.Contains(strings.Join(events, ","), "prepare") {
					t.Fatal("invalid metadata reached backend")
				}
			}
		})
	}
}

type attachTestWriter struct {
	events *[]string
	short  bool
	err    error
}

func (w attachTestWriter) Write(p []byte) (int, error) {
	*w.events = append(*w.events, "write")
	if w.short {
		return len(p) - 1, nil
	}
	return 0, w.err
}

func TestLongAttachQuotedOutputAndWriteFailure(t *testing.T) {
	for _, short := range []bool{false, true} {
		events := []string{}
		r := &attachTestReader{target: attachTestTarget(), events: &events}
		err := attachLongExact(longAttachOptions{id: 7}, attachTestDeps(r), attachTestWriter{&events, short, io.ErrClosedPipe})
		if err == nil || !reflect.DeepEqual(events, []string{"open", "get:7", "close", "host", "alive:2468", "prepare", "write"}) {
			t.Fatal("failed intent write reached interactive action", events, err)
		}
	}
	events := []string{}
	r := &attachTestReader{target: attachTestTarget(), events: &events}
	r.target.Workspace = "/fixture/\x1b\n雪"
	r.target.AccountID = "codex/\t\"fixture"
	var out bytes.Buffer
	if err := attachLongExact(longAttachOptions{id: 7}, attachTestDeps(r), &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "attaching to \"/fixture/\\x1b\\n雪\" · \"codex/\\t\\\"fixture\"\n" {
		t.Fatalf("unsafe or truncated attempt %q", out.String())
	}
}

func TestLongAttachActualDispatcherBypassesBootstrap(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "absent-data")
	config := filepath.Join(root, "invalid.toml")
	if err := os.WriteFile(config, []byte("deliberately invalid = ["), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIQ_DATA_DIR", data)
	t.Setenv("AIQ_CONFIG", config)
	saved := longAttachRuntime
	defer func() { longAttachRuntime = saved }()
	calls := 0
	events := []string{}
	r := &attachTestReader{lookupErr: state.ErrLeaseTargetNotFound, events: &events}
	longAttachRuntime = func() longAttachDeps { calls++; return attachTestDeps(r) }
	for _, args := range [][]string{{"attach", "7", "--started="}, {"attach", ".", "--started=1"}, {"--backend=tmux", "attach", "7"}, {"--backend", "herdr", "attach", "7"}, {"attach", "PRIVATE-invalid"}} {
		if err := cmdLong(args); err == nil {
			t.Fatal("invalid dispatch accepted")
		}
	}
	if calls != 0 {
		t.Fatal("invalid syntax opened attach dependencies")
	}
	if err := cmdLong([]string{"attach", "7", "--started=123"}); err == nil {
		t.Fatal("missing selected lease accepted")
	}
	if calls != 1 || !reflect.DeepEqual(events, []string{"open", "get:7", "close"}) {
		t.Fatal("exact dispatcher did not use read-only route", calls, events)
	}
	if _, err := os.Stat(data); !os.IsNotExist(err) {
		t.Fatal("dispatch initialized data directory")
	}
}
