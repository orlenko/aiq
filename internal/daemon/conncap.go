package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// connCap is how many connections the status page and the navigator hold open at
// once, together.
const connCap = 64

// retireWrite is how long a connection chosen to make room has to finish writing
// the answer it is sending.
const retireWrite = time.Second

// aLongTimeAgo is a read deadline that has already passed.
var aLongTimeAgo = time.Unix(1, 0)

// connSlots is one count of admitted connections shared by the daemon's listeners. When
// every slot is taken, a new connection makes room by retiring the connection that
// has gone longest without a request being answered, unless it is inside a handler.
// When every connection is inside a handler, the new one waits until a handler ends
// or a connection closes.
// Each HTTP server may also hold one accepted connection waiting outside this count.
type connSlots struct {
	max int
	// now is the log's clock, and logf where the log goes.
	now  func() time.Time
	logf func(format string, args ...any)

	mu    sync.Mutex
	used  int
	conns map[*slotConn]struct{}
	seq   uint64
	// wake is closed and replaced whenever a handler ends or a slot is freed.
	wake chan struct{}
	// retiring counts connections chosen to make room that have not closed yet, and
	// waiting the Accepts at the limit.
	retiring, waiting int
	retired, entered  int
	lastLog           time.Time
}

func newConnSlots(max int, logf func(format string, args ...any)) *connSlots {
	return &connSlots{max: max, now: time.Now, logf: logf, conns: map[*slotConn]struct{}{}, wake: make(chan struct{})}
}

func (s *connSlots) broadcastLocked() {
	close(s.wake)
	s.wake = make(chan struct{})
}

// noteLocked gives the line to log on reaching the limit: at once the first time,
// then at most once a minute.
func (s *connSlots) noteLocked(what string) string {
	now := s.now()
	if !s.lastLog.IsZero() && now.Sub(s.lastLog) < time.Minute {
		return ""
	}
	s.lastLog = now
	return fmt.Sprintf("connection limit (%d) reached: %s", s.max, what)
}

// victimLocked is the open connection not inside a handler whose last request was
// answered, or which was accepted, longest ago. It is nil if there is none.
func (s *connSlots) victimLocked() *slotConn {
	var v *slotConn
	for c := range s.conns {
		if c.chosen || c.inHandler {
			continue
		}
		if v == nil || c.mark < v.mark {
			v = c
		}
	}
	return v
}

// server is statusServer around h, counting its connections in s.
func (s *connSlots) server(h, refuse http.Handler) *http.Server {
	srv := statusServer(s.handler(h, refuse))
	srv.ConnContext = s.connContext
	return srv
}

type slotKey struct{}

// connContext gives each request its connection, so handler can tell which one is
// being answered.
func (s *connSlots) connContext(ctx context.Context, c net.Conn) context.Context {
	if sc, ok := c.(*slotConn); ok {
		return context.WithValue(ctx, slotKey{}, sc)
	}
	return ctx
}

// handler marks a connection as being answered while h runs. When h returns, even by
// panicking, the connection's request counts as answered now.
func (s *connSlots) handler(h, refuse http.Handler) http.Handler {
	if refuse == nil {
		refuse = connectionUnavailable()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sc, _ := r.Context().Value(slotKey{}).(*slotConn)
		if sc == nil {
			h.ServeHTTP(w, r)
			return
		}
		s.mu.Lock()
		if sc.chosen {
			s.mu.Unlock()
			w.Header().Set("Connection", "close")
			refuse.ServeHTTP(w, r)
			return
		}
		sc.inHandler = true
		s.entered++
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			sc.inHandler = false
			s.seq++
			sc.mark = s.seq
			s.broadcastLocked()
			s.mu.Unlock()
		}()
		h.ServeHTTP(w, r)
	})
}

func connectionUnavailable() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
}

// listener counts ln's connections in s. Its Close ends only its own waiting Accept.
func (s *connSlots) listener(ln net.Listener) *capListener {
	return &capListener{Listener: ln, slots: s, done: make(chan struct{})}
}

type capListener struct {
	net.Listener
	slots     *connSlots
	done      chan struct{}
	closeOnce sync.Once
}

func (l *capListener) Close() error {
	l.closeOnce.Do(func() {
		l.slots.mu.Lock()
		close(l.done)
		l.slots.mu.Unlock()
	})
	return l.Listener.Close()
}

// Accept takes a slot for the next connection. At the limit it retires at most one
// connection for each Accept waiting, and waits for a slot.
func (l *capListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	s := l.slots
	s.mu.Lock()
	atLimit := false
	for {
		// Closure, admission and victim selection share one ordering. In particular,
		// a freed slot cannot win over a Close that completed while retirement ran.
		select {
		case <-l.done:
			if atLimit {
				s.waiting--
			}
			s.mu.Unlock()
			c.Close()
			return nil, net.ErrClosed
		default:
		}
		if s.used < s.max {
			if atLimit {
				s.waiting--
			}
			s.used++
			s.seq++
			sc := &slotConn{Conn: c, slots: s, mark: s.seq}
			s.conns[sc] = struct{}{}
			s.mu.Unlock()
			return sc, nil
		}
		line := ""
		if !atLimit {
			atLimit = true
			s.waiting++
		}
		var v *slotConn
		if s.retiring < s.waiting {
			if v = s.victimLocked(); v != nil {
				v.chosen = true
				s.retiring++
				s.retired++
				line = s.noteLocked("closing the connection unused longest to make room")
			} else if s.retiring == 0 {
				line = s.noteLocked("every connection is being answered, so a new one waits")
			}
		}
		wake := s.wake
		s.mu.Unlock()
		if line != "" {
			s.logf("%s", line)
		}
		if v != nil {
			v.retire(time.Now())
			s.mu.Lock()
			continue
		}
		select {
		case <-wake:
		case <-l.done:
			s.mu.Lock()
			s.waiting--
			s.mu.Unlock()
			c.Close()
			return nil, net.ErrClosed
		}
		s.mu.Lock()
	}
}

// slotConn holds one slot until it is closed.
type slotConn struct {
	net.Conn
	slots *connSlots
	once  sync.Once

	// Guarded by slots.mu: mark orders connections by when one was accepted or last
	// had a request answered; chosen is set once it is retired to make room.
	mark      uint64
	inHandler bool
	chosen    bool

	// retired keeps the deadlines retire set; writeBy is set before it.
	retired atomic.Bool
	writeBy time.Time
}

// retire makes net/http finish the answer it has already produced and then close
// the connection: reading stops at once, and writing gets retireWrite.
func (c *slotConn) retire(now time.Time) {
	c.writeBy = now.Add(retireWrite)
	c.retired.Store(true)
	c.holdDeadlines()
}

func (c *slotConn) holdDeadlines() {
	c.Conn.SetReadDeadline(aLongTimeAgo)
	c.Conn.SetWriteDeadline(c.writeBy)
}

// keep runs set unless the connection is retired, and puts back retire's deadlines if
// it was retired meanwhile.
func (c *slotConn) keep(set func() error) error {
	if c.retired.Load() {
		return nil
	}
	err := set()
	if c.retired.Load() {
		c.holdDeadlines()
	}
	return err
}

func (c *slotConn) SetDeadline(t time.Time) error {
	return c.keep(func() error { return c.Conn.SetDeadline(t) })
}

func (c *slotConn) SetReadDeadline(t time.Time) error {
	return c.keep(func() error { return c.Conn.SetReadDeadline(t) })
}

func (c *slotConn) SetWriteDeadline(t time.Time) error {
	return c.keep(func() error { return c.Conn.SetWriteDeadline(t) })
}

// CloseWrite lets net/http half-close the connection, as it does on a plain one.
func (c *slotConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return errors.ErrUnsupported
}

// Close frees the slot, once however often it is called.
func (c *slotConn) Close() error {
	c.once.Do(func() {
		s := c.slots
		s.mu.Lock()
		if c.chosen {
			s.retiring--
		}
		delete(s.conns, c)
		s.used--
		s.broadcastLocked()
		s.mu.Unlock()
	})
	return c.Conn.Close()
}
