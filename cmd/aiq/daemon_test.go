package main

import (
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orlenko/aiq/internal/daemon"
	"github.com/orlenko/aiq/internal/filelock"
)

// A refused address stops an install, and must not stop the daemon itself: the
// service runs `aiq daemon run`, which has to reach the mode that keeps polling and
// supervising without serving HTTP.
func TestDaemonListenRefusedOnInstallOnly(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("AIQ_DATA_DIR", filepath.Join(root, "data"))
	cfg := filepath.Join(root, "config.toml")
	t.Setenv("AIQ_CONFIG", cfg)
	body := "[daemon]\nlisten = \"0.0.0.0:7379\"\n[telemetry]\nclaude_statusline = false\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	installed, ran := false, false
	oldInstall, oldRun := installDaemonService, runDaemonServer
	defer func() { installDaemonService, runDaemonServer = oldInstall, oldRun }()
	installDaemonService = func(string) (string, error) { installed = true; return "", nil }
	runDaemonServer = func(*app, *log.Logger) error { ran = true; return nil }

	err := cmdDaemon([]string{"install"})
	if err == nil || installed {
		t.Fatalf("install was not refused: err %v, installed %v", err, installed)
	}
	if !strings.Contains(err.Error(), "keeps the address it started with") || !strings.Contains(err.Error(), cfg) {
		t.Errorf("the refusal does not warn about an older daemon: %v", err)
	}
	if err := cmdDaemon([]string{"run"}); err != nil || !ran {
		t.Fatalf("run did not reach the server: err %v, ran %v", err, ran)
	}
}

// While the address is refused, the plan must not lead with an install that is
// certain to fail, and the only step is the one that fixes the address.
func TestDoctorPutsTheListenFixFirstWhenRefused(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("AIQ_DATA_DIR", filepath.Join(root, "data"))
	cfg := filepath.Join(root, "config.toml")
	t.Setenv("AIQ_CONFIG", cfg)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, ".claude"))
	if err := os.WriteFile(cfg, []byte("[daemon]\nlisten = \"0.0.0.0:7379\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := openApp()
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	listen := 0
	for _, f := range a.doctorChecks().fixes {
		if f.command == "aiq daemon install" {
			t.Errorf("the plan has the refused install: %q", f.title)
		}
		if f.key == "daemon:listen" {
			listen++
		}
	}
	if listen != 1 {
		t.Errorf("%d listen fixes, want 1", listen)
	}
}

// Doctor names the lock when it is why a daemon cannot be there, or when the daemon
// that answers is not the one for this data directory.
func TestDoctorReportsTheLockBesideAnAcceptedAddress(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	t.Setenv("HOME", root)
	t.Setenv("AIQ_DATA_DIR", data)
	cfg := filepath.Join(root, "config.toml")
	t.Setenv("AIQ_CONFIG", cfg)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, ".claude"))
	fixes := func(listen string) map[string]bool {
		t.Helper()
		if err := os.WriteFile(cfg, []byte("[daemon]\nlisten = \""+listen+"\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		a, err := openApp()
		if err != nil {
			t.Fatal(err)
		}
		defer a.close()
		keys := map[string]bool{}
		for _, f := range a.doctorChecks().fixes {
			keys[f.key] = true
		}
		return keys
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Aiq-Daemon", daemon.Identity())
		w.Write([]byte("ok\n"))
	}))
	defer srv.Close()
	if !fixes(strings.TrimPrefix(srv.URL, "http://"))["daemon:datadir"] {
		t.Error("an identified daemon with no lock here was not reported")
	}

	if os.Geteuid() == 0 {
		return
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	silent := strings.TrimPrefix(closed.URL, "http://")
	closed.Close()
	lock := filepath.Join(data, "daemon.lock")
	if err := os.WriteFile(lock, nil, 0o000); err != nil {
		t.Fatal(err)
	}
	got := fixes(silent)
	if !got["daemon:lock"] {
		t.Error("an unreadable lock with no daemon answering was not reported")
	}
	if got["daemon"] {
		t.Error("the plan offers a restart that needs the lock it cannot open")
	}
}

func TestDaemonStateSaysWhatIsRunning(t *testing.T) {
	data := t.TempDir()
	t.Setenv("AIQ_DATA_DIR", data)
	t.Setenv("HOME", t.TempDir())
	answerer := func(identity string) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if identity != "" {
				w.Header().Set("X-Aiq-Daemon", identity)
			}
			w.Write([]byte("ok\n"))
		}))
		t.Cleanup(srv.Close)
		return strings.TrimPrefix(srv.URL, "http://")
	}
	current, older, elsewhere := answerer(daemon.Identity()), answerer(""), answerer("another-machine")
	// A daemon with a navigator adds its own header and leaves the identity as it was.
	navigator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Aiq-Daemon", daemon.Identity())
		w.Header().Set("X-Aiq-Nav", "pid=1; start=2; nav=1")
		w.Write([]byte("ok\n"))
	}))
	t.Cleanup(navigator.Close)
	closed := httptest.NewServer(http.NotFoundHandler())
	silent := strings.TrimPrefix(closed.URL, "http://")
	closed.Close()

	check := func(when, listen, want string) {
		t.Helper()
		if got := daemonState(listen); !strings.Contains(got, want) {
			t.Errorf("%s: %q does not say %q", when, got, want)
		}
	}
	check("refused, no lock", "0.0.0.0:7379", "no daemon holds the lock")
	check("refused, no lock", "0.0.0.0:7379", "An older daemon would not show")
	check("nothing there", silent, "no daemon answers")
	check("this version, no lock here", current, "running at http://")
	check("this version, no lock here", current, "another data directory")
	check("older, no lock", older, "does not identify")
	check("a tunnel to another machine", elsewhere, "does not identify")
	check("a tunnel to another machine", elsewhere, "tunnel")

	f, err := os.OpenFile(filepath.Join(data, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := filelock.TryExclusive(f); err != nil {
		t.Fatal(err)
	}
	check("refused, locked", "0.0.0.0:7379", "a daemon holds the lock")
	check("this version, locked", current, "running at http://")
	check("this version with a navigator, locked", strings.TrimPrefix(navigator.URL, "http://"), "running at http://")
	check("older beside a lock", older, "does not identify")
	check("silent, locked", silent, "none answers")
	check("silent, locked", silent, "one is polling and supervising")
	check("silent, locked", silent, "lsof -nP -iTCP:")
	check("older beside a lock", older, "one is polling and supervising")

	if os.Geteuid() != 0 {
		f.Close()
		if err := os.Chmod(filepath.Join(data, "daemon.lock"), 0o000); err != nil {
			t.Fatal(err)
		}
		check("refused, unreadable", "0.0.0.0:7379", "does not know")
		check("silent, unreadable", silent, "does not know")
		check("this version, unreadable", current, "running at http://")
	}
}

// A daemon that holds the lock but cannot bind its port polls and supervises (#38).
// Doctor says so, offers to free the port instead of a restart that would meet the
// same port.
func TestDoctorReportsADaemonThatCannotServe(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	t.Setenv("HOME", root)
	t.Setenv("AIQ_DATA_DIR", data)
	cfg := filepath.Join(root, "config.toml")
	t.Setenv("AIQ_CONFIG", cfg)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, ".claude"))
	closed := httptest.NewServer(http.NotFoundHandler())
	silent := strings.TrimPrefix(closed.URL, "http://")
	closed.Close()
	body := "[daemon]\nlisten = \"" + silent + "\"\n[telemetry]\nclaude_statusline = false\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(data, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := filelock.TryExclusive(f); err != nil {
		t.Fatal(err)
	}
	a, err := openApp()
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	r := a.doctorChecks()
	said := false
	for _, c := range r.checks {
		if c.ok && strings.Contains(c.text, "polling and supervising without its status page") {
			said = true
		}
		// The service is not installed here, so installing it stays a fix of its own.
		if strings.HasPrefix(c.text, "daemon answering on") && (c.fix == nil || c.fix.key != "daemon:port") {
			t.Errorf("%q has fix %+v, want daemon:port", c.text, c.fix)
		}
	}
	if !said {
		t.Error("doctor did not say a daemon polls and supervises without its status page")
	}

	// The install fix, when it finds the same, says why nothing answers.
	defer func(saved func(string) (string, error)) { installDaemonService = saved }(installDaemonService)
	installDaemonService = func(string) (string, error) { return "", nil }
	err = a.doctorDaemonInstall()
	if err == nil || !strings.Contains(err.Error(), "polling and supervising") || !strings.Contains(err.Error(), "lsof -nP -iTCP:") {
		t.Errorf("the install wait said %v", err)
	}
}
