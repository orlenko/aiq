package daemon

import (
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

// answerFlushConn holds the transport write after a small answer's handler has
// returned. It exposes the first retirement deadline or completed Close, so the
// fixture does not depend on racing net/http's buffered response flush.
type answerFlushConn struct {
	net.Conn
	writing, release, action chan struct{}
	writeOnce, actionOnce    sync.Once
}

func (c *answerFlushConn) Write(p []byte) (int, error) {
	c.writeOnce.Do(func() { close(c.writing); <-c.release })
	return c.Conn.Write(p)
}
func (c *answerFlushConn) SetReadDeadline(at time.Time) error {
	err := c.Conn.SetReadDeadline(at)
	if at.Equal(aLongTimeAgo) {
		c.actionOnce.Do(func() { close(c.action) })
	}
	return err
}
func (c *answerFlushConn) Close() error {
	err := c.Conn.Close()
	c.actionOnce.Do(func() { close(c.action) })
	return err
}

type answerFlushListener struct {
	net.Listener
	first *answerFlushConn
	once  sync.Once
}

func (l *answerFlushListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	var wrapped net.Conn = c
	l.once.Do(func() { l.first.Conn = c; wrapped = l.first })
	return wrapped, nil
}

func TestProducedAnswerFlushesWhenItsConnectionIsRetired(t *testing.T) {
	s, _ := testSlots(1)
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	first := &answerFlushConn{writing: make(chan struct{}), release: make(chan struct{}), action: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(first.release) }) }
	defer release()
	ln := s.listener(&answerFlushListener{Listener: base, first: first})
	srv := s.server(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "produced answer\n") }), nil)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	t.Cleanup(func() {
		release()
		srv.Close()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("server did not stop")
		}
	})
	a := dialRaw(t, base.Addr().String())
	a.send(t, "/answer", true)
	select {
	case <-first.writing:
	case <-time.After(10 * time.Second):
		t.Fatal("answer did not reach its transport flush")
	}
	if st := s.stats(); st.busy != 0 || st.used != 1 {
		t.Fatalf("flush did not follow handler return: %+v", st)
	}
	b := dialRaw(t, base.Addr().String())
	b.send(t, "/answer", true)
	select {
	case <-first.action:
	case <-time.After(10 * time.Second):
		t.Fatal("connection was not selected while its produced answer waited")
	}
	release()
	if body, err := a.answer(10 * time.Second); err != nil || body != "produced answer\n" {
		t.Fatalf("produced answer lost during retirement: %q, %v", body, err)
	}
	if body, err := b.answer(10 * time.Second); err != nil || body != "produced answer\n" {
		t.Fatalf("new connection did not progress: %q, %v", body, err)
	}
	waitFor(t, s, "both connections closed", func(st slotStats) bool { return st.used == 0 })
}
