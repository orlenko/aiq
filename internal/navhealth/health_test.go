package navhealth

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/navauth"
	"github.com/orlenko/aiq/internal/navtoken"
)

const inventedIdentity = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func record(t *testing.T) navtoken.Record {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("navigator unlock is disabled on Windows pending native validation")
	}
	path := filepath.Join(t.TempDir(), "ui-token")
	if err := os.WriteFile(path, []byte("on 123 -5 23456 "+navauth.Encode32([32]byte{1})+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := navtoken.Read(path)
	if err != nil {
		t.Fatal("invented private record not admitted")
	}
	return r
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type body struct {
	io.Reader
	bytes, reads, closes int
	close                func() error
}

func (b *body) Read(p []byte) (int, error) {
	b.reads++
	n, e := b.Reader.Read(p)
	b.bytes += n
	return n, e
}
func (b *body) Close() error {
	b.closes++
	if b.close != nil {
		return b.close()
	}
	return nil
}
func good() *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"X-Aiq-Daemon": {inventedIdentity}, "X-Aiq-Nav": {"pid=123; start=-5; nav=1"}}}
}

func TestHealthExactWireAndCloseBeforeUse(t *testing.T) {
	r := record(t)
	for _, kind := range []string{"good", "body", "extra", "upgrade", "redirect", "identity-duplicate", "identity-alias", "pid", "start", "protocol", "padding", "encoding", "trailer", "late-trailer", "close-error", "close-mutation"} {
		t.Run(kind, func(t *testing.T) {
			response := good()
			b := &body{Reader: strings.NewReader("ok\n")}
			response.Body = b
			calls := 0
			switch kind {
			case "body":
				b.Reader = strings.NewReader("bad")
			case "extra":
				b.Reader = strings.NewReader("ok\nextra")
			case "upgrade":
				response.StatusCode = 101
			case "redirect":
				response.StatusCode = 302
				response.Header.Set("Location", "http://foreign.invalid/")
			case "identity-duplicate":
				response.Header["X-Aiq-Daemon"] = []string{inventedIdentity, inventedIdentity}
			case "identity-alias":
				response.Header["x-aiq-daemon"] = []string{inventedIdentity}
			case "pid":
				response.Header.Set("X-Aiq-Nav", "pid=0123; start=-5; nav=1")
			case "start":
				response.Header.Set("X-Aiq-Nav", "pid=123; start=-6; nav=1")
			case "protocol":
				response.Header.Set("X-Aiq-Nav", "pid=123; start=-5; nav=2")
			case "padding":
				response.Header.Set("X-Aiq-Daemon", inventedIdentity+" ")
			case "encoding":
				response.Header["Content-Encoding"] = nil
			case "trailer":
				response.Header.Set("Trailer", "invented")
			case "late-trailer":
				b.close = func() error { response.Trailer = http.Header{"Invented": {"x"}}; return nil }
			case "close-error":
				b.close = func() error { return errors.New("invented private close cause") }
			case "close-mutation":
				b.close = func() error { response.Header.Set("X-Aiq-Daemon", "changed"); return nil }
			}
			err := check(context.Background(), "127.0.0.1:54321", r, inventedIdentity, roundTrip(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.URL.String() != "http://127.0.0.1:54321/healthz" || req.Method != "GET" || req.Header.Get("Authorization") != "" || req.Header.Get("Cookie") != "" {
					t.Error("unexpected health request")
				}
				return response, nil
			}))
			if (err == nil) != (kind == "good") || calls != 1 || b.closes != 1 || b.bytes > 4 {
				t.Fatal("strict health outcome/ownership mismatch")
			}
			if (kind == "upgrade" || kind == "redirect") && b.reads != 0 {
				t.Fatal("refused status read a body")
			}
			if err != nil && err != ErrUnavailable {
				t.Fatal("raw health cause escaped")
			}
		})
	}
}

func TestHealthAdmissionDoesNoNetwork(t *testing.T) {
	r := record(t)
	calls := 0
	transport := roundTrip(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("invented") })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	trace := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{Got1xxResponse: func(int, textproto.MIMEHeader) error { return nil }})
	for _, row := range []struct {
		ctx              context.Context
		listen, identity string
		record           navtoken.Record
	}{
		{ctx, "127.0.0.1:54321", inventedIdentity, r},
		{nil, "127.0.0.1:54321", inventedIdentity, r},
		{trace, "127.0.0.1:54321", inventedIdentity, r},
		{context.Background(), "localhost:54321", inventedIdentity, r},
		{context.Background(), "127.0.0.1:054321", inventedIdentity, r},
		{context.Background(), "127.0.0.1:54321", "not an identity", r},
		{context.Background(), "127.0.0.1:54321", inventedIdentity, navtoken.Record{}},
	} {
		if check(row.ctx, row.listen, row.record, row.identity, transport) != ErrUnavailable {
			t.Fatal("invalid health admission")
		}
	}
	if calls != 0 {
		t.Fatal("rejected health input reached network")
	}
}

func TestHealthNativeSuccessAndHeaderLimit(t *testing.T) {
	r := record(t)
	for _, large := range []bool{false, true} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.URL.Path != "/healthz" || req.Header.Get("Authorization") != "" {
				t.Error("native health destination")
			}
			w.Header().Set("X-Aiq-Daemon", inventedIdentity)
			w.Header().Set("X-Aiq-Nav", "pid=123; start=-5; nav=1")
			if large {
				w.Header().Set("Invented", strings.Repeat("x", 65537))
			}
			_, _ = w.Write([]byte("ok\n"))
		}))
		err := Check(context.Background(), strings.TrimPrefix(server.URL, "http://"), r, inventedIdentity)
		server.Close()
		if (err == nil) == large {
			t.Fatal("native header allowance")
		}
	}
}

func TestHealthNativeUpgradeRefusesBeforePeerRelease(t *testing.T) {
	r := record(t)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	peerClosed := make(chan struct{})
	accepted := make(chan net.Conn, 1)
	go func() {
		c, e := listener.Accept()
		if e != nil {
			return
		}
		accepted <- c
		defer c.Close()
		request, e := http.ReadRequest(bufio.NewReader(c))
		if e != nil {
			return
		}
		request.Body.Close()
		_, e = io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: invented\r\n\r\n")
		if e != nil {
			return
		}
		var one [1]byte
		if _, e = c.Read(one[:]); e != nil {
			close(peerClosed)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Check(ctx, listener.Addr().String(), r, inventedIdentity) }()
	var c net.Conn
	select {
	case c = <-accepted:
		defer c.Close()
	case <-time.After(time.Second):
		t.Fatal("invented peer not accepted")
	}
	select {
	case err = <-done:
		if err != ErrUnavailable {
			t.Error("upgrade accepted")
		}
	case <-time.After(1500 * time.Millisecond):
		t.Error("upgrade escaped context; peer still held")
		c.Close()
		<-done
	}
	select {
	case <-peerClosed:
	case <-time.After(time.Second):
		t.Error("upgrade body was not closed before peer cleanup")
	}
}

func TestHealthNativeHeldHeadersAndBodyCancelBeforePeerRelease(t *testing.T) {
	for _, partial := range []bool{false, true} {
		r := record(t)
		entered, peerCanceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if partial {
				w.Header().Set("X-Aiq-Daemon", inventedIdentity)
				w.Header().Set("X-Aiq-Nav", "pid=123; start=-5; nav=1")
				_, _ = w.Write([]byte("o"))
				w.(http.Flusher).Flush()
			}
			close(entered)
			select {
			case <-req.Context().Done():
				close(peerCanceled)
			case <-release:
			}
		}))
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		done := make(chan error, 1)
		go func() { done <- Check(ctx, strings.TrimPrefix(server.URL, "http://"), r, inventedIdentity) }()
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Error("held health fixture never entered")
		}
		settled := false
		select {
		case err := <-done:
			settled = true
			if err != ErrUnavailable {
				t.Error("held health response accepted")
			}
		case <-time.After(2 * time.Second):
			t.Error("health escaped parent deadline before peer release")
		}
		select {
		case <-peerCanceled:
		case <-time.After(time.Second):
			t.Error("peer did not observe health cancellation before release")
		}
		cancel()
		close(release)
		server.Close()
		if !settled {
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("held health fixture did not join")
			}
		}
	}
}
