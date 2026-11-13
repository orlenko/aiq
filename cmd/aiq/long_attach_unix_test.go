//go:build unix

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/herdr"
	"github.com/orlenko/aiq/internal/state"
)

func TestLongAttachLegacyStandaloneNativeActualDispatch(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "aiq-legacy-attach-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	workspace, err = filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(workspace)
	data, bin := filepath.Join(root, "data"), filepath.Join(root, "bin")
	if err := os.MkdirAll(filepath.Join(data, "herdr"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIQ_DATA_DIR", data)
	t.Setenv("AIQ_CONFIG", filepath.Join(root, "fresh-config.toml"))
	t.Setenv("PATH", bin)
	t.Setenv("HC_TERMINAL_ID", "") // Force the saved endpoint, without real TTY/PID inspection.
	t.Setenv("HC_TERMINAL_SOCKET", "/invented/caller-owner")
	t.Setenv("HC_SOCKET", "/invented/caller-selector")
	t.Setenv("HC_SESSION", "invented-caller-session")
	t.Setenv("TMUX", "")
	marker := filepath.Join(root, "viewer-call")
	t.Setenv("GO_LEGACY_ATTACH_MARKER", marker)
	for name, script := range map[string]string{
		"git": "#!/bin/sh\nexit 1\n", // The invented workspace is intentionally not a repository.
		"hc":  "#!/bin/sh\nprintf '%s\\n' \"$1\" \"$2\" \"$HC_SOCKET\" \"${HC_TERMINAL_SOCKET-unset}\" \"${HC_TERMINAL_ID-unset}\" \"${HC_SESSION-unset}\" > \"$GO_LEGACY_ATTACH_MARKER\"\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}

	// Construct the on-disk standalone descriptor directly, independently of
	// production registration helpers. No lease or launch is registered.
	sum := sha256.Sum256([]byte(workspace))
	key := hex.EncodeToString(sum[:16])
	record := herdr.Record{Ref: "herdr:" + key + "-" + strings.Repeat("a", 32), Workspace: workspace, Socket: filepath.Join(root, "control"), RunnerPID: 123, PID: 2468, Generation: 7, State: "running", Core: herdr.Core{Socket: filepath.Join(root, "core"), ID: "fixture-standalone-core", Terminal: "t1b", PID: 456}}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	descriptor := filepath.Join(data, "herdr", key+".json")
	if err := os.WriteFile(descriptor, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	var controlCalls, coreCalls atomic.Int32
	var unexpected atomic.Bool
	serve := func(socket string, reply func(string) any) {
		t.Helper()
		listener, err := net.Listen("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				func() {
					defer conn.Close()
					conn.SetDeadline(time.Now().Add(time.Second))
					var req struct {
						ID     int    `json:"id"`
						Method string `json:"method"`
					}
					if json.NewDecoder(conn).Decode(&req) != nil {
						unexpected.Store(true)
						return
					}
					json.NewEncoder(conn).Encode(map[string]any{"id": req.ID, "result": reply(req.Method)})
				}()
			}
		}()
		t.Cleanup(func() { listener.Close(); wg.Wait() })
	}
	serve(record.Socket, func(method string) any {
		controlCalls.Add(1)
		if method != "info" {
			unexpected.Store(true)
		}
		return map[string]any{"Record": record}
	})
	serve(record.Core.Socket, func(method string) any {
		coreCalls.Add(1)
		switch method {
		case "hello":
			return map[string]any{"generation": 1, "core": map[string]any{"id": "fixture-standalone-core"}}
		case "terminal.get":
			return map[string]any{"address": "t1b", "state": "live", "pid": 456}
		default:
			unexpected.Store(true)
			return nil
		}
	})
	previous := longAttachRuntime
	t.Cleanup(func() { longAttachRuntime = previous })
	exactCalls := 0
	longAttachRuntime = func() longAttachDeps { exactCalls++; panic("legacy dispatch reached exact-ID runtime") }
	for i, args := range [][]string{{"attach"}, {"attach", "."}} {
		if err := cmdLong(args); err != nil {
			t.Fatalf("standalone legacy dispatch %q: %v", args, err)
		}
		got, err := os.ReadFile(marker)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "attach\nt1b\n"+record.Core.Socket+"\nunset\nunset\nunset\n" {
			t.Fatalf("legacy viewer argv/context %q", got)
		}
		if controlCalls.Load() != int32(2*(i+1)) || coreCalls.Load() != int32(2*(i+1)) || unexpected.Load() {
			t.Fatal("legacy route changed its observation/verification protocol")
		}
		st, err := state.OpenReadOnly(filepath.Join(data, "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		leases, listErr := st.ListLeases()
		closeErr := st.Close()
		if listErr != nil || closeErr != nil || len(leases) != 0 {
			t.Fatal("standalone attachment required or created a lease")
		}
		after, err := os.ReadFile(descriptor)
		if err != nil || !bytes.Equal(after, encoded) {
			t.Fatal("legacy attachment rewrote standalone ownership")
		}
	}
	if exactCalls != 0 {
		t.Fatal("legacy route consulted exact-ID runtime")
	}
}
