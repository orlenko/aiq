package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/daemon"
	"github.com/orlenko/aiq/internal/navauth"
)

func TestUIAdmissionNoEffects(t *testing.T) {
	poison := uiDependencies{load: func() (*config.Config, error) { t.Fatal("config before admission"); return nil, nil }, data: func() string { t.Fatal("paths before admission"); return "" }}
	for _, platform := range []string{"windows", "linux", "darwin", "plan9"} {
		for _, args := range [][]string{{"--help"}, {"-h"}, {"--bad"}, {"--info", "--revoke"}, {"/invented\nsecret"}, {"--help", "extra"}} {
			var out bytes.Buffer
			e := runUI(context.Background(), args, platform, &out, poison)
			if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
				if e != nil || !strings.Contains(out.String(), uiUsage) {
					t.Fatal("pure help")
				}
			} else if e == nil || e.Error() != uiUsage {
				t.Fatal("fixed no-echo admission")
			}
		}
		if platform != "linux" && platform != "darwin" {
			if e := runUI(context.Background(), nil, platform, io.Discard, poison); e != errUIPlatform {
				t.Fatal("platform backstop")
			}
		}
	}
}

func TestUIActualDispatchChild(t *testing.T) {
	raw := os.Getenv("AIQ_TEST_UI_ARGS")
	if raw == "" {
		return
	}
	var args []string
	if json.Unmarshal([]byte(raw), &args) != nil {
		t.Fatal("fixture args")
	}
	if platform := os.Getenv("AIQ_TEST_UI_OS"); platform != "" {
		goos = platform
	}
	os.Args = append([]string{"aiq"}, args...)
	main()
}

func TestUIActualDispatchAvoidsLauncherLogs(t *testing.T) {
	for _, row := range []struct {
		name, platform, cfg string
		args                []string
		success             bool
	}{
		{"help-poison", "linux", "[[[", []string{"ui", "--help"}, true},
		{"windows-help-poison", "windows", "[[[", []string{"ui", "--help"}, true},
		{"windows-refusal", "windows", "[[[", []string{"ui", "--info"}, false},
		{"control-refusal", "windows", "[[[", []string{"ui", "--bad\nINVENTED_PRIVATE"}, false},
		{"late-config-failure", "linux", "[[[", []string{"ui", "--info"}, false},
		{"legacy-ui-builtin", "linux", "[launchers.ui]\nprovider='claude'\ncommand='/invented/never-run'\n", []string{"ui"}, false},
		{"legacy-ui-version", "linux", "[launchers.ui]\nprovider='claude'\ncommand='/invented/never-run'\n", []string{"version"}, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			root := t.TempDir()
			cfg := filepath.Join(root, "config.toml")
			data := filepath.Join(root, "data")
			if e := os.WriteFile(cfg, []byte(row.cfg), 0600); e != nil {
				t.Fatal(e)
			}
			args, _ := json.Marshal(row.args)
			cmd := exec.Command(os.Args[0], "-test.run=^TestUIActualDispatchChild$")
			cmd.Env = append(os.Environ(), "AIQ_CONFIG="+cfg, "AIQ_DATA_DIR="+data, "AIQ_TEST_UI_ARGS="+string(args), "AIQ_TEST_UI_OS="+row.platform)
			out, e := cmd.CombinedOutput()
			if (e == nil) != row.success {
				t.Fatalf("dispatch outcome: %v %s", e, out)
			}
			if bytes.Contains(out, []byte("INVENTED_PRIVATE")) || bytes.Contains(out, []byte(cfg)) || bytes.Contains(out, []byte("/invented/never-run")) {
				t.Fatal("raw input/config error escaped")
			}
			if _, e = os.Stat(data); !os.IsNotExist(e) {
				t.Fatal("admission/error wrote data or launch log")
			}
		})
	}
}

type uiTestPeer struct {
	data            string
	deps            uiDependencies
	issues, revokes atomic.Int32
	auth            *navauth.State
}

func newUITestPeer(t *testing.T, mode string) *uiTestPeer {
	t.Helper()
	data, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Chmod(data, 0700); e != nil {
		t.Fatal(e)
	}
	p := &uiTestPeer{data: data}
	s := httptest.NewUnstartedServer(nil)
	port := s.Listener.Addr().(*net.TCPAddr).Port
	key := [32]byte{51, 62, 73, 84, 95, 16, 27, 38}
	build := daemon.NavigatorBuild()
	if mode == "wrong-build" {
		build = "invented-wrong-build"
	}
	binding := navauth.Binding{Port: port, Build: build}
	p.auth, e = navauth.New(key, binding)
	if e != nil {
		t.Fatal(e)
	}
	identity := strings.Repeat("a", 64)
	s.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Header().Set("X-Aiq-Daemon", identity)
			w.Header().Set("X-Aiq-Nav", "pid=123; start=-123; nav=1")
			io.WriteString(w, "ok\n")
			return
		}
		if r.URL.Path == "/api/nav/hello" {
			challenge, err := navauth.Decode32(r.Header.Get("X-Aiq-Challenge"))
			if err != nil {
				t.Error("challenge")
				w.WriteHeader(401)
				return
			}
			now, _ := p.auth.Now()
			body, _ := json.Marshal(map[string]any{"nav_version": 1, "build": build, "server_time_ms": now})
			mac, _ := p.auth.SignHello(challenge, 200, body)
			w.Header().Set("X-Aiq-Hello-Proof", navauth.Encode32(mac))
			w.Write(body)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 4097))
		proof, err := navauth.ParseProof(r.Header.Get("X-Aiq-Proof"))
		if err != nil {
			t.Error("proof parse")
			w.WriteHeader(401)
			return
		}
		facts, err := p.auth.Admit(r.Method, r.URL.Path, proof, body)
		if err != nil {
			t.Error("proof admission")
			w.WriteHeader(401)
			return
		}
		var reply []byte
		switch r.URL.Path {
		case "/api/nav/info":
			dir := data
			if mode == "relative-info" {
				dir = "relative"
			}
			if mode == "foreign-info" {
				dir = "/invented/foreign"
			}
			reply, _ = json.Marshal(map[string]string{"root": "", "data_dir": dir, "hostname": "invented\x1b[31mhost"})
		case "/api/nav/login":
			if string(body) != `{"action":"issue"}` {
				t.Error("issue body")
			}
			p.issues.Add(1)
			code, err := p.auth.Issue(facts)
			if err != nil {
				t.Error("issue")
			}
			if mode == "lost-issue" {
				c, _, _ := w.(http.Hijacker).Hijack()
				c.Close()
				return
			}
			reply, _ = json.Marshal(map[string]string{"code": code})
		case "/api/nav/revoke":
			p.revokes.Add(1)
			p.auth.Revoke(facts)
			reply = []byte(`{"ok":true}`)
		default:
			t.Error("unexpected request")
			w.WriteHeader(404)
			return
		}
		mac, err := p.auth.SignResponse(facts, 200, reply)
		if err != nil {
			t.Error("response proof")
		}
		w.Header().Set("X-Aiq-Response-Proof", navauth.Encode32(mac))
		w.Write(reply)
	})
	s.Start()
	t.Cleanup(s.Close)
	t.Cleanup(p.auth.Close)
	record := fmt.Sprintf("on 123 -123 %d %s\n", port, navauth.Encode32(key))
	if e = os.WriteFile(filepath.Join(data, "ui-token"), []byte(record), 0600); e != nil {
		t.Fatal(e)
	}
	cfg := config.Default()
	cfg.UI.Navigator = true
	cfg.Daemon.Listen = s.Listener.Addr().String()
	p.deps = uiDependencies{load: func() (*config.Config, error) { return cfg, nil }, data: func() string { return data }, cwd: func() (string, error) { return "/invented/project &é", nil }, identity: func(context.Context) (string, error) { return identity, nil }, open: func(context.Context, string) (bool, error) { t.Error("unexpected opener"); return false, errUIOpen }}
	return p
}

func TestUIAuthenticatedModesAndDirectoryPin(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("supported native private-file platforms only")
	}
	for _, mode := range []string{"info", "revoke", "wrong-build", "relative-info", "foreign-info", "relative-config", "dot-config", "trailing-config"} {
		t.Run(mode, func(t *testing.T) {
			p := newUITestPeer(t, mode)
			args := []string{"--info"}
			if mode == "revoke" {
				args = []string{"--revoke"}
			}
			switch mode {
			case "relative-config":
				p.deps.data = func() string { return "relative" }
			case "dot-config":
				p.deps.data = func() string { return p.data + "/./" }
			case "trailing-config":
				p.deps.data = func() string { return p.data + "/" }
			}
			var out bytes.Buffer
			e := runUI(context.Background(), args, runtime.GOOS, &out, p.deps)
			if mode == "info" {
				if e != nil || out.String() != "root: \"\"\ndata_dir: "+strconv.Quote(p.data)+"\nhostname: \"invented\\x1b[31mhost\"\n" {
					t.Fatalf("authenticated quoting: %v %q", e, out.String())
				}
			} else if mode == "revoke" {
				if e != nil || p.revokes.Load() != 1 {
					t.Fatal("revoke")
				}
			} else if e == nil || out.Len() != 0 {
				t.Fatal("unpinned output")
			}
			if p.issues.Load() != 0 {
				t.Fatal("read/revoke consumed code")
			}
			if _, e = os.Stat(filepath.Join(p.data, "ui-launch")); !os.IsNotExist(e) {
				t.Fatal("read/revoke created/pruned handoffs")
			}
		})
	}
}

func TestUILaunchOneIssueAndUncertainRetention(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("supported private-file platforms only")
	}
	for _, mode := range []string{"success", "start-failed", "started-failed", "lost-issue", "unsafe-data", "oversized-directory", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			p := newUITestPeer(t, mode)
			var name string
			var opens int
			p.deps.open = func(ctx context.Context, path string) (bool, error) {
				opens++
				name = path
				body, e := os.ReadFile(path)
				if e != nil || !bytes.Contains(body, []byte("http://127.0.0.1:")) || bytes.Contains(body, []byte("<script")) {
					t.Fatal("incomplete handoff at opener")
				}
				info, e := os.Stat(path)
				if e != nil || info.Mode().Perm() != 0600 {
					t.Fatal("handoff mode")
				}
				if mode == "start-failed" {
					return false, errors.New("invented private failure")
				}
				if mode == "started-failed" {
					return true, errors.New("invented private failure")
				}
				return true, nil
			}
			if mode == "unsafe-data" {
				os.Chmod(p.data, 0755)
			}
			if mode == "oversized-directory" {
				p.deps.cwd = func() (string, error) { return "/" + strings.Repeat("é", 1800), nil }
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "canceled" {
				cancel()
			}
			var out bytes.Buffer
			e := runUI(ctx, nil, runtime.GOOS, &out, p.deps)
			if mode == "success" {
				if e != nil {
					t.Fatal(e)
				}
			} else if e == nil || strings.Contains(e.Error(), "private failure") {
				t.Fatal("fixed failure")
			}
			wantIssue := int32(1)
			wantOpen := 1
			if mode == "unsafe-data" || mode == "oversized-directory" || mode == "canceled" {
				wantIssue = 0
				wantOpen = 0
			}
			if mode == "lost-issue" {
				wantOpen = 0
			}
			if p.issues.Load() != wantIssue || opens != wantOpen {
				t.Fatal("issue replay or early side effect")
			}
			files, _ := filepath.Glob(filepath.Join(p.data, "ui-launch", "*.html"))
			if mode == "success" || mode == "started-failed" {
				if len(files) != 1 || files[0] != name {
					t.Fatal("uncertain/success document removed")
				}
			} else if len(files) != 0 {
				t.Fatal("pre-start failure residue")
			}
		})
	}
}

func TestUIOpenerUsesOnlyPrivatePathAndNeverKills(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("native Unix opener fixture")
	}
	for _, mode := range []string{"success", "nonzero", "held", "not-started"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			helper := filepath.Join(root, "xdg-open")
			arg := filepath.Join(root, "invented.html")
			record := filepath.Join(root, "record")
			release := filepath.Join(root, "release")
			exited := filepath.Join(root, "exited")
			body := "#!/bin/sh\ntrap 'printf signaled > \"$UI_TEST_SIGNAL\"' HUP INT TERM\nprintf '%s\\n%s\\n%s\\n' \"$#\" \"$1\" \"${BROWSER-unset}\" > \"$UI_TEST_RECORD\"\n"
			if mode == "held" {
				body += "while [ ! -f \"$UI_TEST_RELEASE\" ]; do /bin/sleep 0.01; done\n"
			}
			body += "printf done > \"$UI_TEST_EXITED\"\n"
			if mode == "nonzero" {
				body += "exit 1\n"
			}
			if mode != "not-started" {
				if e := os.WriteFile(helper, []byte(body), 0700); e != nil {
					t.Fatal(e)
				}
			}
			t.Setenv("PATH", root)
			t.Setenv("BROWSER", "invented-secret-browser-command")
			t.Setenv("UI_TEST_RECORD", record)
			t.Setenv("UI_TEST_RELEASE", release)
			t.Setenv("UI_TEST_EXITED", exited)
			t.Setenv("UI_TEST_SIGNAL", filepath.Join(root, "signal"))
			defer os.WriteFile(release, []byte("release"), 0600)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if mode == "held" {
				ctx, cancel = context.WithTimeout(context.Background(), 150*time.Millisecond)
				defer cancel()
			}
			started, e := openUIFile(ctx, "linux", arg)
			if mode == "success" {
				if e != nil || !started {
					t.Fatal("dispatch")
				}
			} else if e != errUIOpen || started != (mode != "not-started") {
				t.Fatal("uncertain start classification")
			}
			if mode == "not-started" {
				return
			}
			b, e := os.ReadFile(record)
			if e != nil || string(b) != "1\n"+arg+"\nunset\n" {
				t.Fatal("helper received URL/code/options or inherited BROWSER")
			}
			if os.Getenv("BROWSER") != "invented-secret-browser-command" {
				t.Fatal("global environment mutated")
			}
			if mode == "held" {
				if _, e = os.Stat(exited); !os.IsNotExist(e) {
					t.Fatal("held helper exited before test release")
				}
				if _, e = os.Stat(filepath.Join(root, "signal")); !os.IsNotExist(e) {
					t.Fatal("helper received cancellation signal")
				}
				os.WriteFile(release, []byte("release"), 0600)
				deadline := time.Now().Add(2 * time.Second)
				for {
					if _, e = os.Stat(exited); e == nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("owned helper did not finish after test release")
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
		})
	}
}
