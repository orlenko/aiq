package navclient

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/navauth"
)

func TestProtocolUpgradeRefusedBeforeBodyRead(t *testing.T) {
	for _, phase := range []string{"hello", "action"} {
		t.Run(phase, func(t *testing.T) {
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			binding := navauth.Binding{Port: listener.Addr().(*net.TCPAddr).Port, Build: fixtureBuild}
			upgraded, peerClosed := make(chan struct{}), make(chan error, 1)
			var actions atomic.Int32
			var mu sync.Mutex
			var conns []net.Conn
			stopping := false
			var workers sync.WaitGroup
			workers.Add(1)
			var stopOnce sync.Once
			stop := func() {
				stopOnce.Do(func() {
					_ = listener.Close()
					mu.Lock()
					stopping = true
					for _, conn := range conns {
						_ = conn.Close()
					}
					mu.Unlock()
					workers.Wait()
				})
			}
			// Cleanup exists before workers; even the old indefinite client read
			// is released and joined after its semantic failure is recorded.
			t.Cleanup(stop)
			go func() {
				defer workers.Done()
				for {
					conn, err := listener.Accept()
					if err != nil {
						return
					}
					mu.Lock()
					if stopping {
						mu.Unlock()
						_ = conn.Close()
						return
					}
					conns = append(conns, conn)
					workers.Add(1)
					mu.Unlock()
					go func() {
						defer workers.Done()
						defer conn.Close()
						_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
						r, err := http.ReadRequest(bufio.NewReader(conn))
						if err != nil {
							return
						}
						defer r.Body.Close()
						if r.URL.Path != "/api/nav/hello" {
							actions.Add(1)
						}
						if phase == "action" && r.URL.Path == "/api/nav/hello" {
							_, _ = io.WriteString(conn, basicReply(t, r, binding, fixtureHello(), 200))
							return
						}
						_, _ = io.ReadAll(io.LimitReader(r.Body, navauth.MaxRequest+1))
						name := "X-Aiq-Hello-Proof"
						if r.URL.Path != "/api/nav/hello" {
							name = "X-Aiq-Response-Proof"
						}
						// Canonical zero MAC reaches the original body read. No
						// forged authentication or arbitrary callback is involved.
						_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: invented\r\n"+name+": "+strings.Repeat("A", 43)+"\r\n\r\n")
						close(upgraded)
						var one [1]byte
						_, err = conn.Read(one[:])
						peerClosed <- err
					}()
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			type outcome struct {
				result result
				err    error
			}
			returned := make(chan outcome, 1)
			go func() {
				r, e := newClient(fixtureKey, binding).run(ctx, issue, dependencies{})
				returned <- outcome{r, e}
			}()
			select {
			case <-upgraded:
			case <-time.After(2 * time.Second):
				t.Error("native upgrade was not delivered")
				stop()
			}
			var out outcome
			select {
			case out = <-returned:
			case <-ctx.Done():
				select {
				case out = <-returned:
				case <-time.After(200 * time.Millisecond):
					t.Error("native upgrade body outlived the operation deadline")
					stop() // Failure FIRST; this induced EOF cannot pass.
					select {
					case out = <-returned:
					case <-time.After(time.Second):
						t.Fatal("client did not join after cleanup")
					}
				}
			}
			if out.err != ErrUnavailable || out.result.info != (Info{}) || out.result.code.value != nil {
				t.Error("upgrade exposed a result")
			}
			want := int32(0)
			if phase == "action" {
				want = 1
			}
			if actions.Load() != want {
				t.Error("upgrade caused an action or replay")
			}
			select {
			case err := <-peerClosed:
				if err == nil {
					t.Error("peer received unexpected data instead of closure")
				}
				if e, ok := err.(net.Error); ok && e.Timeout() {
					t.Error("peer close was only a deadline")
				}
			case <-time.After(300 * time.Millisecond):
				t.Error("client did not close the upgraded socket before cleanup")
			}
		})
	}
}
