package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/orlenko/aiq/internal/diagnostic"
	"github.com/orlenko/aiq/internal/navauth"
	"github.com/orlenko/aiq/internal/transcript"
)

// This adapter is unmounted. A later generation owner supplies the listener's
// actual key/port/build, publication/retirement and registered outer envelope.
type navAuthInfo struct {
	Root     string `json:"root"`
	DataDir  string `json:"data_dir"`
	Hostname string `json:"hostname"`
}
type navAuthResult struct {
	status      int
	kind, value string
}
type navAuthReply struct {
	status       int
	body         []byte
	proof, hello string
}
type navAuthAdapter struct {
	state    *navauth.State
	build    string
	info     navAuthInfo
	issue    func(*navauth.Facts) (string, error)
	exchange func(string) (string, error)
	revoke   func(*navauth.Facts) error
	encode   func(navAuthResult) ([]byte, error)
	sign     func(*navauth.Facts, int, []byte) ([32]byte, error)
}

var _ NavVerifier = (*navAuthAdapter)(nil)
var errNavAuth = errors.New("navigator authentication unavailable")

func newNavAuthAdapter(key [32]byte, binding navauth.Binding, info navAuthInfo) (*navAuthAdapter, error) {
	remaining := navauth.MaxResponse
	for _, s := range []string{info.Root, info.DataDir, info.Hostname} {
		if !utf8.ValidString(s) || len(s) > remaining {
			return nil, errNavAuth
		}
		remaining -= len(s)
	}
	info = navAuthInfo{strings.Clone(info.Root), strings.Clone(info.DataDir), strings.Clone(info.Hostname)}
	body, err := json.Marshal(info)
	if err != nil || len(body) > navauth.MaxResponse {
		return nil, errNavAuth
	}
	state, err := navauth.New(key, binding)
	if err != nil {
		return nil, errNavAuth
	}
	a := &navAuthAdapter{state: state, build: strings.Clone(binding.Build), info: info}
	a.issue = state.Issue
	a.exchange = state.Exchange
	a.revoke = state.Revoke
	a.sign = state.SignResponse
	a.encode = a.encodeResult
	return a, nil
}
func (a *navAuthAdapter) Close() {
	if a != nil && a.state != nil {
		a.state.Close()
	}
}
func (a *navAuthAdapter) Credential(value string) bool {
	return a != nil && a.state != nil && a.state.Credential(value)
}

type navAuthAttemptKey struct{}
type navAuthBodyKey struct{}
type navAuthFactsKey struct{}
type navAuthLoginKey struct{}
type navAuthLoginInvocation struct{ interrupted bool }
type navAuthBody struct {
	owner                      *navAuthAdapter
	busy, valid                bool
	invalid, proofBusy         bool
	chunked                    bool
	input                      *navInput
	method, path, action, code string
	raw                        []byte
}

func (b *navAuthBody) parserReady(r *http.Request) bool {
	return !b.chunked || b.input.trailersEmpty(r)
}

type navAuthVerified struct {
	owner *navAuthAdapter
	facts *navauth.Facts
	body  *navAuthBody
}

// Count aliases too: a direct Request can contain differently cased map keys.
func navAuthHeader(h http.Header, name string) (string, int, bool) {
	value := ""
	count := 0
	present := false
	for k, values := range h {
		if strings.EqualFold(k, name) {
			present = true
			count += len(values)
			if len(values) > 0 {
				value = values[0]
			}
		}
	}
	return value, count, present
}
func navAuthFraming(r *http.Request) bool {
	if r == nil || r.URL == nil {
		return false
	}
	u := r.URL
	if u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || r.RequestURI != u.Path {
		return false
	}
	valid := r.Method == "GET" && (u.Path == "/api/nav/hello" || u.Path == "/api/nav/info") || r.Method == "POST" && (u.Path == "/api/nav/login" || u.Path == "/api/nav/revoke")
	if !valid || r.ContentLength < -1 || r.ContentLength > navauth.MaxRequest || len(r.Trailer) != 0 {
		return false
	}
	if _, _, present := navAuthHeader(r.Header, "Content-Encoding"); present {
		return false
	}
	if _, _, present := navAuthHeader(r.Header, "Trailer"); present {
		return false
	}
	contentType, count, present := navAuthHeader(r.Header, "Content-Type")
	if r.Method == "GET" {
		return !present && r.ContentLength <= 0 && len(r.TransferEncoding) == 0
	}
	if count != 1 {
		return false
	}
	media, params, err := mime.ParseMediaType(contentType)
	if err != nil || media != "application/json" || len(params) > 1 {
		return false
	}
	for k, v := range params {
		if k != "charset" || !strings.EqualFold(v, "utf-8") {
			return false
		}
	}
	return len(r.TransferEncoding) == 0 || len(r.TransferEncoding) == 1 && r.TransferEncoding[0] == "chunked" && r.ContentLength == -1
}
func navAuthObject(raw []byte) (map[string]string, bool) {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, false
	}
	values := make(map[string]string, 2)
	for d.More() {
		k, e := d.Token()
		if e != nil {
			return nil, false
		}
		key, ok := k.(string)
		if !ok || (key != "action" && key != "code") {
			return nil, false
		}
		if _, exists := values[key]; exists {
			return nil, false
		}
		var v string
		if d.Decode(&v) != nil {
			return nil, false
		}
		values[key] = v
	}
	if token, err = d.Token(); err != nil || token != json.Delim('}') {
		return nil, false
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, false
	}
	return values, true
}
func (a *navAuthAdapter) ownedBody(r *http.Request) (*navAuthBody, bool) {
	if r == nil {
		return nil, false
	}
	if cached, ok := r.Context().Value(navAuthBodyKey{}).(*navAuthBody); ok {
		if cached != nil && cached.busy {
			cached.invalid = true
		}
		return cached, cached != nil && cached.owner == a && !cached.busy && !cached.invalid && cached.valid && cached.parserReady(r)
	}
	if !navAuthFraming(r) {
		return nil, false
	}
	marker := r.Context().Value(navInputKey{})
	input, _ := marker.(*navInput)
	// Ingress provenance survives mutable transfer fields and request copies.
	chunked := len(r.TransferEncoding) != 0 || marker != nil
	if chunked {
		if !input.open(r) {
			return nil, false
		}
	}
	b := &navAuthBody{owner: a, busy: true, method: r.Method, path: r.URL.Path, chunked: chunked, input: input}
	*r = *r.WithContext(context.WithValue(r.Context(), navAuthBodyKey{}, b))
	defer func() { b.busy = false }()
	length := r.ContentLength
	limit := int64(navauth.MaxRequest + 1)
	if b.method == "GET" {
		limit = 1
	}
	var err error
	if r.Body != nil {
		b.raw, err = io.ReadAll(io.LimitReader(r.Body, limit))
	}
	if err != nil || b.invalid || !b.parserReady(r) || len(r.Trailer) != 0 || len(b.raw) > navauth.MaxRequest || length >= 0 && int64(len(b.raw)) != length || !utf8.Valid(b.raw) {
		return b, false
	}
	if b.method == "GET" {
		b.valid = len(b.raw) == 0
		return b, b.valid
	}
	values, ok := navAuthObject(b.raw)
	if !ok {
		return b, false
	}
	if b.path == "/api/nav/revoke" {
		b.valid = len(values) == 0
		return b, b.valid
	}
	b.action, b.code = values["action"], values["code"]
	if b.action == "issue" {
		b.valid = len(values) == 1
	} else if b.action == "exchange" && len(values) == 2 {
		_, err = navauth.Decode32(b.code)
		b.valid = err == nil
	}
	return b, b.valid
}

func (a *navAuthAdapter) Proof(r *http.Request) bool {
	if a == nil || a.state == nil || r == nil {
		return false
	}
	if r.Context().Value(navAuthAttemptKey{}) != nil {
		return false
	}
	*r = *r.WithContext(context.WithValue(r.Context(), navAuthAttemptKey{}, true))
	header, count, _ := navAuthHeader(r.Header, "X-Aiq-Proof")
	if count != 1 {
		return false
	}
	proof, err := navauth.ParseProof(header)
	if err != nil {
		return false
	}
	if cached, ok := r.Context().Value(navAuthBodyKey{}).(*navAuthBody); ok && cached != nil && cached.owner == a && cached.busy {
		cached.proofBusy, cached.invalid = true, true
	}
	b, ok := a.ownedBody(r)
	if !ok || b.path == "/api/nav/hello" || b.path == "/api/nav/login" && b.action != "issue" || r.Context().Err() != nil {
		return false
	}
	facts, err := a.state.Admit(b.method, b.path, proof, b.raw)
	if err != nil {
		return false
	}
	*r = *r.WithContext(context.WithValue(r.Context(), navAuthFactsKey{}, &navAuthVerified{a, facts, b}))
	r.Body = io.NopCloser(bytes.NewReader(b.raw))
	return true
}
func (a *navAuthAdapter) verified(r *http.Request, path string) (*navAuthVerified, bool) {
	if r == nil {
		return nil, false
	}
	v, ok := r.Context().Value(navAuthFactsKey{}).(*navAuthVerified)
	return v, ok && v != nil && v.owner == a && v.facts != nil && v.body != nil && v.body.owner == a && v.body.valid && v.body.path == path && v.body.parserReady(r)
}

func (a *navAuthAdapter) encodeResult(v navAuthResult) ([]byte, error) {
	switch v.kind {
	case "info":
		return json.Marshal(a.info)
	case "code":
		return json.Marshal(struct {
			Code string `json:"code"`
		}{v.value})
	case "credential":
		return json.Marshal(struct {
			Credential string `json:"credential"`
		}{v.value})
	case "ok":
		return []byte(`{"ok":true}`), nil
	case "error":
		return json.Marshal(struct {
			Error string `json:"error"`
		}{v.value})
	}
	return nil, errNavAuth
}
func (a *navAuthAdapter) signed(r *http.Request, v *navAuthVerified, action func() navAuthResult) (out navAuthReply) {
	done, signing := false, false
	defer func() {
		if done {
			return
		}
		value := recover()
		if value == http.ErrAbortHandler {
			panic(http.ErrAbortHandler)
		}
		// Any secondary recovery failure is terminal, including panic(nil).
		reduced := false
		defer func() {
			if !reduced {
				recover()
				panic(http.ErrAbortHandler)
			}
		}()
		origin := diagnostic.Origin{}
		if err, ok := value.(error); ok {
			origin = transcript.PanicOrigin(err)
		}
		if !origin.Present() {
			origin = diagnostic.Capture(value)
		}
		diagnostic.Note(r.Context(), origin)
		if signing {
			panic(http.ErrAbortHandler)
		}
		out = navAuthReply{status: 500, body: []byte(`{"error":"internal_error"}`)}
		mac, err := a.sign(v.facts, out.status, out.body)
		if err != nil {
			panic(http.ErrAbortHandler)
		}
		out.proof = navauth.Encode32(mac)
		reduced = true
	}()
	result := action()
	body, err := a.encode(result)
	if err != nil || len(body) > navauth.MaxResponse {
		result.status = 500
		body = []byte(`{"error":"internal_error"}`)
	}
	out = navAuthReply{status: result.status, body: body}
	signing = true
	mac, err := a.sign(v.facts, out.status, out.body)
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	out.proof = navauth.Encode32(mac)
	done = true
	return
}

// Output is terminal from BEFORE the first writer call. The outer production
// observer/lifetime composition is separate; this function never retries output.
func navAuthWrite(w http.ResponseWriter, r *http.Request, out navAuthReply) {
	done := false
	defer func() {
		if done {
			return
		}
		value := recover()
		if value == http.ErrAbortHandler {
			panic(http.ErrAbortHandler)
		}
		defer func() { recover(); panic(http.ErrAbortHandler) }()
		origin := diagnostic.Origin{}
		if err, ok := value.(error); ok {
			origin = transcript.PanicOrigin(err)
		}
		if !origin.Present() {
			origin = diagnostic.Capture(value)
		}
		diagnostic.Note(r.Context(), origin)
	}()
	h := w.Header()
	h.Del("X-Aiq-Response-Proof")
	h.Del("X-Aiq-Hello-Proof")
	h.Del("Content-Encoding")
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(len(out.body)))
	if out.proof != "" {
		h.Set("X-Aiq-Response-Proof", out.proof)
	}
	if out.hello != "" {
		h.Set("X-Aiq-Hello-Proof", out.hello)
	}
	w.WriteHeader(out.status)
	n, err := w.Write(out.body)
	if err != nil || n != len(out.body) {
		panic(http.ErrAbortHandler)
	}
	done = true
}
func navAuthError(status int, code string) navAuthReply {
	return navAuthReply{status: status, body: []byte(`{"error":"` + code + `"}`)}
}
func (a *navAuthAdapter) Hello(w http.ResponseWriter, r *http.Request) {
	b, ok := a.ownedBody(r)
	if !ok || b.path != "/api/nav/hello" {
		navAuthWrite(w, r, navAuthError(400, "invalid_request"))
		return
	}
	header, count, present := navAuthHeader(r.Header, "X-Aiq-Challenge")
	if !present {
		body, _ := json.Marshal(struct {
			Version int    `json:"nav_version"`
			Build   string `json:"build"`
		}{navauth.Protocol, a.build})
		navAuthWrite(w, r, navAuthReply{status: 200, body: body})
		return
	}
	challenge, err := navauth.Decode32(header)
	if count != 1 || err != nil {
		navAuthWrite(w, r, navAuthError(400, "invalid_request"))
		return
	}
	now, err := a.state.Now()
	if err != nil {
		navAuthWrite(w, r, navAuthError(503, "unavailable"))
		return
	}
	body, _ := json.Marshal(struct {
		Version int    `json:"nav_version"`
		Build   string `json:"build"`
		Time    int64  `json:"server_time_ms"`
	}{navauth.Protocol, a.build, now})
	mac, err := a.state.SignHello(challenge, 200, body)
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	navAuthWrite(w, r, navAuthReply{status: 200, body: body, hello: navauth.Encode32(mac)})
}
func (a *navAuthAdapter) Login(w http.ResponseWriter, r *http.Request) {
	if r == nil {
		navAuthWrite(w, r, navAuthError(400, "invalid_request"))
		return
	}
	if invocation, ok := r.Context().Value(navAuthLoginKey{}).(*navAuthLoginInvocation); ok && invocation != nil {
		invocation.interrupted = true
		return // the original invocation alone owns output and privileged work
	}
	invocation := &navAuthLoginInvocation{}
	*r = *r.WithContext(context.WithValue(r.Context(), navAuthLoginKey{}, invocation))
	b, ok := a.ownedBody(r)
	if invocation.interrupted {
		if b != nil {
			b.invalid, b.valid = true, false
		}
		navAuthWrite(w, r, navAuthError(400, "invalid_request"))
		return
	}
	if b != nil && b.owner == a && b.proofBusy {
		navAuthWrite(w, r, navAuthReply{status: 401})
		return
	}
	if !ok || b.path != "/api/nav/login" {
		navAuthWrite(w, r, navAuthError(400, "invalid_request"))
		return
	}
	if b.action == "exchange" {
		value, err := a.exchange(b.code)
		if err != nil {
			navAuthWrite(w, r, navAuthError(401, "invalid_link"))
			return
		}
		if _, err = navauth.Decode32(value); err != nil {
			navAuthWrite(w, r, navAuthError(500, "internal_error"))
			return
		}
		body, err := a.encode(navAuthResult{kind: "credential", value: value})
		if err != nil || len(body) > navauth.MaxResponse {
			navAuthWrite(w, r, navAuthError(500, "internal_error"))
			return
		}
		navAuthWrite(w, r, navAuthReply{status: 200, body: body})
		return
	}
	if !a.Proof(r) {
		navAuthWrite(w, r, navAuthReply{status: 401})
		return
	}
	v, ok := a.verified(r, "/api/nav/login")
	if !ok {
		navAuthWrite(w, r, navAuthReply{status: 401})
		return
	}
	out := a.signed(r, v, func() navAuthResult {
		code, err := a.issue(v.facts)
		if err == navauth.ErrCapacity {
			return navAuthResult{409, "error", "login_busy"}
		}
		if err != nil {
			return navAuthResult{500, "error", "internal_error"}
		}
		if _, err = navauth.Decode32(code); err != nil {
			return navAuthResult{500, "error", "internal_error"}
		}
		return navAuthResult{200, "code", code}
	})
	navAuthWrite(w, r, out)
}
func (a *navAuthAdapter) Info(w http.ResponseWriter, r *http.Request) {
	v, ok := a.verified(r, "/api/nav/info")
	if !ok {
		navAuthWrite(w, r, navAuthReply{status: 401})
		return
	}
	navAuthWrite(w, r, a.signed(r, v, func() navAuthResult { return navAuthResult{200, "info", ""} }))
}
func (a *navAuthAdapter) Revoke(w http.ResponseWriter, r *http.Request) {
	v, ok := a.verified(r, "/api/nav/revoke")
	if !ok {
		navAuthWrite(w, r, navAuthReply{status: 401})
		return
	}
	navAuthWrite(w, r, a.signed(r, v, func() navAuthResult {
		if a.revoke(v.facts) != nil {
			return navAuthResult{500, "error", "internal_error"}
		}
		return navAuthResult{200, "ok", ""}
	}))
}
