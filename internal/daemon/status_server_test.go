package daemon

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// startStatusPage serves the status page's handler on a fresh loopback port, with
// the server's settings changed by tune, and returns its address.
func startStatusPage(t *testing.T, tune func(*http.Server)) string {
	t.Helper()
	srv := statusServer(localOnly((&Server{}).routes("test")))
	tune(srv)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().String()
}

func dialStatus(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	return conn
}

func TestStatusServerSettings(t *testing.T) {
	srv := statusServer(http.NotFoundHandler())
	if srv.ReadHeaderTimeout != 5*time.Second || srv.ReadTimeout != 30*time.Second || srv.WriteTimeout != 30*time.Second ||
		srv.IdleTimeout != 2*time.Minute || srv.MaxHeaderBytes != 64<<10 || !srv.DisableGeneralOptionsHandler {
		t.Errorf("the status server is %+v", srv)
	}
}

// A request whose body stops arriving loses its connection at the read deadline.
func TestStatusServerCutsOffABodyThatStopsArriving(t *testing.T) {
	addr := startStatusPage(t, func(s *http.Server) { s.ReadTimeout = 300 * time.Millisecond })
	start := time.Now() // the server's deadline cannot start before the dial
	conn := dialStatus(t, addr)
	fmt.Fprintf(conn, "POST /healthz HTTP/1.1\r\nHost: 127.0.0.1\r\nContent-Length: 1000\r\n\r\nab")
	_, err := io.Copy(io.Discard, conn)
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("the connection was still open after %v", time.Since(start))
	}
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Errorf("closed after %v, before the deadline", d)
	}
}

// A kept-alive connection with no next request is closed at the idle deadline.
func TestStatusServerClosesAnIdleConnection(t *testing.T) {
	addr := startStatusPage(t, func(s *http.Server) { s.IdleTimeout = 300 * time.Millisecond })
	conn := dialStatus(t, addr)
	fmt.Fprintf(conn, "GET /healthz HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n")
	r := bufio.NewReader(conn)
	resp, err := http.ReadResponse(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Close {
		t.Fatalf("healthz: %d, close %v", resp.StatusCode, resp.Close)
	}
	_, err = io.Copy(io.Discard, r)
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("the idle connection was still open")
	}
}

// OPTIONS * meets the same checks as every other request.
func TestStatusServerChecksOptionsStar(t *testing.T) {
	addr := startStatusPage(t, func(*http.Server) {})
	for host, want := range map[string]int{"evil.example": http.StatusForbidden, "127.0.0.1": http.StatusBadRequest} {
		conn := dialStatus(t, addr)
		fmt.Fprintf(conn, "OPTIONS * HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", host)
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("OPTIONS * with Host %s: %d, want %d", host, resp.StatusCode, want)
		}
	}
}
