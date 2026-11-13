package navclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/navauth"
	"github.com/orlenko/aiq/internal/navtoken"
)

func TestAuthenticatedJSONFidelity(t *testing.T) {
	for _, tc := range []struct {
		name, text, want string
		ok               bool
	}{
		{"empty", `""`, "", true}, {"pair", `"\ud83d\ude00"`, "😀", true},
		{"literal-replacement", `"�"`, "�", true}, {"escaped-replacement", `"\ufffd"`, "�", true},
		{"escaped-backslash", `"\\ud800"`, `\ud800`, true}, {"escaped-quote", `"a\"b"`, `a"b`, true},
		{"lone-high", `"\ud800"`, "", false}, {"lone-low", `"\udc00"`, "", false},
		{"high-high", `"\ud800\ud800"`, "", false}, {"high-text", `"\ud800text"`, "", false},
		{"bad-utf8", "\"" + string([]byte{0xff}) + "\"", "", false},
		{"null", `null`, "", false}, {"bool", `true`, "", false}, {"number", `1`, "", false},
		{"array", `[]`, "", false}, {"object", `{}`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"root":` + tc.text + `,"data_dir":"invented","hostname":"invented"}`
			c, _, actions := rawPeer(t, func(r *http.Request, b navauth.Binding) string {
				if r.URL.Path == "/api/nav/hello" {
					return basicReply(t, r, b, fixtureHello(), 200)
				}
				return basicReply(t, r, b, body, 200)
			})
			i, err := c.Info(context.Background())
			if (err == nil) != tc.ok || tc.ok && i.Root != tc.want || !tc.ok && i != (Info{}) || actions.Load() != 1 {
				t.Fatalf("lossless typed validation failed: success=%v expected=%v", err == nil, tc.ok)
			}
		})
	}
	for _, body := range []string{
		`{"root":"x","root":"y","data_dir":"z","hostname":"h"}`,
		`{"root":"x","\u0072oot":"y","data_dir":"z","hostname":"h"}`,
		`{"Root":"x","data_dir":"z","hostname":"h"}`,
		`{"root":"x","data_dir":"z","hostname":"h","extra":true}`,
		`{"root":"x","data_dir":"z"}`, goodInfo + `{}`, goodInfo + ` null`,
		`{"\ud800":"x","data_dir":"z","hostname":"h"}`,
	} {
		c, _, _ := rawPeer(t, func(r *http.Request, b navauth.Binding) string {
			if r.URL.Path == "/api/nav/hello" {
				return basicReply(t, r, b, fixtureHello(), 200)
			}
			return basicReply(t, r, b, body, 200)
		})
		if i, err := c.Info(context.Background()); err != ErrUnavailable || i != (Info{}) {
			t.Fatal("closed schema accepted or partial metadata published")
		}
	}
}

func TestBadAuthenticatedHelloNeverActs(t *testing.T) {
	for _, stamp := range []string{"null", `"1000000"`, "1.0", "1e6", "-1", "-0", "9223372036854775808"} {
		body := `{"nav_version":1,"build":"` + fixtureBuild + `","server_time_ms":` + stamp + `}`
		c, _, actions := rawPeer(t, func(r *http.Request, b navauth.Binding) string { return basicReply(t, r, b, body, 200) })
		if i, err := c.Info(context.Background()); err != ErrUnavailable || i != (Info{}) || actions.Load() != 0 {
			t.Fatal("malformed authenticated time used")
		}
	}
	for _, body := range []string{
		strings.Replace(fixtureHello(), `"nav_version":1`, `"nav_version":null`, 1),
		strings.Replace(fixtureHello(), `"nav_version":1`, `"nav_version":2`, 1),
		strings.Replace(fixtureHello(), fixtureBuild, `\ud800`, 1),
		strings.Replace(fixtureHello(), fixtureBuild, "other-build", 1),
		fixtureHello() + `{}`,
	} {
		c, _, actions := rawPeer(t, func(r *http.Request, b navauth.Binding) string { return basicReply(t, r, b, body, 200) })
		if _, err := c.Issue(context.Background()); err != ErrUnavailable || actions.Load() != 0 {
			t.Fatal("malformed authenticated hello caused an action")
		}
	}
}

func TestMACBeforeUseAndNoReplay(t *testing.T) {
	for _, change := range []string{"body", "status", "proof", "foreign-build", "foreign-port", "response-request-mac", "response-nonce", "response-status", "response-body", "lost-action"} {
		t.Run(change, func(t *testing.T) {
			c, requests, actions := rawPeer(t, func(r *http.Request, b navauth.Binding) string {
				body := fixtureHello()
				if r.URL.Path != "/api/nav/hello" {
					if change == "lost-action" {
						return ""
					}
					body = goodInfo
				}
				if change == "foreign-build" {
					b.Build = "different-build"
				}
				if change == "foreign-port" {
					b.Port++
				}
				reply := basicReply(t, r, b, body, 200)
				if r.URL.Path != "/api/nav/hello" && (change == "response-request-mac" || change == "response-nonce") {
					p, _ := navauth.ParseProof(r.Header.Get("X-Aiq-Proof"))
					if change == "response-nonce" {
						p.Nonce[0] ^= 1
					} else {
						p.MAC[0] ^= 1
					}
					bad, _ := navauth.ResponseMAC(fixtureKey, p, 200, []byte(body))
					start := strings.Index(reply, "X-Aiq-Response-Proof: ") + len("X-Aiq-Response-Proof: ")
					reply = reply[:start] + navauth.Encode32(bad) + reply[start+43:]
				}
				if r.URL.Path != "/api/nav/hello" {
					if change == "response-status" {
						reply = strings.Replace(reply, "200 Fixture", "201 Fixture", 1)
					}
					if change == "response-body" {
						reply = strings.Replace(reply, "invented-host", "invented-most", 1)
					}
				}
				if r.URL.Path == "/api/nav/hello" {
					switch change {
					case "body":
						reply = strings.Replace(reply, `1000000`, `1000001`, 1)
					case "status":
						reply = strings.Replace(reply, "200 Fixture", "201 Fixture", 1)
					case "proof":
						start := strings.Index(reply, "X-Aiq-Hello-Proof: ") + len("X-Aiq-Hello-Proof: ")
						reply = reply[:start] + strings.Repeat("A", 43) + reply[start+43:]
					}
				}
				return reply
			})
			i, err := c.Info(context.Background())
			wantActions := int32(0)
			if change == "lost-action" || strings.HasPrefix(change, "response-") {
				wantActions = 1
			}
			if err != ErrUnavailable || i != (Info{}) || actions.Load() != wantActions || requests.Load() != wantActions+1 {
				t.Fatal("invalid authority accepted or request replayed")
			}
		})
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type countedReader struct{ calls int }

func (r *countedReader) Read(b []byte) (int, error) { r.calls++; clear(b); return len(b), nil }

func TestInheritedTraceAndCanceledParentRefuseBeforeIO(t *testing.T) {
	c := newClient(fixtureKey, navauth.Binding{54321, fixtureBuild})
	var entropy countedReader
	network, hook := 0, 0
	d := dependencies{random: &entropy, rt: roundTrip(func(*http.Request) (*http.Response, error) { network++; return nil, errors.New("invented") })}
	traced := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{Got1xxResponse: func(int, textproto.MIMEHeader) error { hook++; return nil }})
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, ctx := range []context.Context{traced, canceled} {
		if r, err := c.run(ctx, issue, d); err != ErrUnavailable || r.info != (Info{}) || r.code.value != nil {
			t.Fatal("entry refusal failed")
		}
	}
	if entropy.calls != 0 || network != 0 || hook != 0 {
		t.Fatal("refused context performed I/O")
	}
}

func TestConstructorAndSecretFormatting(t *testing.T) {
	if _, err := New(navtoken.Record{}, fixtureBuild); err != ErrUnavailable {
		t.Fatal("invalid state accepted")
	}
	if runtime.GOOS != "windows" {
		path := filepath.Join(t.TempDir(), "invented-state")
		if err := os.WriteFile(path, []byte("on 123 456 54321 "+navauth.Encode32(fixtureKey)+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		r, err := navtoken.Read(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, build := range []string{"", strings.Repeat("x", 257), string([]byte{0xff})} {
			if _, err := New(r, build); err != ErrUnavailable {
				t.Fatal("invalid build accepted")
			}
		}
		c, err := New(r, fixtureBuild)
		if err != nil || c.facts == nil {
			t.Fatal("explicit private record not accepted")
		}
		copyKey, _ := r.Key()
		copyKey[0]++
		key, _ := c.facts()
		if key != fixtureKey {
			t.Fatal("client did not own key")
		}
	}
	called := 0
	c := Client{facts: func() ([32]byte, navauth.Binding) { called++; return fixtureKey, navauth.Binding{} }}
	code := Code{value: func() string { called++; return fixtureCode }}
	for _, v := range []any{c, &c, code, &code, struct {
		client Client
		code   Code
	}{c, code}, []Code{code}, map[string]Client{"invented": c}} {
		for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
			out := fmt.Sprintf(format, v)
			if strings.Contains(out, fixtureCode) || strings.Contains(out, navauth.Encode32(fixtureKey)) || strings.Contains(out, fmt.Sprint(fixtureKey)) {
				t.Fatal("secret leaked by formatting")
			}
		}
		if _, err := json.Marshal(v); err != nil {
			t.Fatal(err)
		}
	}
	if called != 0 {
		t.Fatal("formatting called a secret accessor")
	}
	// Negative control: ordinary nested private strings really are observable.
	if out := fmt.Sprintf("%v", struct{ code string }{fixtureCode}); !strings.Contains(out, fixtureCode) {
		t.Fatal("formatting negative control is ineffective")
	}
	if value, ok := code.Value(); !ok || value != fixtureCode || called != 1 {
		t.Fatal("explicit accessor failed")
	}
}

func TestSignedErrorsAndTypedCode(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   error
	}{
		{409, `{"error":"login_busy"}`, ErrBusy}, {500, `{"error":"internal_error"}`, ErrInternal},
		{409, `{"error":"other"}`, ErrUnavailable}, {401, `{"error":"login_busy"}`, ErrUnavailable},
		{500, `{"error":null}`, ErrUnavailable}, {200, `{"code":null}`, ErrUnavailable},
		{200, `{"code":"not-a-code"}`, ErrUnavailable}, {200, `{"code":"` + fixtureCode + `","extra":true}`, ErrUnavailable},
	} {
		c, _, _ := rawPeer(t, func(r *http.Request, b navauth.Binding) string {
			if r.URL.Path == "/api/nav/hello" {
				return basicReply(t, r, b, fixtureHello(), 200)
			}
			return basicReply(t, r, b, tc.body, tc.status)
		})
		if code, err := c.Issue(context.Background()); err != tc.want || code.value != nil {
			t.Fatal("signed failure classification or zero secret failed")
		}
	}
	for _, body := range []string{`{"ok":null}`, `{"ok":false}`, `{"ok":"true"}`, `{"ok":1}`} {
		if _, err := decodeResult(revoke, 200, []byte(body)); err != ErrUnavailable {
			t.Fatal("nontrue revoke accepted")
		}
	}
}

func TestAuthenticatedServerTimeOverflowAndElapsed(t *testing.T) {
	for _, backwards := range []bool{false, true} {
		c, _, actions := rawPeer(t, func(r *http.Request, b navauth.Binding) string {
			body := strings.Replace(fixtureHello(), "1000000", strconv.FormatInt(math.MaxInt64, 10), 1)
			return basicReply(t, r, b, body, 200)
		})
		calls := 0
		base := time.Now()
		_, err := c.run(context.Background(), info, dependencies{now: func() time.Time {
			calls++
			if calls == 1 {
				return base
			}
			if backwards {
				return base.Add(-time.Millisecond)
			}
			return base.Add(time.Millisecond)
		}})
		if err != ErrUnavailable || actions.Load() != 0 {
			t.Fatal("invalid authority-time arithmetic sent an action")
		}
	}
}
