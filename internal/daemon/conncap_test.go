package daemon

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// slotStats is what a test reads from the slots to know where things stand.
type slotStats struct {
	used, busy, waiting, retired, entered int
}

func (s *connSlots) stats() slotStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := slotStats{used: s.used, waiting: s.waiting, retired: s.retired, entered: s.entered}
	for c := range s.conns {
		if c.inHandler {
			st.busy++
		}
	}
	return st
}

// waitFor waits until the slots meet cond.
func waitFor(t *testing.T, s *connSlots, what string, cond func(slotStats) bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond(s.stats()) {
		if time.Now().After(deadline) {
			t.Fatalf("not %s: %+v", what, s.stats())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// logLines collects what the slots log.
type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logLines) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

var testPage = []byte(strings.Repeat("0123456789abcdef", 1<<10))

// capped is a small server counting its connections in a connSlots.
type capped struct {
	addr     string
	ln       *capListener
	accepted chan struct{}

	mu    sync.Mutex
	gates map[string]chan struct{}
}

// gate is what /hold/name and /page wait on; a send lets one such request finish.
func (c *capped) gate(name string) chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gates[name] == nil {
		c.gates[name] = make(chan struct{})
	}
	return c.gates[name]
}

// hookListener tells when Accept hands a connection to the server.
type hookListener struct {
	net.Listener
	accepted chan struct{}
}

func (h hookListener) Accept() (net.Conn, error) {
	c, err := h.Listener.Accept()
	if err == nil {
		h.accepted <- struct{}{}
	}
	return c, err
}

// serveCapped serves /ok, /page and /hold/name on a fresh loopback port, counting
// connections in s. Its waits for a request or the next one are a minute, so a test
// sees what the slots do before the server's own limits act.
func serveCapped(t *testing.T, s *connSlots) *capped {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := &capped{addr: ln.Addr().String(), ln: s.listener(ln), accepted: make(chan struct{}, 1000), gates: map[string]chan struct{}{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	mux.HandleFunc("/page", func(w http.ResponseWriter, r *http.Request) {
		<-c.gate("page")
		w.Write(testPage)
	})
	mux.HandleFunc("/hold/", func(w http.ResponseWriter, r *http.Request) {
		<-c.gate(strings.TrimPrefix(r.URL.Path, "/hold/"))
		io.WriteString(w, "held\n")
	})
	srv := s.server(mux, nil)
	srv.ReadHeaderTimeout, srv.ReadTimeout, srv.IdleTimeout = time.Minute, time.Minute, time.Minute
	go srv.Serve(hookListener{c.ln, c.accepted})
	t.Cleanup(func() {
		c.mu.Lock()
		for _, g := range c.gates {
			close(g)
		}
		c.gates = map[string]chan struct{}{}
		c.mu.Unlock()
		srv.Close()
	})
	return c
}

func testSlots(n int) (*connSlots, *logLines) {
	l := &logLines{}
	return newConnSlots(n, l.logf), l
}

// rawConn is a client connection that writes requests by hand.
type rawConn struct {
	net.Conn
	r *bufio.Reader
}

func dialRaw(t *testing.T, addr string) *rawConn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &rawConn{c, bufio.NewReader(c)}
}

func (c *rawConn) send(t *testing.T, path string, close bool) {
	t.Helper()
	conn := ""
	if close {
		conn = "Connection: close\r\n"
	}
	c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: 127.0.0.1\r\n%s\r\n", path, conn); err != nil {
		t.Fatal(err)
	}
}

// answer reads one whole answer within d.
func (c *rawConn) answer(d time.Duration) (string, error) {
	c.SetReadDeadline(time.Now().Add(d))
	resp, err := http.ReadResponse(c.r, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

func (c *rawConn) get(t *testing.T, path string) string {
	t.Helper()
	c.send(t, path, false)
	body, err := c.answer(10 * time.Second)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return body
}

// closedByServer reports whether the server has closed the connection.
func (c *rawConn) closedByServer() bool {
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, err := c.r.ReadByte()
	var ne net.Error
	return err != nil && !(errors.As(err, &ne) && ne.Timeout())
}

// fetch asks for path on a new connection and gives the answer.
func fetch(t *testing.T, addr, path string) string {
	t.Helper()
	c := dialRaw(t, addr)
	c.send(t, path, true)
	body, err := c.answer(10 * time.Second)
	if err != nil {
		t.Fatalf("%s on a new connection: %v", path, err)
	}
	return body
}

// noAnswerFor checks that c gets nothing for 200 ms.
func noAnswerFor(t *testing.T, c *rawConn, what string) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, err := c.r.Peek(1); err == nil {
		t.Fatalf("%s was answered", what)
	}
}

func TestAtTheLimitANewConnectionWaitsForAFreeSlot(t *testing.T) {
	s, _ := testSlots(2)
	srv := serveCapped(t, s)
	a, b := dialRaw(t, srv.addr), dialRaw(t, srv.addr)
	a.send(t, "/hold/a", false)
	b.send(t, "/hold/b", false)
	waitFor(t, s, "two inside a handler", func(st slotStats) bool { return st.busy == 2 })
	<-srv.accepted
	<-srv.accepted
	third := dialRaw(t, srv.addr)
	third.send(t, "/ok", false)
	select {
	case <-srv.accepted:
		t.Fatal("a third connection was handed to the server at the limit")
	case <-time.After(200 * time.Millisecond):
	}
	srv.gate("a") <- struct{}{}
	if body, err := a.answer(10 * time.Second); body != "held\n" || err != nil {
		t.Errorf("the first answer: %q, %v", body, err)
	}
	if body, err := third.answer(10 * time.Second); body != "ok\n" || err != nil {
		t.Errorf("the third: %q, %v", body, err)
	}
}

func TestTheConnectionUnusedLongestIsClosedToMakeRoomAndOnlyIt(t *testing.T) {
	s, _ := testSlots(3)
	srv := serveCapped(t, s)
	var conns []*rawConn
	for i := 0; i < 3; i++ {
		c := dialRaw(t, srv.addr)
		if body := c.get(t, "/ok"); body != "ok\n" {
			t.Fatalf("connection %d: %q", i, body)
		}
		waitFor(t, s, "answered", func(st slotStats) bool { return st.busy == 0 && st.entered == i+1 })
		conns = append(conns, c)
	}
	if body := fetch(t, srv.addr, "/ok"); body != "ok\n" {
		t.Fatalf("the fourth: %q", body)
	}
	if !conns[0].closedByServer() {
		t.Error("the connection unused longest is still open")
	}
	for i, c := range conns[1:] {
		if body := c.get(t, "/ok"); body != "ok\n" {
			t.Errorf("newer connection %d: %q", i+1, body)
		}
	}
}

func TestConnectionsThatNeverSendARequestDoNotKeepANewOneWaiting(t *testing.T) {
	s, _ := testSlots(2)
	srv := serveCapped(t, s)
	silent := dialRaw(t, srv.addr)
	waitFor(t, s, "one in use", func(st slotStats) bool { return st.used == 1 })
	half := dialRaw(t, srv.addr)
	fmt.Fprint(half, "GET /ok HTTP/1.1\r\nHo")
	waitFor(t, s, "two in use", func(st slotStats) bool { return st.used == 2 })
	if body := fetch(t, srv.addr, "/ok"); body != "ok\n" {
		t.Fatalf("the third: %q", body)
	}
	if !silent.closedByServer() {
		t.Error("the oldest connection is still open")
	}
}

func TestARequestWhoseBodyStopsArrivingDoesNotKeepANewOneWaiting(t *testing.T) {
	s, _ := testSlots(2)
	srv := serveCapped(t, s)
	for i := 0; i < 2; i++ {
		c := dialRaw(t, srv.addr)
		fmt.Fprint(c, "POST /ok HTTP/1.1\r\nHost: 127.0.0.1\r\nContent-Length: 1000\r\n\r\nx")
	}
	waitFor(t, s, "both answered", func(st slotStats) bool { return st.entered == 2 && st.busy == 0 })
	if body := fetch(t, srv.addr, "/ok"); body != "ok\n" {
		t.Fatalf("the third: %q", body)
	}
}

func TestManyConnectionsThatNeverSendARequestDoNotKeepANewOneWaiting(t *testing.T) {
	s, _ := testSlots(2)
	srv := serveCapped(t, s)
	for i := 0; i < 50; i++ {
		dialRaw(t, srv.addr)
	}
	if body := fetch(t, srv.addr, "/ok"); body != "ok\n" {
		t.Fatalf("after 50 silent connections: %q", body)
	}
}

func TestAConnectionInsideAHandlerIsNotClosedToMakeRoom(t *testing.T) {
	s, _ := testSlots(1)
	srv := serveCapped(t, s)
	a := dialRaw(t, srv.addr)
	a.send(t, "/hold/a", false)
	waitFor(t, s, "inside a handler", func(st slotStats) bool { return st.busy == 1 })
	b := dialRaw(t, srv.addr)
	b.send(t, "/ok", false)
	waitFor(t, s, "waiting", func(st slotStats) bool { return st.waiting == 1 })
	noAnswerFor(t, b, "the second connection")
	if st := s.stats(); st.retired != 0 {
		t.Errorf("%d connections were closed to make room while the only one was inside a handler", st.retired)
	}
	srv.gate("a") <- struct{}{}
	if body, err := a.answer(10 * time.Second); body != "held\n" || err != nil {
		t.Errorf("the first answer: %q, %v", body, err)
	}
	if body, err := b.answer(10 * time.Second); body != "ok\n" || err != nil {
		t.Errorf("the second: %q, %v", body, err)
	}
}

func TestAnAnswerIsSentBeforeItsConnectionIsClosedToMakeRoom(t *testing.T) {
	s, _ := testSlots(1)
	srv := serveCapped(t, s)
	for i := 0; i < 200; i++ {
		a := dialRaw(t, srv.addr)
		a.send(t, "/page", false)
		waitFor(t, s, "inside a handler", func(st slotStats) bool { return st.busy == 1 })
		b := dialRaw(t, srv.addr)
		b.send(t, "/ok", true)
		waitFor(t, s, "waiting", func(st slotStats) bool { return st.waiting == 1 })
		srv.gate("page") <- struct{}{}
		if body, err := a.answer(10 * time.Second); body != string(testPage) || err != nil {
			t.Fatalf("try %d: the first answer had %d of %d bytes: %v", i, len(body), len(testPage), err)
		}
		if body, err := b.answer(10 * time.Second); body != "ok\n" || err != nil {
			t.Fatalf("try %d: the second: %q, %v", i, body, err)
		}
		a.Close()
		b.Close()
		waitFor(t, s, "all closed", func(st slotStats) bool { return st.used == 0 })
	}
}

func TestAWaitingConnectionIsServedWhenAHandlerEnds(t *testing.T) {
	s, _ := testSlots(2)
	srv := serveCapped(t, s)
	a, b := dialRaw(t, srv.addr), dialRaw(t, srv.addr)
	a.send(t, "/hold/a", false)
	b.send(t, "/hold/b", false)
	waitFor(t, s, "two inside a handler", func(st slotStats) bool { return st.busy == 2 })
	third := dialRaw(t, srv.addr)
	third.send(t, "/ok", false)
	waitFor(t, s, "waiting", func(st slotStats) bool { return st.waiting == 1 })
	srv.gate("a") <- struct{}{}
	srv.gate("b") <- struct{}{}
	for _, c := range []*rawConn{a, b} {
		if body, err := c.answer(10 * time.Second); body != "held\n" || err != nil {
			t.Errorf("a held answer: %q, %v", body, err)
		}
	}
	if body, err := third.answer(10 * time.Second); body != "ok\n" || err != nil {
		t.Errorf("the third: %q, %v", body, err)
	}
}

func TestNothingIsClosedBelowTheLimit(t *testing.T) {
	s, _ := testSlots(3)
	srv := serveCapped(t, s)
	a, b := dialRaw(t, srv.addr), dialRaw(t, srv.addr)
	a.get(t, "/ok")
	b.get(t, "/ok")
	waitFor(t, s, "answered", func(st slotStats) bool { return st.entered == 2 && st.busy == 0 })
	if body := fetch(t, srv.addr, "/ok"); body != "ok\n" {
		t.Fatalf("the third: %q", body)
	}
	for _, c := range []*rawConn{a, b} {
		if body := c.get(t, "/ok"); body != "ok\n" {
			t.Errorf("an idle connection: %q", body)
		}
	}
	if st := s.stats(); st.retired != 0 {
		t.Errorf("%d connections were closed below the limit", st.retired)
	}
}

func TestTheLimitIsSharedByBothListeners(t *testing.T) {
	s, _ := testSlots(2)
	one, two := serveCapped(t, s), serveCapped(t, s)
	a, b := dialRaw(t, one.addr), dialRaw(t, two.addr)
	a.send(t, "/hold/a", false)
	b.send(t, "/hold/b", false)
	waitFor(t, s, "two inside a handler", func(st slotStats) bool { return st.busy == 2 })
	third := dialRaw(t, one.addr)
	third.send(t, "/ok", false)
	noAnswerFor(t, third, "a third connection on the first listener")
	two.gate("b") <- struct{}{}
	if body, err := b.answer(10 * time.Second); body != "held\n" || err != nil {
		t.Errorf("the other listener's answer: %q, %v", body, err)
	}
	if body, err := third.answer(10 * time.Second); body != "ok\n" || err != nil {
		t.Errorf("the third: %q, %v", body, err)
	}
}

// acceptOne dials l and accepts that connection.
func acceptOne(t *testing.T, l *capListener) (server, client net.Conn) {
	t.Helper()
	client, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	server, err = l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	return server, client
}

func capLoopback(t *testing.T, s *connSlots) *capListener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := s.listener(ln)
	t.Cleanup(func() { l.Close() })
	return l
}

func TestClosingAConnectionTwiceFreesOneSlot(t *testing.T) {
	s, _ := testSlots(2)
	l := capLoopback(t, s)
	first, _ := acceptOne(t, l)
	acceptOne(t, l)
	first.Close()
	first.Close()
	if st := s.stats(); st.used != 1 {
		t.Errorf("%d slots in use after closing one of two connections twice", st.used)
	}
}

type tryAgain struct{}

func (*tryAgain) Error() string   { return "try again" }
func (*tryAgain) Timeout() bool   { return false }
func (*tryAgain) Temporary() bool { return true }

// failingListener fails every Accept with err.
type failingListener struct{ err error }

func (f failingListener) Accept() (net.Conn, error) { return nil, f.err }
func (failingListener) Close() error                { return nil }
func (failingListener) Addr() net.Addr              { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

func TestAFailedAcceptGivesBackTheSameErrorAndHoldsNoSlot(t *testing.T) {
	s, _ := testSlots(2)
	want := &tryAgain{}
	_, err := s.listener(failingListener{want}).Accept()
	if err != error(want) {
		t.Errorf("Accept gave %#v, want the listener's own error", err)
	}
	if st := s.stats(); st.used != 0 {
		t.Errorf("%d slots in use after a failed Accept", st.used)
	}
}

func TestClosingTheListenerEndsAWaitingAccept(t *testing.T) {
	s, _ := testSlots(1)
	l := capLoopback(t, s)
	acceptOne(t, l)
	pending, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer pending.Close()
	got := make(chan error, 1)
	go func() {
		c, err := l.Accept()
		if c != nil {
			c.Close()
		}
		got <- err
	}()
	waitFor(t, s, "waiting", func(st slotStats) bool { return st.waiting == 1 })
	l.Close()
	select {
	case err := <-got:
		if !errors.Is(err, net.ErrClosed) {
			t.Errorf("Accept gave %v, want net.ErrClosed", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Accept still waits after Close")
	}
	pending.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := pending.Read(make([]byte, 1)); err != io.EOF {
		t.Errorf("the waiting client read %v, want EOF", err)
	}
}

func TestClosingOneListenerLeavesTheOtherWaiting(t *testing.T) {
	s, _ := testSlots(1)
	one, two := capLoopback(t, s), capLoopback(t, s)
	held, _ := acceptOne(t, one)
	results := map[*capListener]chan error{}
	for _, l := range []*capListener{one, two} {
		c, err := net.Dial("tcp", l.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		got := make(chan error, 1)
		results[l] = got
		go func() {
			c, err := l.Accept()
			if c != nil {
				c.Close()
			}
			got <- err
		}()
	}
	waitFor(t, s, "both waiting", func(st slotStats) bool { return st.waiting == 2 })
	one.Close()
	select {
	case err := <-results[one]:
		if !errors.Is(err, net.ErrClosed) {
			t.Errorf("the closed listener's Accept gave %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the closed listener's Accept still waits")
	}
	select {
	case err := <-results[two]:
		t.Fatalf("closing one listener ended the other's Accept: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	held.Close()
	select {
	case err := <-results[two]:
		if err != nil {
			t.Errorf("the other listener's Accept gave %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the other listener did not get the freed slot")
	}
}

func TestAListenerClosedAgainAfterServeEndsIsSafe(t *testing.T) {
	s, _ := testSlots(1)
	l := s.listener(failingListener{errors.New("gone")})
	if err := s.server(http.NotFoundHandler(), nil).Serve(l); err == nil {
		t.Fatal("Serve returned nil")
	}
	l.Close()
	if st := s.stats(); st.used != 0 {
		t.Errorf("%d slots in use", st.used)
	}
}

func TestAConnectionClosedToMakeRoomKeepsItsReadDeadline(t *testing.T) {
	s, _ := testSlots(1)
	c, _ := acceptOne(t, capLoopback(t, s))
	c.(*slotConn).retire(time.Now())
	c.SetReadDeadline(time.Now().Add(time.Hour))
	c.SetDeadline(time.Now().Add(time.Hour))
	got := make(chan error, 1)
	go func() {
		_, err := c.Read(make([]byte, 1))
		got <- err
	}()
	select {
	case err := <-got:
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Errorf("Read gave %v, want a timeout", err)
		}
	case <-time.After(5 * time.Second):
		c.Close()
		t.Fatal("a later deadline let Read wait")
	}
}

func TestASlotConnectionCanCloseOnlyItsWritingSide(t *testing.T) {
	s, _ := testSlots(1)
	c, peer := acceptOne(t, capLoopback(t, s))
	cw, ok := c.(interface{ CloseWrite() error })
	if !ok {
		t.Fatal("no CloseWrite")
	}
	if err := cw.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	peer.SetDeadline(time.Now().Add(10 * time.Second))
	if b, err := io.ReadAll(peer); len(b) != 0 || err != nil {
		t.Fatalf("the peer read %q, %v, want EOF", b, err)
	}
	if _, err := peer.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(10 * time.Second))
	b := make([]byte, 1)
	if _, err := io.ReadFull(c, b); err != nil || b[0] != 'x' {
		t.Errorf("after CloseWrite, read %q, %v", b, err)
	}
}

func TestBothListenersShareTheDaemonsConnectionLimit(t *testing.T) {
	useDataDir(t)
	d := startDaemon(t, true)
	d.mu.Lock()
	statusLn, navLn, statusSrv, navSrv := d.statusLn, d.navLn, d.statusSrv, d.navSrv
	d.mu.Unlock()
	sl, ok1 := statusLn.(*capListener)
	nl, ok2 := navLn.(*capListener)
	if !ok1 || !ok2 {
		t.Fatalf("the listeners are %T and %T", statusLn, navLn)
	}
	if sl.slots != nl.slots || sl.slots != d.s.slots {
		t.Fatal("the two listeners count connections separately")
	}
	if connCap != 64 || sl.slots.max != connCap {
		t.Errorf("the limit is %d, want 64", sl.slots.max)
	}
	if statusSrv.ConnContext == nil || navSrv.ConnContext == nil {
		t.Error("a server does not tell its handler which connection a request came on")
	}
	before := sl.slots.stats().entered
	d.get("/healthz")
	port, _ := navOnFields(t, d.s, navFile(t))
	getURL(t, "http://127.0.0.1:"+strconv.Itoa(port)+"/api/nav/hello")
	if after := sl.slots.stats().entered; after != before+2 {
		t.Errorf("%d requests were counted, want 2", after-before)
	}
}

func TestReachingTheLimitIsLoggedAtMostOnceAMinute(t *testing.T) {
	s, log := testSlots(1)
	var mu sync.Mutex
	now := time.Now()
	s.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	srv := serveCapped(t, s)
	idle := func() {
		waitFor(t, s, "all closed", func(st slotStats) bool { return st.used == 0 })
	}

	silent := dialRaw(t, srv.addr)
	waitFor(t, s, "one in use", func(st slotStats) bool { return st.used == 1 })
	fetch(t, srv.addr, "/ok")
	if lines := log.get(); len(lines) != 1 || !strings.Contains(lines[0], "connection limit (1) reached") {
		t.Fatalf("after the first time a connection was closed to make room, the log is %q", lines)
	}
	silent.closedByServer()

	idle()
	dialRaw(t, srv.addr)
	waitFor(t, s, "one in use", func(st slotStats) bool { return st.used == 1 })
	fetch(t, srv.addr, "/ok")
	idle()
	a := dialRaw(t, srv.addr)
	a.send(t, "/hold/a", true)
	waitFor(t, s, "inside a handler", func(st slotStats) bool { return st.busy == 1 })
	waiter := dialRaw(t, srv.addr)
	waiter.send(t, "/ok", true)
	waitFor(t, s, "waiting", func(st slotStats) bool { return st.waiting == 1 })
	srv.gate("a") <- struct{}{}
	if body, err := waiter.answer(10 * time.Second); body != "ok\n" || err != nil {
		t.Fatalf("the waiting connection: %q, %v", body, err)
	}
	if lines := log.get(); len(lines) != 1 {
		t.Fatalf("within the minute, the log is %q", lines)
	}

	mu.Lock()
	now = now.Add(61 * time.Second)
	mu.Unlock()
	idle()
	dialRaw(t, srv.addr)
	waitFor(t, s, "one in use", func(st slotStats) bool { return st.used == 1 })
	fetch(t, srv.addr, "/ok")
	lines := log.get()
	if len(lines) != 2 {
		t.Fatalf("after the minute, the log is %q", lines)
	}
	_, port, _ := net.SplitHostPort(srv.addr)
	for _, line := range lines {
		if strings.Contains(line, "127.0.0.1") || strings.Contains(line, port) || strings.Contains(line, "/") {
			t.Errorf("the line %q names an address, a port or a path", line)
		}
	}
}
