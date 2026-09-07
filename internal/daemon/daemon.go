// Package daemon runs the machine-wide aiq service: it polls telemetry on a
// schedule, reaps dead leases, and serves a JSON API plus a small web page on
// the loopback interface. Shims never depend on it — the SQLite state is the
// contract — but they ping it after a limit event so the next poll is prompt.
package daemon

import (
	_ "embed"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/longrun"
	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/proc"
)

//go:embed ui.html
var uiHTML []byte

//go:embed help.html
var helpHTML []byte

type Server struct {
	pool   *pool.Pool
	cfg    *config.Config
	log    *log.Logger
	pollCh chan []string

	mu       sync.Mutex
	lastPoll time.Time
	lastErr  string
	polling  bool
}

func New(cfg *config.Config, p *pool.Pool, logger *log.Logger) *Server {
	return &Server{
		pool:   p,
		cfg:    cfg,
		log:    logger,
		pollCh: make(chan []string, 8),
	}
}

// Run serves until the listener fails.
func (s *Server) Run() error {
	go s.pollLoop()
	go s.housekeeping()
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleUI)
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("/help", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(helpHTML)
	})
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/poll", s.handlePoll)
	mux.HandleFunc("/api/events", s.handleEvents)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	ln, err := net.Listen("tcp", s.cfg.Daemon.Listen)
	if err != nil {
		return err
	}
	s.log.Printf("aiq daemon listening on http://%s/", s.cfg.Daemon.Listen)
	srv := &http.Server{Handler: localOnly(mux), ReadHeaderTimeout: 5 * time.Second}
	return srv.Serve(ln)
}

func (s *Server) pollLoop() {
	interval := time.Duration(s.cfg.Poll.IntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	s.poll(nil)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.poll(nil)
		case ids := <-s.pollCh:
			// Coalesce a burst of requests.
			time.Sleep(2 * time.Second)
			for len(s.pollCh) > 0 {
				more := <-s.pollCh
				if len(more) == 0 {
					ids = nil
				} else if ids != nil {
					ids = append(ids, more...)
				}
			}
			s.poll(ids)
		}
	}
}

func (s *Server) poll(ids []string) {
	s.mu.Lock()
	if s.polling {
		s.mu.Unlock()
		return
	}
	s.polling = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.polling = false
		s.mu.Unlock()
	}()

	results := s.pool.Refresh(ids...)
	var ok, failed []string
	for _, r := range results {
		if r.Err != nil {
			failed = append(failed, r.ID+": "+r.Err.Error())
		} else {
			ok = append(ok, r.ID)
		}
	}
	s.mu.Lock()
	s.lastPoll = time.Now()
	s.lastErr = strings.Join(failed, "; ")
	s.mu.Unlock()
	if len(ok) > 0 {
		s.log.Printf("poll: updated %s", strings.Join(ok, ", "))
	}
	for _, f := range failed {
		s.log.Printf("poll: %s", f)
	}
}

func (s *Server) housekeeping() {
	self, _ := os.Executable()
	sup := &longrun.Supervisor{Pool: s.pool, Logf: s.log.Printf, AiqBin: self}
	interval := time.Duration(s.cfg.Long.CheckIntervalSeconds) * time.Second
	for {
		time.Sleep(interval)
		s.pool.St.PruneLeases(pool.Hostname(), proc.Alive)
		s.pool.St.TrimEvents(2000)
		sup.Tick()
	}
}

func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(uiHTML)
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	view, err := s.pool.View(50)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.mu.Lock()
	extra := map[string]any{
		"last_poll":     unixOrZero(s.lastPoll),
		"last_error":    s.lastErr,
		"polling":       s.polling,
		"poll_interval": s.cfg.Poll.IntervalSeconds,
		"listen":        s.cfg.Daemon.Listen,
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"daemon": extra, "view": view})
}

func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	var ids []string
	if v := r.URL.Query().Get("account"); v != "" {
		ids = strings.Split(v, ",")
	}
	select {
	case s.pollCh <- ids:
	default:
	}
	w.Write([]byte("queued\n"))
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	events, err := s.pool.St.ListEvents(200)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(events)
}

// localOnly rejects requests whose Host header is not a loopback name, so a
// page that rebinds a DNS name to 127.0.0.1 cannot read the API.
func localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.Trim(host, "[]")
		if host != "localhost" && host != "127.0.0.1" && host != "::1" {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Notify asks a running daemon to poll soon. It never blocks a launch: one
// second timeout, errors ignored.
func Notify(listen string, accountIDs ...string) {
	if listen == "" {
		return
	}
	url := "http://" + listen + "/api/poll"
	if len(accountIDs) > 0 {
		url += "?account=" + strings.Join(accountIDs, ",")
	}
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Post(url, "text/plain", nil)
	if err == nil {
		resp.Body.Close()
	}
}

// Alive reports whether a daemon answers on listen.
func Alive(listen string) bool {
	client := &http.Client{Timeout: 700 * time.Millisecond}
	resp, err := client.Get("http://" + listen + "/healthz")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

// LogPath is where the service writes its log.
func LogPath() string { return filepath.Join(paths.LogDir(), "daemon.log") }

// OpenLog opens (creating) the daemon log for appending.
func OpenLog() (*os.File, error) {
	if err := os.MkdirAll(paths.LogDir(), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(LogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
