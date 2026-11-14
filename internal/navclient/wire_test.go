package navclient

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/navauth"
)

var fixtureKey = [32]byte{91, 2, 38, 74, 18, 25, 56, 109, 58, 19, 78, 7, 69, 32, 29, 15, 94, 35, 83, 63, 21, 87, 73, 84, 49, 26, 37, 88, 66, 97, 31, 53}

const fixtureBuild = "invented-client-build"
const goodInfo = `{"root":"","data_dir":"/invented/data","hostname":"invented-host"}`
const fixtureCode = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"

// This fixture constructs the protocol frame independently of navauth's MAC
// helpers. It authenticates the client's exact request before replying.
func literalMAC(tag string, fields ...[]byte) string {
	b := []byte("aiq-nav-auth/1\x00")
	b = binary.BigEndian.AppendUint32(b, uint32(len(fields)+1))
	for _, f := range append([][]byte{[]byte(tag)}, fields...) {
		b = binary.BigEndian.AppendUint32(b, uint32(len(f)))
		b = append(b, f...)
	}
	m := hmac.New(sha256.New, fixtureKey[:])
	_, _ = m.Write(b)
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func fixtureHello() string {
	return `{"nav_version":1,"build":"` + fixtureBuild + `","server_time_ms":1000000}`
}

func signedHeaders(t *testing.T, r *http.Request, binding navauth.Binding, status int, body string) string {
	t.Helper()
	hash := sha256.Sum256([]byte(body))
	if r.URL.Path == "/api/nav/hello" {
		challenge, err := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Aiq-Challenge"))
		if err != nil || len(challenge) != 32 {
			t.Error("invalid challenge")
		}
		return "X-Aiq-Hello-Proof: " + literalMAC("hello", challenge, []byte("1"), []byte(binding.Build), []byte(strconv.Itoa(binding.Port)), []byte(strconv.Itoa(status)), hash[:]) + "\r\n"
	}
	parts := strings.Split(r.Header.Get("X-Aiq-Proof"), " ")
	if len(parts) != 4 || parts[0] != "v1" {
		t.Error("missing request proof")
		return ""
	}
	nonce, _ := base64.RawURLEncoding.DecodeString(parts[2])
	mac, _ := base64.RawURLEncoding.DecodeString(parts[3])
	requestBody, err := io.ReadAll(r.Body)
	if err != nil {
		t.Error("request read failed")
	}
	requestHash := sha256.Sum256(requestBody)
	want := literalMAC("request", []byte("1"), []byte(r.Method), []byte(r.URL.Path), []byte(strconv.Itoa(binding.Port)), []byte(binding.Build), []byte(parts[1]), nonce, requestHash[:])
	if want != parts[3] || len(nonce) != 32 || len(mac) != 32 || r.URL.RawQuery != "" || r.Host != "127.0.0.1:"+strconv.Itoa(binding.Port) {
		t.Error("client request disagrees with independent wire vector")
	}
	if r.Method == "POST" && r.Header.Get("Content-Type") != "application/json" {
		t.Error("wrong content type")
	}
	if r.URL.Path == "/api/nav/login" && string(requestBody) != `{"action":"issue"}` ||
		r.URL.Path == "/api/nav/revoke" && string(requestBody) != `{}` ||
		r.URL.Path == "/api/nav/info" && len(requestBody) != 0 {
		t.Error("wrong action body")
	}
	return "X-Aiq-Response-Proof: " + literalMAC("response", nonce, mac, []byte(strconv.Itoa(status)), hash[:]) + "\r\n"
}

type peerReply func(*http.Request, navauth.Binding) string

// Every operation uses the actual production Transport and two distinct native
// TCP connections. Cleanup unblocks all accepted reads before waiting.
func rawPeer(t *testing.T, reply peerReply) (Client, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := navauth.Binding{Port: l.Addr().(*net.TCPAddr).Port, Build: fixtureBuild}
	var requests, actions atomic.Int32
	var mu sync.Mutex
	var conns []net.Conn
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
				r, err := http.ReadRequest(bufio.NewReader(conn))
				if err != nil {
					return
				}
				defer r.Body.Close()
				requests.Add(1)
				if r.URL.Path != "/api/nav/hello" {
					actions.Add(1)
				}
				_, _ = io.WriteString(conn, reply(r, b))
			}()
		}
	}()
	t.Cleanup(func() {
		_ = l.Close()
		mu.Lock()
		for _, conn := range conns {
			_ = conn.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return newClient(fixtureKey, b), &requests, &actions
}

func basicReply(t *testing.T, r *http.Request, b navauth.Binding, body string, status int) string {
	return fmt.Sprintf("HTTP/1.1 %d Fixture\r\n", status) + signedHeaders(t, r, b, status, body) +
		"Content-Length: " + strconv.Itoa(len(body)) + "\r\nConnection: close\r\n\r\n" + body
}

func TestNativeOperationsIndependentMAC(t *testing.T) {
	c, requests, actions := rawPeer(t, func(r *http.Request, b navauth.Binding) string {
		body := goodInfo
		switch r.URL.Path {
		case "/api/nav/hello":
			body = fixtureHello()
		case "/api/nav/login":
			body = `{"code":"` + fixtureCode + `"}`
		case "/api/nav/revoke":
			body = `{"ok":true}`
		}
		return basicReply(t, r, b, body, 200)
	})
	i, err := c.Info(context.Background())
	if err != nil || i != (Info{"", "/invented/data", "invented-host"}) {
		t.Fatalf("Info: %#v %v", i, err)
	}
	code, err := c.Issue(context.Background())
	value, ok := code.Value()
	if err != nil || !ok || value != fixtureCode {
		t.Fatal("Issue failed")
	}
	if err := c.Revoke(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 6 || actions.Load() != 3 {
		t.Fatal("operation was retried or not sent")
	}
}

func TestNativeFramingAndHeaderBudget(t *testing.T) {
	for _, tc := range []struct {
		name string
		ok   bool
	}{
		{"single", true}, {"identical-length", true}, {"conflicting-length", false},
		{"chunk-over-length", true}, {"empty-chunk-trailer", true}, {"declared-trailer", false},
		{"fixed-declared-trailer", false}, {"late-trailer", false}, {"encoding", false},
		{"proof-duplicate", false}, {"proof-mixedcase", false}, {"truncated", false},
		{"header-at-cap", true}, {"header-over-cap", false},
		{"informational-below", true}, {"informational-over", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _, actions := rawPeer(t, func(r *http.Request, b navauth.Binding) string {
				if r.URL.Path != "/api/nav/hello" {
					return basicReply(t, r, b, goodInfo, 200)
				}
				body := fixtureHello()
				proof := signedHeaders(t, r, b, 200, body)
				head := "HTTP/1.1 200 Fixture\r\n" + proof
				length := "Content-Length: " + strconv.Itoa(len(body)) + "\r\n"
				switch tc.name {
				case "identical-length":
					length += length
				case "conflicting-length":
					length += "Content-Length: 1\r\n"
				case "chunk-over-length", "empty-chunk-trailer", "declared-trailer", "late-trailer":
					length += "Transfer-Encoding: chunked\r\n"
					if tc.name == "empty-chunk-trailer" {
						length += "Trailer:\r\n"
					}
					if tc.name == "declared-trailer" {
						length += "Trailer: X-Late\r\n"
					}
					body = fmt.Sprintf("%x\r\n%s\r\n0\r\n", len(body), body)
					if tc.name == "late-trailer" {
						body += "X-Late: invented\r\n"
					}
					body += "\r\n"
				case "fixed-declared-trailer":
					length += "Trailer: X-Late\r\n"
				case "encoding":
					length += "Content-Encoding: identity\r\n"
				case "proof-duplicate":
					head += proof
				case "proof-mixedcase":
					head += strings.Replace(proof, "X-Aiq-Hello-Proof", "x-aiq-hello-proof", 1)
				case "truncated":
					body = body[:len(body)-1]
				case "informational-below", "informational-over":
					block := "HTTP/1.1 103 Early Hints\r\nX-Fill: " + strings.Repeat("a", 4000) + "\r\n\r\n"
					n := 2
					if tc.name == "informational-over" {
						n = 17
					}
					head = strings.Repeat(block, n) + head
				}
				head += length + "Connection: close\r\n"
				if tc.name == "header-at-cap" || tc.name == "header-over-cap" {
					target := navauth.MaxResponse
					if tc.name == "header-over-cap" {
						target++
					}
					head += "X-Fill: " + strings.Repeat("a", target-len(head)-len("X-Fill: \r\n\r\n")) + "\r\n"
				}
				return head + "\r\n" + body
			})
			i, err := c.Info(context.Background())
			if (err == nil) != tc.ok {
				t.Fatalf("expected success=%v; got %v", tc.ok, err)
			}
			if tc.ok && actions.Load() != 1 || !tc.ok && (i != (Info{}) || actions.Load() != 0) {
				t.Fatal("framing failure leaked metadata or sent an action")
			}
		})
	}
}
