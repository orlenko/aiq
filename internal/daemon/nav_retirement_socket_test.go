package daemon

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/navroute"
)

type navRetirementLog struct {
	busy, parked, release chan struct{}
	busyOnce, parkOnce    sync.Once
	mu                    sync.Mutex
	text                  strings.Builder
	reports               int
}

func (l *navRetirementLog) Write(p []byte) (int, error) {
	line := string(p)
	if strings.Contains(line, "navigator: panic report") {
		l.parkOnce.Do(func() { close(l.parked) })
		<-l.release
	}
	l.mu.Lock()
	l.text.Write(p)
	if strings.Contains(line, "navigator: panic report") {
		l.reports++
	}
	l.mu.Unlock()
	if strings.Contains(line, "every connection is being answered") {
		l.busyOnce.Do(func() { close(l.busy) })
	}
	return len(p), nil
}

type navRetirementTCP struct {
	net.Conn
	expired   atomic.Bool
	deadlines chan time.Time
}

func (c *navRetirementTCP) SetReadDeadline(at time.Time) error {
	err := c.Conn.SetReadDeadline(at)
	if at.Equal(aLongTimeAgo) {
		c.expired.Store(true)
	}
	return err
}
func (c *navRetirementTCP) SetWriteDeadline(at time.Time) error {
	err := c.Conn.SetWriteDeadline(at)
	if c.expired.Load() && !at.IsZero() && at.Before(time.Now().Add(2*retireWrite)) {
		select {
		case c.deadlines <- at:
		default:
		}
	}
	return err
}
func (c *navRetirementTCP) CloseWrite() error { return c.Conn.(*net.TCPConn).CloseWrite() }

type navRetirementListener struct {
	net.Listener
	deadlines chan time.Time
}

func (l navRetirementListener) Accept() (net.Conn, error) {
	c, e := l.Listener.Accept()
	if e != nil {
		return nil, e
	}
	return &navRetirementTCP{Conn: c, deadlines: l.deadlines}, nil
}

func TestNavRetirementProtectsOuterCompletion(t *testing.T) {
	// This fixture uses only the pre-correction production API. Keep identical
	// bytes for old/fixed runs; the native response and semantic oracle differ.
	appRelease, sinkRelease := make(chan struct{}), make(chan struct{})
	var appOnce, sinkOnce sync.Once
	releaseApp := func() { appOnce.Do(func() { close(appRelease) }) }
	releaseSink := func() { sinkOnce.Do(func() { close(sinkRelease) }) }
	logging := &navRetirementLog{busy: make(chan struct{}), parked: make(chan struct{}), release: sinkRelease}
	logger := log.New(logging, "", 0)
	s := New(config.Default(), nil, logger)
	s.slots = newConnSlots(1, logger.Printf)
	s.navVerifier = testVerifier{}
	observer := newNavObserver(navroute.Catalog(), logger.Printf, nil, nil)
	s.navObservations = observer
	entered := make(chan *slotConn, 1)
	var requests atomic.Int32
	mux := navroute.NewMux()
	if err := mux.Handle(navroute.Sessions, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			entered <- r.Context().Value(slotKey{}).(*slotConn)
			<-appRelease
			navDiagnosticsLeaf()
		}
		io.WriteString(w, "healthy")
	})); err != nil {
		t.Fatal(err)
	}
	s.MountNavigator(mux)
	server := s.navigatorServer()
	errorLog := &lockedBuffer{}
	server.ErrorLog = log.New(errorLog, "", 0)
	var first, second net.Conn
	var listener net.Listener
	serveDone := make(chan error, 1)
	started := false
	// Every resource and barrier has cleanup ownership BEFORE any worker launch.
	t.Cleanup(func() {
		releaseSink()
		releaseApp()
		if first != nil {
			first.Close()
		}
		if second != nil {
			second.Close()
		}
		if listener != nil {
			listener.Close()
		}
		server.Close()
		if started {
			select {
			case <-serveDone:
			case <-time.After(3 * time.Second):
				t.Error("server worker did not join")
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if !observer.Close(ctx) {
			t.Error("observer did not join")
		}
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadlines := make(chan time.Time, 16)
	listener = s.slots.listener(navRetirementListener{ln, deadlines})
	started = true
	go func() { serveDone <- server.Serve(listener) }()
	dial := func() net.Conn {
		c, e := net.DialTimeout("tcp", ln.Addr().String(), 3*time.Second)
		if e != nil {
			t.Fatal(e)
		}
		if e = c.SetDeadline(time.Now().Add(8 * time.Second)); e != nil {
			c.Close()
			t.Fatal(e)
		}
		return c
	}
	writeRequest := func(c net.Conn) {
		_, e := fmt.Fprintf(c, "GET /api/nav/sessions HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nConnection: close\r\n\r\n", ln.Addr().String(), rightCredential)
		if e != nil {
			t.Fatal(e)
		}
	}
	first = dial()
	writeRequest(first)
	var sc *slotConn
	select {
	case sc = <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("first application did not enter")
	}
	second = dial()
	writeRequest(second)
	// Completing the actual all-busy log establishes an Accept waiter while the
	// app is held, and makes the later selection log minute-suppressed.
	select {
	case <-logging.busy:
	case <-time.After(3 * time.Second):
		t.Fatal("all-busy log did not complete")
	}
	s.slots.mu.Lock()
	waiting := s.slots.waiting
	active := sc.inHandler
	chosen := sc.chosen
	s.slots.mu.Unlock()
	if waiting != 1 || !active || chosen {
		t.Fatal("fixture did not establish real busy admission")
	}
	releaseApp()
	select {
	case <-logging.parked:
	case <-time.After(3 * time.Second):
		t.Fatal("shared observer sink did not park")
	}
	s.slots.mu.Lock()
	active = sc.inHandler
	chosen = sc.chosen
	s.slots.mu.Unlock()
	// Record the old semantic failure without stopping native observation/cleanup.
	if !active || chosen {
		t.Error("connection became retirement-eligible during outer completion")
	}
	var applied time.Time
	select {
	case applied = <-deadlines:
		t.Errorf("retirement write deadline applied while outer completion parked")
		if delay := time.Until(applied.Add(75 * time.Millisecond)); delay > 0 {
			time.Sleep(delay)
		}
	case <-time.After(retireWrite + 150*time.Millisecond):
	}
	releaseSink()
	read := func(c net.Conn) (int, string, error) {
		response, e := http.ReadResponse(bufio.NewReader(c), nil)
		if e != nil {
			return 0, "", e
		}
		defer response.Body.Close()
		body, e := io.ReadAll(response.Body)
		return response.StatusCode, string(body), e
	}
	status, body, readErr := read(first)
	t.Logf("first native response: status=%d complete=%t body_bytes=%d retirement_deadline=%t", status, readErr == nil, len(body), !applied.IsZero())
	if readErr != nil || status != 500 || body != "" {
		t.Error("first request did not return complete empty500")
	}
	status, body, readErr = read(second)
	if readErr != nil || status != 200 || body != "healthy" {
		t.Error("waiting request did not recover")
	}
	first.Close()
	second.Close()
	logging.mu.Lock()
	text, reports := logging.text.String(), logging.reports
	logging.mu.Unlock()
	if reports != 1 || !strings.Contains(text, `navigator: panic report on "GET /api/nav/sessions" at internal/daemon/nav_diagnostics_leaf_test.go:3`) || errorLog.String() != "" {
		t.Error("diagnostic count/origin or server log changed")
	}
	limit := time.Now().Add(3 * time.Second)
	for {
		s.slots.mu.Lock()
		used, waiting, retiring := s.slots.used, s.slots.waiting, s.slots.retiring
		wake := s.slots.wake
		s.slots.mu.Unlock()
		if used == 0 && waiting == 0 && retiring == 0 {
			break
		}
		if time.Now().After(limit) {
			t.Error("admission did not fully clean up")
			break
		}
		select {
		case <-wake:
		case <-time.After(time.Until(limit)):
		}
	}
}
