//go:build darwin || linux

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/herdr"
	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/state"
)

func TestAlphanumericNativeListAndAttach(t *testing.T) {
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	ws := t.TempDir()
	st, err := state.Open(paths.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.Default()
	a := &app{cfg: cfg, st: st, pool: &pool.Pool{Cfg: cfg, St: st}}
	r := nativeRecordFixture(t, ws)
	r.Core.Terminal = "t1b"
	r.Generation = 3
	r.State = "running"
	saveNativeFixture(t, r)
	_, mutations := nativeInfoFixture(t, r)
	listener, err := net.Listen("unix", r.Core.Socket)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			func() {
				defer c.Close()
				c.SetDeadline(time.Now().Add(time.Second))
				var q struct {
					ID     int
					Method string
					Params struct{ Terminal string }
				}
				if json.NewDecoder(c).Decode(&q) != nil {
					return
				}
				var result any
				switch q.Method {
				case "hello":
					result = map[string]any{"generation": 1, "core": map[string]any{"id": r.Core.ID}}
				case "terminal.get":
					if q.Params.Terminal != "t1b" {
						t.Error("core lookup lost exact saved address")
					}
					result = map[string]any{"address": "t1b", "state": "live", "pid": r.Core.PID}
				default:
					t.Errorf("unexpected core operation %q", q.Method)
				}
				json.NewEncoder(c).Encode(map[string]any{"id": q.ID, "result": result})
			}()
		}
	}()
	t.Cleanup(func() { listener.Close(); wg.Wait() })
	out, err := stdoutOf(t, a.longList)
	if err != nil || !strings.Contains(out, "herdr/t1b") || !strings.Contains(out, ws) {
		t.Fatal("CLI list lost letter-bearing address", err)
	}
	stubDir := t.TempDir()
	capture := filepath.Join(stubDir, "capture")
	script := "#!/bin/sh\nprintf '%s\\n' \"$#\" \"$1\" \"$2\" \"$HC_SOCKET\" >> \"$GO_HERDR_ATTACH_CAPTURE\"\n"
	if err := os.WriteFile(filepath.Join(stubDir, "hc"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GO_HERDR_ATTACH_CAPTURE", capture)
	t.Setenv("HC_SOCKET", filepath.Join(ws, "unrelated-caller-core"))
	t.Setenv("HC_TERMINAL_ID", "t9")
	stale := r
	stale.Generation--
	if err := herdr.Attach(stale.Target()); !errors.Is(err, herdr.ErrGenerationChanged) {
		t.Fatal("saved generation guard lost", err)
	}
	if _, err := os.Stat(capture); !os.IsNotExist(err) {
		t.Fatal("stale generation reached attach command")
	}
	if err := herdr.Attach(r.Target()); err != nil {
		t.Fatal(err)
	}
	recorded, err := os.ReadFile(capture)
	if err != nil || string(recorded) != fmt.Sprintf("2\nattach\nt1b\n%s\n", r.Core.Socket) {
		t.Fatal("attach did not preserve exact address/socket or ran another command", err)
	}
	if err := os.Rename(r.Socket, r.Socket+".offline"); err != nil {
		t.Fatal(err)
	}
	out, err = stdoutOf(t, a.longList)
	if err != nil || !strings.Contains(out, "herdr/t1b") || !strings.Contains(out, "unknown") {
		t.Fatal("unreachable alphanumeric ownership disappeared", err)
	}
	if mutations.Load() != 0 {
		t.Fatal("list or attach mutated the native runner")
	}
	launches, err := st.ListLaunches("", nil, nil)
	if err != nil || len(launches) != 0 {
		t.Fatal("list or attach started an extra provider", err)
	}
}
