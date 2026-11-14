package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/daemon"
	"github.com/orlenko/aiq/internal/navclient"
	"github.com/orlenko/aiq/internal/navtoken"
	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/state"
)

// Only the private native driver overlays this guard AND daemon identity,
// hostname, loops and shutdown dependencies. Environment flags alone cannot run
// this fixture against native identity, services, accounts or provider loops.
const navAppFixtureIsolated = false

func TestNavigatorAppBrowserFixture(t *testing.T) {
	if !navAppFixtureIsolated {
		t.Skip("native application fixture NOT RUN without isolated dependency overlay")
	}
	ready, stop := os.Getenv("AIQ_TEST_NAV_APP_READY"), os.Getenv("AIQ_TEST_NAV_APP_STOP")
	if ready == "" || stop == "" {
		t.Fatal("private native driver configuration absent")
	}
	x := newNavAppFixture(t, 1, navAppFixtureQuote)
	t.Setenv("AIQ_DATA_DIR", filepath.Dir(x.asksPath))
	t.Setenv("AIQ_CONFIG", filepath.Join(x.root, "config.toml"))
	t.Setenv("HOME", filepath.Join(x.root, "real-home"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(x.root, "real-claude"))
	t.Setenv("CODEX_HOME", filepath.Join(x.root, "real-codex"))
	for _, dir := range []string{os.Getenv("HOME"), os.Getenv("CODEX_HOME")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	probe, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listen := probe.Addr().String()
	probe.Close()
	cfg := config.Default()
	cfg.UI.Navigator = true
	cfg.Daemon.Listen = listen
	cfg.Updates.Enabled = false
	if err = config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(os.Getenv("AIQ_DATA_DIR"), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	a := &app{cfg: cfg, st: store, pool: &pool.Pool{Cfg: cfg, St: store, KeepLeases: true}}
	done := make(chan struct{})
	go func() { defer close(done); _ = runDaemonServer(a, log.New(io.Discard, "", 0)) }()
	t.Cleanup(func() {
		_ = os.WriteFile(stop, []byte("stop invented fixture"), 0600)
		select {
		case <-done:
		case <-time.After(4 * time.Second):
			t.Error("owned daemon fixture did not stop")
		}
	})
	var record navtoken.Record
	deadline := time.Now().Add(5 * time.Second)
	for {
		record, err = navtoken.Read(filepath.Join(os.Getenv("AIQ_DATA_DIR"), "ui-token"))
		if err == nil && record.Status() == navtoken.On {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("owned navigator did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	client, err := navclient.New(record, daemon.NavigatorBuild())
	if err != nil {
		t.Fatal(err)
	}
	control, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stopDriver := make(chan struct{}, 1)
	server := &http.Server{ReadHeaderTimeout: time.Second, WriteTimeout: 15 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		switch r.URL.Path {
		case "/launch":
			var file string
			err := runUI(r.Context(), []string{x.work}, runtime.GOOS, io.Discard, uiDependencies{identity: func(context.Context) (string, error) { return strings.Repeat("a", 64), nil }, open: func(_ context.Context, p string) (bool, error) { file = p; return true, nil }})
			if err != nil || file == "" {
				w.WriteHeader(503)
				io.WriteString(w, `{"error":"fixture_launch_unavailable"}`)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"file": file})
		case "/revoke":
			if client.Revoke(r.Context()) != nil {
				w.WriteHeader(503)
				return
			}
			io.WriteString(w, `{}`)
		case "/stop-daemon":
			_ = os.WriteFile(stop, []byte("stop invented daemon"), 0600)
			select {
			case <-done:
				io.WriteString(w, `{}`)
			case <-r.Context().Done():
			}
		case "/stop":
			select {
			case stopDriver <- struct{}{}:
			default:
			}
			io.WriteString(w, `{}`)
		default:
			w.WriteHeader(404)
		}
	})}
	go server.Serve(control)
	defer server.Close()
	port, _ := record.Port()
	raw, _ := json.Marshal(map[string]any{"port": port, "control": control.Addr().String(), "build": daemon.NavigatorBuild(), "directory": x.work, "id": fullIDFixtureID, "anchor": x.at.UnixMilli()})
	f, err := os.OpenFile(ready, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal("private ready file")
	}
	_, writeErr := f.Write(raw)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatal("private ready write")
	}
	select {
	case <-stopDriver:
	case <-time.After(90 * time.Second):
		t.Fatal("private browser driver did not complete")
	}
}
