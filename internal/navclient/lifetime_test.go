package navclient

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/navauth"
)

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(b []byte) (int, error) { return f(b) }

type fixtureBody struct {
	io.Reader
	close func() error
}

func (b fixtureBody) Close() error { return b.close() }

func TestBoundedBodiesCompleteEOF(t *testing.T) {
	for _, hello := range []bool{false, true} {
		for _, size := range []int{navauth.MaxResponse, navauth.MaxResponse + 1} {
			c, _, actions := rawPeer(t, func(r *http.Request, b navauth.Binding) string {
				body := goodInfo
				if r.URL.Path == "/api/nav/hello" {
					body = fixtureHello()
				}
				if hello == (r.URL.Path == "/api/nav/hello") {
					body += strings.Repeat(" ", size-len(body))
				}
				return basicReply(t, r, b, body, 200)
			})
			i, err := c.Info(context.Background())
			if size == navauth.MaxResponse {
				if err != nil || i.Hostname != "invented-host" || actions.Load() != 1 {
					t.Fatal("bounded complete response refused")
				}
			} else if err != ErrUnavailable || i != (Info{}) || hello && actions.Load() != 0 {
				t.Fatal("overlimit response published or used")
			}
		}
	}
}

func TestReadCloseAndOriginalTrailerFailures(t *testing.T) {
	for _, phase := range []string{"hello", "action"} {
		for _, failure := range []string{"read", "close", "late-trailer", "proof-alias", "unsigned"} {
			t.Run(phase+"-"+failure, func(t *testing.T) {
				requests, closed := 0, 0
				c := newClient(fixtureKey, navauth.Binding{Port: 54321, Build: fixtureBuild})
				rt := roundTrip(func(r *http.Request) (*http.Response, error) {
					requests++
					body := goodInfo
					if requests == 1 {
						body = fixtureHello()
					}
					_, b := c.facts()
					resp, err := http.ReadResponse(bufio.NewReader(strings.NewReader(basicReply(t, r, b, body, 200))), r)
					if err != nil {
						t.Fatal(err)
					}
					original := resp.Body
					reader := io.Reader(original)
					active := phase == "hello" && requests == 1 || phase == "action" && requests == 2
					if active {
						switch failure {
						case "read":
							reader = readerFunc(func([]byte) (int, error) { return 0, errors.New("invented private cause") })
						case "late-trailer":
							reader = readerFunc(func(p []byte) (int, error) {
								n, e := original.Read(p)
								if e == io.EOF {
									resp.Trailer = http.Header{"X-Late": {"invented"}}
								}
								return n, e
							})
						case "proof-alias":
							name := "X-Aiq-Response-Proof"
							if requests == 1 {
								name = "X-Aiq-Hello-Proof"
							}
							resp.Header[strings.ToLower(name)] = []string{resp.Header.Get(name)}
						case "unsigned":
							resp.Header.Del("X-Aiq-Hello-Proof")
							resp.Header.Del("X-Aiq-Response-Proof")
						}
					}
					resp.Body = fixtureBody{Reader: reader, close: func() error {
						closed++
						_ = original.Close()
						if active && failure == "close" {
							return errors.New("invented close cause")
						}
						return nil
					}}
					return resp, nil
				})
				r, err := c.run(context.Background(), info, dependencies{rt: rt})
				want := 1
				if phase == "action" {
					want = 2
				}
				if err != ErrUnavailable || r.info != (Info{}) || requests != want || closed != requests {
					t.Fatal("failure published metadata, retried or missed Close")
				}
			})
		}
	}
}

func TestSharedDeadlineAndCancellationAfterEntropy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reads, requests := 0, 0
	c := newClient(fixtureKey, navauth.Binding{Port: 54321, Build: fixtureBuild})
	r, err := c.run(ctx, issue, dependencies{
		random: readerFunc(func(p []byte) (int, error) {
			reads++
			clear(p)
			if reads == 2 {
				cancel()
			}
			return len(p), nil
		}),
		rt: roundTrip(func(req *http.Request) (*http.Response, error) {
			requests++
			deadline, ok := req.Context().Deadline()
			if !ok || time.Until(deadline) > 10*time.Second {
				t.Fatal("operation deadline missing")
			}
			_, b := c.facts()
			return http.ReadResponse(bufio.NewReader(strings.NewReader(basicReply(t, req, b, fixtureHello(), 200))), req)
		}),
	})
	if err != ErrUnavailable || r.code.value != nil || reads != 2 || requests != 1 {
		t.Fatal("cancellation after entropy did not prevent the action")
	}
	if r, err := c.run(context.Background(), issue, dependencies{random: strings.NewReader("short")}); err != ErrUnavailable || r.code.value != nil {
		t.Fatal("short entropy accepted")
	}
	parent, end := context.WithTimeout(context.Background(), time.Second)
	defer end()
	parentDeadline, _ := parent.Deadline()
	var first time.Time
	requests = 0
	_, err = c.run(parent, info, dependencies{rt: roundTrip(func(req *http.Request) (*http.Response, error) {
		requests++
		deadline, _ := req.Context().Deadline()
		if deadline != parentDeadline || !first.IsZero() && deadline != first {
			t.Fatal("hello/action changed the single parent budget")
		}
		first = deadline
		body := goodInfo
		if requests == 1 {
			body = fixtureHello()
		}
		_, b := c.facts()
		return http.ReadResponse(bufio.NewReader(strings.NewReader(basicReply(t, req, b, body, 200))), req)
	})})
	if err != nil || requests != 2 {
		t.Fatal("deadline positive control failed")
	}
}

func TestNativeRedirectRefusalAndOwnedTransport(t *testing.T) {
	var foreign atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { foreign.Add(1) }))
	defer target.Close()
	t.Setenv("HTTP_PROXY", target.URL)
	t.Setenv("http_proxy", target.URL)
	t.Setenv("ALL_PROXY", target.URL)
	t.Setenv("NO_PROXY", "")
	for _, phase := range []string{"hello", "action"} {
		c, _, actions := rawPeer(t, func(r *http.Request, b navauth.Binding) string {
			if phase == "hello" || r.URL.Path != "/api/nav/hello" {
				return "HTTP/1.1 302 Redirect\r\nLocation: " + target.URL + "/invented\r\nSet-Cookie: invented=never\r\nContent-Length: 0\r\n\r\n"
			}
			return basicReply(t, r, b, fixtureHello(), 200)
		})
		if i, err := c.Info(context.Background()); err != ErrUnavailable || i != (Info{}) {
			t.Fatal("redirect accepted")
		}
		if foreign.Load() != 0 || phase == "hello" && actions.Load() != 0 {
			t.Fatal("proxy or redirect received a request")
		}
	}
}
