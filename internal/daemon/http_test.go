package daemon

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/filelock"
	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/state"
)

func TestCheckListen(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:7379", "[::1]:7379"} {
		if err := CheckListen(addr); err != nil {
			t.Errorf("%s: %v", addr, err)
		}
	}
	for _, addr := range []string{"", "localhost:7379", "0.0.0.0:7379", "[::]:7379", "192.0.2.5:7379", "127.0.0.1", "127.0.0.1:0", "127.0.0.1:x", "127.0.0.1:70000", "example.com:7379", "127.0.0.1:+7379", "[127.0.0.1]:7379", "127.0.0.1:07379"} {
		if CheckListen(addr) == nil {
			t.Errorf("%q was accepted", addr)
		}
	}
}

func TestClientsDoNotContactARefusedAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a request reached %s", r.URL)
	}))
	defer srv.Close()
	bad := "localhost:" + srv.URL[len("http://127.0.0.1:"):]
	Notify(bad, "a/b")
	if Alive(bad) {
		t.Error("a refused address counted as alive")
	}
}

func TestLocalOnly(t *testing.T) {
	const host = "127.0.0.1:7379"
	h := localOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	for _, c := range []struct {
		name, peer, host, origin, method, site string
		want                                   int
	}{
		{"aiq's own request", "127.0.0.1:50000", host, "", "POST", "", 204},
		{"the status page", "127.0.0.1:50000", host, "http://" + host, "POST", "", 204},
		{"ipv6 loopback peer", "[::1]:50000", "[::1]:7379", "http://[::1]:7379", "GET", "", 204},
		{"a forwarded local port", "127.0.0.1:50000", "127.0.0.1:8000", "http://127.0.0.1:8000", "GET", "", 204},
		{"a website's POST", "127.0.0.1:50000", host, "https://example.com", "POST", "", 403},
		{"a sandboxed frame", "127.0.0.1:50000", host, "null", "POST", "", 403},
		{"another local port's page", "127.0.0.1:50000", host, "http://127.0.0.1:3000", "POST", "", 403},
		{"the same host over https", "127.0.0.1:50000", host, "https://" + host, "POST", "", 403},
		{"a rebound name", "127.0.0.1:50000", "evil.example:7379", "", "GET", "", 403},
		{"a rebound name with its own origin", "127.0.0.1:50000", "evil.example:7379", "http://evil.example:7379", "GET", "", 403},
		{"a network peer", "192.0.2.5:50000", host, "", "GET", "", 403},
		{"a network peer naming localhost", "192.0.2.5:50000", "localhost:7379", "", "GET", "", 403},
		{"an unparseable peer", "somewhere", host, "", "GET", "", 403},
		{"a cross-site POST with no Origin", "127.0.0.1:50000", host, "", "POST", "cross-site", 403},
		{"a same-site POST with no Origin", "127.0.0.1:50000", host, "", "POST", "same-site", 403},
		{"the page's own POST", "127.0.0.1:50000", host, "http://" + host, "POST", "same-origin", 204},
		{"a typed address", "127.0.0.1:50000", host, "", "GET", "none", 204},
		{"a framed page load", "127.0.0.1:50000", host, "", "GET", "cross-site", 204},
	} {
		r := httptest.NewRequest(c.method, "/api/poll", nil)
		r.RemoteAddr, r.Host = c.peer, c.host
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if c.site != "" {
			r.Header.Set("Sec-Fetch-Site", c.site)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s: %d, want %d", c.name, w.Code, c.want)
		}
		if c.want == 204 && (w.Header().Get("Cross-Origin-Resource-Policy") != "same-origin" || w.Header().Get("X-Frame-Options") != "DENY") {
			t.Errorf("%s: missing security headers", c.name)
		}
	}
}

// A refused address must leave the daemon running under its lock, with polling and
// supervision started: returning the error would make the service restart it forever.
func TestRunWithARefusedAddressKeepsRunningWithoutHTTP(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("AIQ_DATA_DIR", root)
	t.Setenv("PATH", os.Getenv("PATH")) // Run may reorder PATH
	cfg := config.Default()
	closed := httptest.NewServer(http.NotFoundHandler())
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(closed.URL, "http://"))
	closed.Close()
	cfg.Daemon.Listen = net.JoinHostPort("0.0.0.0", port)
	s := New(cfg, nil, log.New(io.Discard, "", 0))

	lockedAt := func(step string) {
		t.Helper()
		if held, err := LockHeld(); err != nil || !held {
			t.Errorf("no lock held at %s: held %v, err %v", step, held, err)
		}
	}
	defer func(loops func(*Server), wait func()) { startLoops, waitWithoutHTTP = loops, wait }(startLoops, waitWithoutHTTP)
	started, waited := false, false
	startLoops = func(*Server) { started = true; lockedAt("the start of the loops") }
	waitWithoutHTTP = func() { waited = true; lockedAt("the wait") }
	defer func() {
		if instanceLock != nil {
			instanceLock.Close()
			instanceLock = nil
		}
	}()
	if err := s.Run(); err != nil {
		t.Fatalf("Run returned %v", err)
	}
	if !started || !waited {
		t.Fatalf("Run did not reach the mode with loops and no HTTP: started %v, waited %v", started, waited)
	}
}

// daemonLike answers as a daemon would, with the given identity and machine name.
func daemonLike(t *testing.T, identity, hostname string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			if identity != "" {
				w.Header().Set(identityHeader, identity)
			}
			w.Write([]byte("ok\n"))
		case "/api/state":
			json.NewEncoder(w).Encode(map[string]any{"daemon": map[string]any{}, "view": pool.View{Hostname: hostname}})
		}
	}))
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	return port
}

// Two daemons would both supervise the same sessions, so a daemon that finds an
// older one on this machine starts nothing. Anything else on the port must not stop it.
func TestOlderDaemonAnswersOnlyForAnOlderDaemonHere(t *testing.T) {
	older := daemonLike(t, "", pool.Hostname())
	current := daemonLike(t, Identity(), pool.Hostname())
	elsewhere := daemonLike(t, "", "another-machine")

	wrongBody := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not ok\n"))
	}))
	defer wrongBody.Close()
	_, wrongPort, _ := net.SplitHostPort(strings.TrimPrefix(wrongBody.URL, "http://"))

	release := make(chan struct{})
	hung := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer hung.Close()
	defer close(release)
	_, hungPort, _ := net.SplitHostPort(strings.TrimPrefix(hung.URL, "http://"))

	target := older
	redirects := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:"+target+r.URL.Path, http.StatusFound)
	}))
	defer redirects.Close()
	_, redirectPort, _ := net.SplitHostPort(strings.TrimPrefix(redirects.URL, "http://"))

	for _, c := range []struct {
		name, port string
		want       bool
	}{
		{"older here", older, true},
		{"this version", current, false},
		{"tunnel to another machine", elsewhere, false},
		{"another program", wrongPort, false},
		{"no answer", hungPort, false},
		{"redirect", redirectPort, false},
	} {
		for _, host := range []string{"0.0.0.0", "127.0.0.1"} {
			if got := olderDaemonAnswers(net.JoinHostPort(host, c.port)); got != c.want {
				t.Errorf("%s on %s: %v, want %v", c.name, host, got, c.want)
			}
		}
	}
}

func TestOlderDaemonAnswersOnIPv6Loopback(t *testing.T) {
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Write([]byte("ok\n"))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"view": pool.View{Hostname: pool.Hostname()}})
	}))
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	defer srv.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	if !olderDaemonAnswers(net.JoinHostPort("0.0.0.0", port)) {
		t.Error("an older daemon on [::1] went unseen")
	}
}

// The state answer is what the older-daemon check reads, so it is tested against
// what the handler really sends.
func TestStateAnswerNamesThisMachineWhereTheCheckLooks(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("AIQ_DATA_DIR", root)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, ".claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, ".codex"))
	t.Setenv("COPILOT_HOME", filepath.Join(root, ".copilot"))
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := New(config.Default(), &pool.Pool{Cfg: config.Default(), St: st}, log.New(io.Discard, "", 0))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Write([]byte("ok\n"))
			return
		}
		s.handleState(w, r)
	}))
	defer srv.Close()
	if !olderDaemonAnswers(strings.TrimPrefix(srv.URL, "http://")) {
		t.Error("the real state answer did not read as this machine")
	}
}

// freeAfter stands in for the wait between binds. It runs during on each wait and
// frees the held port at wait number at, and it gives back how many waits ran.
func freeAfter(t *testing.T, held net.Listener, at int, during func()) *int {
	t.Helper()
	n := 0
	saved := listenRetryPause
	t.Cleanup(func() { listenRetryPause = saved })
	listenRetryPause = func() {
		n++
		if during != nil {
			during()
		}
		if n == at {
			held.Close()
		}
		if n > at+3 {
			t.Fatalf("still waiting after %d tries", n)
		}
	}
	return &n
}

// serveStops makes the status page's Serve return at once, so Run ends, and gives
// back the addresses it was asked to serve on.
func serveStops(t *testing.T) *[]string {
	t.Helper()
	var served []string
	saved := serveStatus
	t.Cleanup(func() { serveStatus = saved })
	serveStatus = func(_ *http.Server, ln net.Listener) error {
		served = append(served, ln.Addr().String())
		ln.Close()
		return errors.New("stopped by the test")
	}
	return &served
}

// The regression of #38: a daemon whose accepted address another program holds polls
// and supervises under its lock instead of exiting, which made the service restart it
// before supervision ever ran, and it serves once the port is free.
func TestRunSupervisesWhileThePortIsHeldAndServesOnceFree(t *testing.T) {
	useDataDir(t)
	defer releaseInstanceLock()
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	cfg := config.Default()
	cfg.Daemon.Listen = taken.Addr().String()
	logged := &lockedBuffer{}
	s := New(cfg, nil, log.New(logged, "", 0))

	served := serveStops(t)
	loops := 0
	defer func(saved func(*Server)) { startLoops = saved }(startLoops)
	startLoops = func(*Server) {
		loops++
		if held, err := LockHeld(); err != nil || !held {
			t.Errorf("the loops started without the lock: held %v, err %v", held, err)
		}
		if len(*served) != 0 {
			t.Error("the loops waited for the status page")
		}
	}
	waits := freeAfter(t, taken, 3, func() {
		if loops != 1 {
			t.Errorf("waiting for the port with the loops started %d times", loops)
		}
	})
	if err := s.Run(); err == nil || err.Error() != "stopped by the test" {
		t.Fatalf("Run returned %v", err)
	}
	if loops != 1 || *waits != 3 || len(*served) != 1 || (*served)[0] != cfg.Daemon.Listen {
		t.Fatalf("loops %d, waits %d, served %v", loops, *waits, *served)
	}
	out := logged.String()
	if n := strings.Count(out, "not serving the status page or API, but polling and supervising"); n != 1 {
		t.Errorf("the bind failure was logged %d times: %s", n, out)
	}
	if !strings.Contains(out, "can be bound now") {
		t.Errorf("the recovery was not logged: %s", out)
	}
}

// The loops start exactly once whichever way Run goes, and never beside an older
// daemon or a second daemon of this version.
func TestRunStartsTheLoopsOnceOnEveryPath(t *testing.T) {
	here := pool.Hostname() // looked up once, before any server reads it
	older := func(slowFirst bool) http.Handler {
		var mu sync.Mutex
		first := slowFirst
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			slow := first && r.URL.Path == "/healthz"
			if slow {
				first = false
			}
			mu.Unlock()
			if slow {
				time.Sleep(time.Second) // past the probe's 700 ms
			}
			if r.URL.Path == "/healthz" {
				w.Write([]byte("ok\n"))
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"view": pool.View{Hostname: here}})
		})
	}
	serveOn := func(t *testing.T, ln net.Listener, h http.Handler) {
		srv := &http.Server{Handler: h}
		go srv.Serve(ln)
		t.Cleanup(func() { srv.Close() })
	}
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, cfg *config.Config)
		loops int
		err   string // "" for nil
	}{
		{"refused address", func(t *testing.T, cfg *config.Config) {
			_, port, _ := net.SplitHostPort(freeLoopback(t))
			cfg.Daemon.Listen = net.JoinHostPort("0.0.0.0", port)
		}, 1, ""},
		{"free address", func(t *testing.T, cfg *config.Config) {
			cfg.Daemon.Listen = freeLoopback(t)
			freeAfter(t, nil, 0, func() { t.Error("waited with the port free") })
		}, 1, "stopped by the test"},
		{"held, then freed", func(t *testing.T, cfg *config.Config) {
			taken := holdPort(t)
			cfg.Daemon.Listen = taken.Addr().String()
			freeAfter(t, taken, 1, nil)
		}, 1, "stopped by the test"},
		{"held, then an older daemon answers", func(t *testing.T, cfg *config.Config) {
			taken := holdPort(t)
			cfg.Daemon.Listen = taken.Addr().String()
			freeAfter(t, taken, 2, func() { serveOn(t, taken, older(false)) })
		}, 1, "older"},
		{"an older daemon too slow for the first probe", func(t *testing.T, cfg *config.Config) {
			taken := holdPort(t)
			cfg.Daemon.Listen = taken.Addr().String()
			serveOn(t, taken, older(true))
			freeAfter(t, taken, 1, func() { t.Error("waited beside an older daemon") })
		}, 0, "older"},
		{"a second daemon while the port is held", func(t *testing.T, cfg *config.Config) {
			taken := holdPort(t)
			cfg.Daemon.Listen = taken.Addr().String()
			f, err := os.OpenFile(lockPath(), os.O_CREATE|os.O_RDWR, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { f.Close() })
			if err := filelock.TryExclusive(f); err != nil {
				t.Fatal(err)
			}
			saved := lockRetryPause
			t.Cleanup(func() { lockRetryPause = saved })
			lockRetryPause = func() {}
			freeAfter(t, taken, 1, func() { t.Error("a second daemon waited for the port") })
		}, 0, "another aiq daemon"},
	} {
		t.Run(c.name, func(t *testing.T) {
			useDataDir(t)
			defer releaseInstanceLock()
			defer func(loops func(*Server), wait func()) { startLoops, waitWithoutHTTP = loops, wait }(startLoops, waitWithoutHTTP)
			loops := 0
			startLoops = func(*Server) { loops++ }
			waitWithoutHTTP = func() {}
			serveStops(t)
			cfg := config.Default()
			c.setup(t, cfg)
			err := New(cfg, nil, log.New(io.Discard, "", 0)).Run()
			if c.err == "" && err != nil || c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)) {
				t.Errorf("Run returned %v, want %q", err, c.err)
			}
			if loops != c.loops {
				t.Errorf("the loops started %d times, want %d", loops, c.loops)
			}
		})
	}
}

// holdPort holds a loopback port of the test's own until the test ends.
func holdPort(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

func TestRunStopsBesideAnOlderDaemonWithoutTakingTheLock(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("AIQ_DATA_DIR", root)
	t.Setenv("PATH", os.Getenv("PATH"))
	cfg := config.Default()
	cfg.Daemon.Listen = net.JoinHostPort("0.0.0.0", daemonLike(t, "", pool.Hostname()))
	defer func(loops func(*Server)) { startLoops = loops }(startLoops)
	startLoops = func(*Server) { t.Error("the loops started beside an older daemon") }
	if err := New(cfg, nil, log.New(io.Discard, "", 0)).Run(); err == nil || !strings.Contains(err.Error(), "older") {
		t.Fatalf("Run returned %v", err)
	}
	if held, err := LockHeld(); held || err != nil {
		t.Fatalf("held %v, err %v", held, err)
	}
	if instanceLock != nil {
		t.Fatal("the lock was taken")
	}
	for _, path := range []string{navTokenPath(), navPortsPath()} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("%s was written: %v", path, err)
		}
	}
}

func TestClientsDoNotFollowARedirect(t *testing.T) {
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	followed := false
	away := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed = true }))
	defer away.Close()
	redirects := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, away.URL, http.StatusFound)
	}))
	defer redirects.Close()
	listen := strings.TrimPrefix(redirects.URL, "http://")
	if Notify(listen, "a/b") {
		t.Error("a redirect counted as taken")
	}
	if Alive(listen) {
		t.Error("a redirect counted as alive")
	}
	if followed {
		t.Error("a redirect was followed")
	}
}

func TestHealthRequiresTheAnswerAndReportsTheIdentity(t *testing.T) {
	for _, c := range []struct {
		identity   string
		identified bool
	}{{"", false}, {"another-machine", false}, {pool.Hostname(), false}, {Identity(), true}} {
		listen := net.JoinHostPort("127.0.0.1", daemonLike(t, c.identity, "h"))
		if alive, identified := Health(listen); !alive || identified != c.identified {
			t.Errorf("identity %q: alive %v, identified %v", c.identity, alive, identified)
		}
	}
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer other.Close()
	if Alive(strings.TrimPrefix(other.URL, "http://")) {
		t.Error("an empty answer counted as alive")
	}
}

func TestInstanceLockWaitsOutABriefProbe(t *testing.T) {
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	if err := os.WriteFile(lockPath(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	probe, err := os.Open(lockPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := filelock.TryShared(probe); err != nil {
		t.Fatal(err)
	}
	defer func(pause func()) { lockRetryPause = pause }(lockRetryPause)
	lockRetryPause = func() { probe.Close() }
	defer func() {
		if instanceLock != nil {
			instanceLock.Close()
			instanceLock = nil
		}
	}()
	if err := holdInstanceLock(); err != nil {
		t.Fatalf("a brief probe stopped the lock: %v", err)
	}
}

func TestLockHeldReportsAnUnreadableLock(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read the lock")
	}
	data := t.TempDir()
	t.Setenv("AIQ_DATA_DIR", data)
	if err := os.WriteFile(filepath.Join(data, "daemon.lock"), nil, 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := LockHeld(); err == nil {
		t.Fatal("no error for a lock that cannot be read")
	}
}

func TestLockHeldWithNoLockFile(t *testing.T) {
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	if held, err := LockHeld(); held || err != nil {
		t.Fatalf("held %v, err %v", held, err)
	}
	if err := holdInstanceLock(); err != nil {
		t.Fatal(err)
	}
	if held, err := LockHeld(); !held || err != nil {
		t.Fatalf("after locking: held %v, err %v", held, err)
	}
	instanceLock.Close()
	instanceLock = nil
	if held, err := LockHeld(); held || err != nil {
		t.Fatalf("after release: held %v, err %v", held, err)
	}
}

func TestInstanceLockRefusesASecondDaemon(t *testing.T) {
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	defer func() {
		if instanceLock != nil {
			instanceLock.Close()
			instanceLock = nil
		}
	}()
	if err := holdInstanceLock(); err != nil {
		t.Fatal(err)
	}
	held := instanceLock
	if holdInstanceLock() == nil {
		t.Fatal("a second daemon took the lock")
	}
	if instanceLock != held {
		t.Fatal("the failed attempt replaced the held lock")
	}
}

// Launch arguments and the audit log can hold prompts, and any local user can reach
// the port. aiq status --json prints the same struct and must keep both.
func TestTheAPILeavesOutLaunchArgumentsAndEvents(t *testing.T) {
	const secret = "a-prompt-typed-at-launch"
	view := &pool.View{
		SchemaVersion: 1,
		Hostname:      "h",
		GeneratedAt:   9007199254740993, // a float64 cannot hold this one exactly
		Accounts: []pool.AccountView{{
			ID:     "claude/a",
			Leases: []pool.LeaseView{{ID: 7, Cwd: "/w", Args: secret}},
		}},
		Events: []pool.EventView{{Type: "launch", Detail: secret}},
	}
	whole, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(whole), secret) != 2 {
		t.Fatalf("the command's own output lost a field: %s", whole)
	}
	doc, err := stateDocument(view)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), secret) || strings.Contains(string(got), `"events"`) || strings.Contains(string(got), `"args"`) {
		t.Errorf("the API serves it: %s", got)
	}
	for _, want := range []string{`"schema_version":2`, `"hostname":"h"`, `"generated_at":9007199254740993`, `"cwd":"/w"`} {
		if !strings.Contains(string(got), want) {
			t.Errorf("the API lost %s: %s", want, got)
		}
	}
}

func TestTheDaemonServesNoEventHistoryOrLaunchArguments(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := New(config.Default(), &pool.Pool{Cfg: config.Default(), St: st}, log.New(io.Discard, "", 0))
	const secret = "a-prompt-in-the-audit-log"
	st.LogEvent("claude", "claude/a", "launch", secret, time.Now())
	if err := st.AddAccount(state.Account{ID: "claude/a", Provider: "claude", Name: "a", Home: t.TempDir(), Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddLease(state.Lease{
		AccountID: "claude/a", PID: os.Getpid(), Hostname: pool.Hostname(), Mode: state.ModeWorker,
		Cwd: "/lease-cwd", Args: secret, StartedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.routes("id"))
	defer srv.Close()

	for path, wantStatus := range map[string]int{"/api/events": http.StatusNotFound, "/api/state": http.StatusOK, "/": http.StatusOK} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != wantStatus {
			t.Errorf("%s: status %d, want %d", path, resp.StatusCode, wantStatus)
		}
		if strings.Contains(string(body), secret) {
			t.Errorf("%s serves what it must not: %s", path, body)
		}
		if path == "/api/state" && !strings.Contains(string(body), "/lease-cwd") {
			t.Errorf("the lease was not in the answer, so this proves nothing: %s", body)
		}
	}
	// The event was there to serve, so the checks above can fail.
	events, err := st.ListEvents(5)
	if err != nil || len(events) != 1 {
		t.Fatalf("events: %v, %v", events, err)
	}
}
