package daemon

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/navauth"
	"github.com/orlenko/aiq/internal/navlaunch"
	"github.com/orlenko/aiq/internal/paths"
)

// Explicit, invented-only native-browser fixture. The private driver supplies a
// ready-file path and owns its isolated browser; ordinary suites do not start it.
func TestNavigatorBrowserAuthFixture(t *testing.T) {
	ready := os.Getenv("AIQ_TEST_NAV_BROWSER_READY")
	if ready == "" {
		t.Skip("native browser fixture NOT RUN without its explicit private driver")
	}
	s := lifeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"rows":[]}`))
	}))
	// Only the invented test directory: satisfy the private launch namespace
	// policy without repairing any production directory or following its links.
	privateData, err := filepath.EvalSymlinks(paths.DataDir())
	if err != nil || os.Chmod(privateData, 0700) != nil {
		t.Fatal("private fixture data setup")
	}
	t.Setenv("AIQ_DATA_DIR", privateData)
	s.slots = newConnSlots(64, func(string, ...any) {})
	s.MountNavigatorPages(NavigatorPages())
	g, _ := lifeStart(t, s)
	var exchanges atomic.Int32
	exchange := g.auth.exchange
	g.auth.exchange = func(code string) (string, error) { exchanges.Add(1); return exchange(code) }
	done := make(chan struct{})
	go func() { s.serveNavigator(g); close(done) }()
	t.Cleanup(func() { g.Close(); lifeWait(t, done) })
	control, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stop, once := make(chan struct{}), new(sync.Once)
	var serial atomic.Uint32
	ctl := &http.Server{ReadHeaderTimeout: time.Second, WriteTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		switch r.URL.Path {
		case "/issue", "/file":
			const body = `{"action":"issue"}`
			now, e := g.auth.state.Now()
			seed := serial.Add(1)
			if e != nil || seed > 255 {
				w.WriteHeader(503)
				return
			}
			p := navauth.Proof{Timestamp: now, Nonce: [32]byte{byte(seed)}}
			p.MAC, e = navauth.RequestMAC(inventedLifeKey, navauth.Binding{Port: g.port, Build: navBuild()}, "POST", "/api/nav/login", now, p.Nonce, []byte(body))
			if e != nil {
				w.WriteHeader(503)
				return
			}
			facts, e := g.auth.state.Admit("POST", "/api/nav/login", p, []byte(body))
			if e != nil {
				w.WriteHeader(503)
				return
			}
			code, e := g.auth.state.Issue(facts)
			if e != nil {
				w.WriteHeader(503)
				return
			}
			if r.URL.Path == "/file" {
				file, err := navlaunch.Prepare(r.Context(), g.handle.scope.dataDir)
				if err != nil {
					w.WriteHeader(503)
					return
				}
				retained := false
				defer func() { file.Finish(retained) }()
				body, err := navlaunch.Document(g.port, "/invented/project &é", code)
				if err != nil {
					w.WriteHeader(503)
					return
				}
				name, err := file.Publish(r.Context(), body)
				if err != nil {
					w.WriteHeader(503)
					return
				}
				retained = true
				_ = json.NewEncoder(w).Encode(map[string]string{"file": name})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"code": code})
		case "/counts":
			_ = json.NewEncoder(w).Encode(map[string]int32{"exchanges": exchanges.Load()})
		case "/stop":
			once.Do(func() { close(stop) })
		default:
			w.WriteHeader(404)
		}
	})}
	go ctl.Serve(control)
	t.Cleanup(func() { ctl.Close() })
	raw, err := json.Marshal(map[string]any{"port": g.port, "control": control.Addr().String(), "build": navBuild()})
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(ready, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.Write(raw)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatal("private ready fixture write failed")
	}
	select {
	case <-stop:
	case <-time.After(90 * time.Second):
		t.Fatal("private native browser driver did not finish")
	}
}
