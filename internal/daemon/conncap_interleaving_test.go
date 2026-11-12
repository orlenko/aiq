package daemon

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type retirementBarrierConn struct {
	net.Conn
	entered chan struct{}
	release chan struct{}
}

func (c *retirementBarrierConn) SetReadDeadline(at time.Time) error {
	close(c.entered)
	<-c.release
	return c.Conn.SetReadDeadline(at)
}

func TestRetirementAdmissionAndClosedListener(t *testing.T) {
	s, _ := testSlots(1)
	ln := capLoopback(t, s)
	held, _ := acceptOne(t, ln)
	sc := held.(*slotConn)
	barrier := &retirementBarrierConn{Conn: sc.Conn, entered: make(chan struct{}), release: make(chan struct{})}
	sc.Conn = barrier
	defer func() {
		ln.Close()
		held.Close()
		select {
		case <-barrier.release:
		default:
			close(barrier.release)
		}
	}()
	pending, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer pending.Close()
	result := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if c != nil {
			c.Close()
		}
		result <- err
	}()
	select {
	case <-barrier.entered:
	case <-time.After(time.Second):
		t.Fatal("retirement did not start")
	}
	called := false
	req := httptest.NewRequest("GET", "http://127.0.0.1/ok", nil)
	req = req.WithContext(s.connContext(req.Context(), sc))
	response := httptest.NewRecorder()
	s.handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })).ServeHTTP(response, req)
	if called {
		t.Error("application handler admitted after retirement selection")
	}
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Connection") != "close" {
		t.Errorf("retired request: status %d, headers %v", response.Code, response.Header())
	}
	ln.Close()
	held.Close()
	close(barrier.release)
	select {
	case err := <-result:
		if !errors.Is(err, net.ErrClosed) {
			t.Errorf("closed listener admitted pending connection: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending Accept did not finish")
	}
	if st := s.stats(); st.used != 0 || st.waiting != 0 {
		t.Errorf("cancelled admission retained slots: %+v", st)
	}
	s.mu.Lock()
	retiring := s.retiring
	s.mu.Unlock()
	if retiring != 0 {
		t.Errorf("%d retired connections remain counted", retiring)
	}
	pending.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := pending.Read(make([]byte, 1)); err != io.EOF {
		t.Errorf("cancelled pending socket read: %v, want EOF", err)
	}
	// Closing this listener does not disable shared slots for the other one.
	other := capLoopback(t, s)
	acceptOne(t, other)
}
