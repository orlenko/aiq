// Package navclient authenticates manual navigator operations against an
// explicitly supplied state record and expected build. It discovers no files,
// prints nothing, and never launches a browser or retries an action.
package navclient

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/orlenko/aiq/internal/navauth"
	"github.com/orlenko/aiq/internal/navtoken"
)

var (
	ErrUnavailable = errors.New("navigator request could not be authenticated or completed")
	ErrBusy        = errors.New("navigator login links are busy; try a new launch later")
	ErrInternal    = errors.New("navigator could not complete the request; try a new launch")
)

// Client keeps secret material only in an immutable closure, including when
// nested formatting walks otherwise unexported fields.
type Client struct {
	facts func() ([32]byte, navauth.Binding)
}

// Code is a single-use browser link secret, exposed only by Value.
type Code struct{ value func() string }

const redacted = "[navigator secret redacted]"

func (Client) String() string               { return redacted }
func (Client) GoString() string             { return redacted }
func (Client) Format(s fmt.State, _ rune)   { _, _ = io.WriteString(s, redacted) }
func (Client) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }
func (Code) String() string                 { return redacted }
func (Code) GoString() string               { return redacted }
func (Code) Format(s fmt.State, _ rune)     { _, _ = io.WriteString(s, redacted) }
func (Code) MarshalJSON() ([]byte, error)   { return []byte(`"` + redacted + `"`), nil }
func (c Code) Value() (string, bool) {
	if c.value == nil {
		return "", false
	}
	return c.value(), true
}

// New performs no I/O. A valid record is shape, not daemon liveness; each
// operation authenticates a new hello before sending its one action.
func New(record navtoken.Record, expectedBuild string) (Client, error) {
	port, ok := record.Port()
	key, hasKey := record.Key()
	if record.Status() != navtoken.On || !ok || !hasKey || port < 1 || port > 65535 ||
		expectedBuild == "" || len(expectedBuild) > 256 || !utf8.ValidString(expectedBuild) {
		return Client{}, ErrUnavailable
	}
	return newClient(key, navauth.Binding{Port: port, Build: strings.Clone(expectedBuild)}), nil
}

func newClient(key [32]byte, binding navauth.Binding) Client {
	return Client{facts: func() ([32]byte, navauth.Binding) { return key, binding }}
}

// Info contains only authenticated, completely validated metadata. Empty Root
// means that no project has been selected.
type Info struct{ Root, DataDir, Hostname string }

type operation uint8

const (
	info operation = iota
	issue
	revoke
)

func (c Client) Info(ctx context.Context) (Info, error) {
	r, err := c.run(ctx, info, dependencies{})
	return r.info, err
}
func (c Client) Issue(ctx context.Context) (Code, error) {
	r, err := c.run(ctx, issue, dependencies{})
	return r.code, err
}
func (c Client) Revoke(ctx context.Context) error {
	_, err := c.run(ctx, revoke, dependencies{})
	return err
}

// Instance-local synthetic seams; public operations always own a fresh native
// transport and use real monotonic time and cryptographic randomness.
type dependencies struct {
	random io.Reader
	now    func() time.Time
	rt     http.RoundTripper
}
type result struct {
	info Info
	code Code
}

func traceAllowed(ctx context.Context) bool {
	t := httptrace.ContextClientTrace(ctx)
	return t == nil || t.Got1xxResponse == nil
}

func (c Client) run(parent context.Context, op operation, d dependencies) (result, error) {
	if parent == nil || c.facts == nil || op > revoke || !traceAllowed(parent) {
		return result{}, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	if ctx.Err() != nil {
		return result{}, ErrUnavailable
	}
	if d.random == nil {
		d.random = rand.Reader
	}
	if d.now == nil {
		d.now = time.Now
	}
	if d.rt == nil {
		d.rt = &http.Transport{
			Proxy: nil, DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
			DisableCompression: true, DisableKeepAlives: true,
			MaxResponseHeaderBytes: navauth.MaxResponse,
		}
	}
	if closer, ok := d.rt.(interface{ CloseIdleConnections() }); ok {
		defer closer.CloseIdleConnections()
	}
	hc := &http.Client{Transport: d.rt, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	key, binding := c.facts()
	base := "http://127.0.0.1:" + strconv.Itoa(binding.Port)
	var challenge [32]byte
	if _, err := io.ReadFull(d.random, challenge[:]); err != nil || ctx.Err() != nil {
		return result{}, ErrUnavailable
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/api/nav/hello", nil)
	req.Header.Set("X-Aiq-Challenge", navauth.Encode32(challenge))
	hello, err := exchange(hc, req, "X-Aiq-Hello-Proof")
	if err != nil {
		return result{}, ErrUnavailable
	}
	want, macErr := navauth.HelloMAC(key, binding, challenge, hello.status, hello.body)
	if macErr != nil || !navauth.EqualMAC(want, hello.mac) || hello.status != http.StatusOK {
		return result{}, ErrUnavailable
	}
	fields, ok := object(hello.body, "nav_version", "build", "server_time_ms")
	serverTime, timeOK := integer(fields["server_time_ms"])
	protocol, protocolOK := integer(fields["nav_version"])
	build, buildOK := fields["build"].(string)
	if !ok || !timeOK || !protocolOK || protocol != navauth.Protocol || !buildOK || build != binding.Build {
		return result{}, ErrUnavailable
	}
	received := d.now()
	method, path, body := "GET", "/api/nav/info", []byte(nil)
	switch op {
	case issue:
		method, path, body = "POST", "/api/nav/login", []byte(`{"action":"issue"}`)
	case revoke:
		method, path, body = "POST", "/api/nav/revoke", []byte(`{}`)
	}
	var nonce [32]byte
	if _, err := io.ReadFull(d.random, nonce[:]); err != nil || ctx.Err() != nil {
		return result{}, ErrUnavailable
	}
	elapsed := d.now().Sub(received)
	if elapsed < 0 || serverTime > math.MaxInt64-elapsed.Milliseconds() {
		return result{}, ErrUnavailable
	}
	p := navauth.Proof{Timestamp: serverTime + elapsed.Milliseconds(), Nonce: nonce}
	p.MAC, err = navauth.RequestMAC(key, binding, method, path, p.Timestamp, nonce, body)
	if err != nil {
		return result{}, ErrUnavailable
	}
	req, _ = http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(body))
	req.Header.Set("X-Aiq-Proof", p.String())
	if method == "POST" {
		req.Header.Set("Content-Type", "application/json")
	}
	reply, err := exchange(hc, req, "X-Aiq-Response-Proof")
	if err != nil {
		return result{}, ErrUnavailable
	}
	want, macErr = navauth.ResponseMAC(key, p, reply.status, reply.body)
	if macErr != nil || !navauth.EqualMAC(want, reply.mac) || ctx.Err() != nil {
		return result{}, ErrUnavailable
	}
	decoded, err := decodeResult(op, reply.status, reply.body)
	if ctx.Err() != nil {
		return result{}, ErrUnavailable
	}
	return decoded, err
}

type wireReply struct {
	status int
	body   []byte
	mac    [32]byte
}

func exchange(c *http.Client, req *http.Request, proofHeader string) (out wireReply, err error) {
	if req.Context().Err() != nil || !traceAllowed(req.Context()) {
		return wireReply{}, ErrUnavailable
	}
	resp, doErr := c.Do(req)
	if resp != nil && resp.Body != nil {
		defer func() {
			if closeErr := resp.Body.Close(); closeErr != nil || err != nil {
				out, err = wireReply{}, ErrUnavailable
			}
		}()
	}
	if doErr != nil || resp == nil || resp.Body == nil || len(resp.Trailer) != 0 {
		return wireReply{}, ErrUnavailable
	}
	var proof string
	count := 0
	for k, values := range resp.Header {
		if strings.EqualFold(k, "Content-Encoding") {
			return wireReply{}, ErrUnavailable
		}
		if strings.EqualFold(k, "Trailer") {
			for _, v := range values {
				if strings.TrimSpace(v) != "" {
					return wireReply{}, ErrUnavailable
				}
			}
		}
		if strings.EqualFold(k, proofHeader) {
			count += len(values)
			if len(values) == 1 {
				proof = values[0]
			}
		}
	}
	mac, decodeErr := navauth.Decode32(proof)
	if count != 1 || decodeErr != nil {
		return wireReply{}, ErrUnavailable
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, navauth.MaxResponse+1))
	if readErr != nil || len(body) > navauth.MaxResponse || len(resp.Trailer) != 0 || req.Context().Err() != nil {
		return wireReply{}, ErrUnavailable
	}
	return wireReply{resp.StatusCode, body, mac}, nil
}

func decodeResult(op operation, status int, body []byte) (result, error) {
	if status != http.StatusOK {
		fields, ok := object(body, "error")
		code, isString := fields["error"].(string)
		if ok && isString {
			if status == 409 && code == "login_busy" {
				return result{}, ErrBusy
			}
			if status == 500 && code == "internal_error" {
				return result{}, ErrInternal
			}
		}
		return result{}, ErrUnavailable
	}
	switch op {
	case info:
		fields, ok := object(body, "root", "data_dir", "hostname")
		root, rOK := fields["root"].(string)
		data, dOK := fields["data_dir"].(string)
		host, hOK := fields["hostname"].(string)
		if ok && rOK && dOK && hOK {
			return result{info: Info{root, data, host}}, nil
		}
	case issue:
		fields, ok := object(body, "code")
		value, isString := fields["code"].(string)
		_, err := navauth.Decode32(value)
		if ok && isString && err == nil {
			owned := strings.Clone(value)
			return result{code: Code{value: func() string { return owned }}}, nil
		}
	case revoke:
		fields, ok := object(body, "ok")
		value, isBool := fields["ok"].(bool)
		if ok && isBool && value {
			return result{}, nil
		}
	}
	return result{}, ErrUnavailable
}
