package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/orlenko/aiq/internal/daemon"
	"github.com/orlenko/aiq/internal/diagnostic"
	"github.com/orlenko/aiq/internal/state"
)

var errNavAsksAliasUnavailable = errors.New("asks aliases unavailable")
var errNavAsksStoreUnavailable = errors.New("asks store unavailable")
var errNavAsksStoreMissing = errors.New("asks store missing")

// Injected and unmounted. Authentication, the filesystem adapter, production
// limits and registered observer delivery belong to the guarded factory.
type navAsksAlias struct {
	path     string
	resolved bool
}
type navAsksResolve func(context.Context, string) (navAsksAlias, error)
type navAsksHTTPDeps struct {
	read       func(context.Context, time.Time, state.AsksSnapshotLimits) (*state.AsksSnapshot, error)
	resolve    navAsksResolve
	now        func() time.Time
	keyPresent func() bool
	render     navAsksWordingRender
	marshal    func(any) ([]byte, error)
}
type navAsksHTTPLimits struct {
	response navAsksResponseOptions // Only numerical allowances are retained.
	reader   state.AsksSnapshotLimits
	requests int
}
type navAsksHTTPCode uint8

const (
	askHTTPInvalid navAsksHTTPCode = iota
	askHTTPMissing
	askHTTPMethod
	askHTTPBusy
	askHTTPLarge
	askHTTPCancelled
	askHTTPInternal
	askHTTPUnshowable
	askHTTPUnavailable
)

var navAsksHTTPCodes = [...]struct {
	status int
	name   string
}{
	{400, "invalid_request"}, {404, "not_found"}, {405, "method_not_allowed"},
	{503, "asks_busy"}, {503, "asks_too_large"}, {503, "request_cancelled"},
	{500, "internal_error"}, {500, "unshowable"},
	{503, "asks_unavailable"},
}

type navAsksOutcome uint8

const (
	askOrdinary navAsksOutcome = iota
	askDecoderFinal
	askPanicFinal
)

type navAsksHTTPResponse struct {
	code   navAsksHTTPCode
	status int
	body   []byte
	kind   navAsksOutcome
}
type navAsksHandler struct {
	d      navAsksHTTPDeps
	limits navAsksHTTPLimits
	slots  chan struct{}
	errors [len(navAsksHTTPCodes)][]byte
}

func newNavAsksHandler(d navAsksHTTPDeps, l navAsksHTTPLimits) (*navAsksHandler, error) {
	o := l.response
	if d.read == nil || d.resolve == nil || d.now == nil || l.requests <= 0 || l.reader.Rows <= 0 || l.reader.TextBytes <= 0 ||
		o.width < 1 || o.rows == 0 || o.inputBytes == 0 || o.renderCells == 0 || o.arrayEntries == 0 || o.stringUnits == 0 || o.bodyBytes <= 0 || o.rows > math.MaxInt64 || o.inputBytes > math.MaxInt64 {
		return nil, errors.New("invalid asks HTTP configuration")
	}
	// No caller-owned scope, aliases or serial latch crosses requests.
	l.response = navAsksResponseOptions{width: o.width, rows: o.rows, inputBytes: o.inputBytes, renderCells: o.renderCells, arrayEntries: o.arrayEntries, stringUnits: o.stringUnits, bodyBytes: o.bodyBytes}
	if d.render == nil {
		d.render = webMarkdownLinesChecked
	}
	if d.marshal == nil {
		d.marshal = json.Marshal
	}
	h := &navAsksHandler{d: d, limits: l, slots: make(chan struct{}, l.requests)}
	for i, c := range navAsksHTTPCodes {
		b := []byte(`{"error":"` + c.name + `"}`)
		if int64(len(b)) > o.bodyBytes {
			return nil, errors.New("asks HTTP limit cannot hold errors")
		}
		h.errors[i] = b
	}
	return h, nil
}
func (h *navAsksHandler) failure(code navAsksHTTPCode, kind navAsksOutcome) navAsksHTTPResponse {
	return navAsksHTTPResponse{code: code, status: navAsksHTTPCodes[code].status, body: h.errors[code], kind: kind}
}
func navAsksHTTPDecode(req *http.Request) (navAsksScope, navAsksHTTPCode, bool) {
	bad := navAsksScope{}
	if req == nil || req.URL == nil {
		return bad, askHTTPInvalid, false
	}
	u := req.URL
	if u.Scheme != "" || u.Host != "" || u.User != nil || u.RawPath != "" || u.Fragment != "" || u.RawFragment != "" || u.Opaque != "" || strings.Contains(u.Path, "%") || !strings.HasPrefix(u.Path, "/") || path.Clean(u.Path) != u.Path || strings.HasSuffix(u.Path, "/") {
		return bad, askHTTPInvalid, false
	}
	if u.Path != "/api/nav/asks" {
		return bad, askHTTPMissing, false
	}
	target := u.Path
	if u.RawQuery != "" || u.ForceQuery {
		target += "?" + u.RawQuery
	}
	if req.RequestURI != "" && req.RequestURI != target {
		return bad, askHTTPInvalid, false
	}
	if req.Method != http.MethodGet {
		return bad, askHTTPMethod, false
	}
	if u.ForceQuery && u.RawQuery == "" || len(u.RawQuery) > 3*4096+64 || req.ContentLength > 0 || len(req.TransferEncoding) > 0 {
		return bad, askHTTPInvalid, false
	}
	if u.RawQuery != "" {
		for _, segment := range strings.Split(u.RawQuery, "&") {
			if segment == "" {
				return bad, askHTTPInvalid, false
			}
		}
	}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return bad, askHTTPInvalid, false
	}
	for key, value := range values {
		if key != "dir" || len(value) != 1 {
			return bad, askHTTPInvalid, false
		}
	}
	dir, exists := values["dir"]
	if !exists {
		return navAsksScope{"all", ""}, 0, true
	}
	d := dir[0]
	if d == "" || len(d) > 4096 || !utf8.ValidString(d) || strings.IndexByte(d, 0) >= 0 || !filepath.IsAbs(d) {
		return bad, askHTTPInvalid, false
	}
	return navAsksScope{"directory", d}, 0, true
}
func (r *navAsksRun) collectAliases(resolve navAsksResolve) error {
	if r.o.scope.Mode == "all" {
		return nil
	}
	for _, a := range r.raw.Items {
		if err := r.c.ctx.Err(); err != nil {
			return err
		}
		if a.Workspace == "" || asksInDir(a.Workspace, r.o.scope.Dir, func(p string) string { return p }) {
			continue
		}
		for _, key := range []string{r.o.scope.Dir, a.Workspace} {
			if _, ok := r.aliases[key]; ok {
				continue
			}
			if err := r.reserveAlias(key); err != nil {
				return err
			}
			value, err := resolve(r.c.ctx, key)
			r.faults.external(err)
			if r.faults.current {
				return diagnostic.Failure(diagnostic.Unshowable, r.faults.latch.Origin())
			}
			if stopped := r.c.ctx.Err(); stopped != nil {
				return stopped
			}
			if err != nil {
				if err == context.Canceled || err == context.DeadlineExceeded {
					return err
				}
				return errNavAsksAliasUnavailable
			}
			if !value.resolved {
				if value.path != "" {
					return diagnostic.Base(diagnostic.Unshowable)
				}
				value.path = key
			}
			if err := r.commitAlias(key, value.path); err != nil {
				return err
			}
		}
	}
	return nil
}
func (h *navAsksHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	h.serve(w, req, &navAsksFaultState{latch: &diagnostic.Latch{}})
}
func (h *navAsksHandler) serve(w http.ResponseWriter, req *http.Request, f *navAsksFaultState) {
	var release func()
	defer func() {
		if release != nil {
			release()
		}
	}()
	p := h.prepare(req, &release, f)
	headers := w.Header() // Transport stays outside builder recovery.
	if p.kind == askOrdinary && req != nil && req.Context().Err() != nil {
		p = h.failure(askHTTPCancelled, askOrdinary)
	}
	headers.Set("Content-Type", "application/json")
	headers.Set("Cache-Control", "no-store")
	for _, key := range []string{"ETag", "Retry-After", "Allow", "Content-Length"} {
		headers.Del(key)
	}
	if p.code == askHTTPMethod && p.status == 405 {
		headers.Set("Allow", http.MethodGet)
	}
	headers.Set("Content-Length", strconv.Itoa(len(p.body)))
	daemon.WriteNavigatorBody(w, req, headers, p.status, p.body)
}
func (h *navAsksHandler) prepare(req *http.Request, release *func(), f *navAsksFaultState) (out navAsksHTTPResponse) {
	defer func() {
		if value := recover(); value != nil {
			origin := diagnostic.Capture(value)
			f.current = true
			f.latch.Note(origin)
			out = h.failure(askHTTPInternal, askPanicFinal)
		}
		if req != nil {
			diagnostic.Note(req.Context(), f.latch.Origin())
		}
	}()
	scope, code, ok := navAsksHTTPDecode(req)
	if !ok {
		return h.failure(code, askDecoderFinal)
	}
	ctx := req.Context()
	if ctx.Err() != nil {
		return h.failure(askHTTPCancelled, askOrdinary)
	}
	select {
	case h.slots <- struct{}{}:
		*release = func() { <-h.slots }
	default:
		if ctx.Err() != nil {
			return h.failure(askHTTPCancelled, askOrdinary)
		}
		return h.failure(askHTTPBusy, askOrdinary)
	}
	if ctx.Err() != nil {
		return h.failure(askHTTPCancelled, askOrdinary)
	}
	o := h.limits.response
	o.scope = scope
	o.now = h.d.now()
	if ctx.Err() != nil {
		return h.failure(askHTTPCancelled, askOrdinary)
	}
	const seconds = navConversationSafeInteger / 1000
	if o.now.Before(time.Unix(-seconds, 0)) || !o.now.Before(time.Unix(seconds, 0).Add(time.Second)) {
		return h.failure(askHTTPUnshowable, askOrdinary)
	}
	s, err := h.d.read(ctx, o.now, h.limits.reader)
	if err == errNavAsksStoreMissing {
		o.notSetup = true
		s = &state.AsksSnapshot{UTCDay: state.AsksDay(o.now)}
		err = nil
	}
	f.external(err)
	if f.current {
		return h.failure(askHTTPInternal, askPanicFinal)
	}
	if ctx.Err() != nil {
		return h.failure(askHTTPCancelled, askOrdinary)
	}
	if err != nil {
		if err == errNavAsksStoreUnavailable {
			return h.failure(askHTTPUnavailable, askOrdinary)
		}
		if err == context.Canceled || err == context.DeadlineExceeded {
			return h.failure(askHTTPCancelled, askOrdinary)
		}
		if err == state.ErrAsksSnapshotTooLarge {
			return h.failure(askHTTPLarge, askOrdinary)
		}
		return h.failure(askHTTPInternal, askOrdinary)
	}
	if s == nil {
		return h.failure(askHTTPInternal, askOrdinary)
	}
	if !o.notSetup && h.d.keyPresent != nil {
		o.keyKnown = true
		o.keyOK = h.d.keyPresent()
	}
	r, err := newNavAsksRun(ctx, *s, o, f)
	if err == nil {
		err = r.collectAliases(h.d.resolve)
	}
	if err != nil {
		return h.runFailure(ctx, err, f)
	}
	body, err := r.finish(h.d.render, h.d.marshal)
	if err != nil {
		return h.runFailure(ctx, err, f)
	}
	if ctx.Err() != nil {
		return h.failure(askHTTPCancelled, askOrdinary)
	}
	return navAsksHTTPResponse{status: 200, body: body, kind: askOrdinary}
}
func (h *navAsksHandler) runFailure(ctx context.Context, err error, f *navAsksFaultState) navAsksHTTPResponse {
	if f.current {
		return h.failure(askHTTPInternal, askPanicFinal)
	}
	if ctx.Err() != nil {
		return h.failure(askHTTPCancelled, askOrdinary)
	}
	if err == context.Canceled || err == context.DeadlineExceeded {
		return h.failure(askHTTPCancelled, askOrdinary)
	}
	if err == errNavAsksAliasUnavailable {
		return h.failure(askHTTPInternal, askOrdinary)
	}
	if err == errNavAsksResponseTooLarge || err == errNavAsksWordingTooLarge {
		return h.failure(askHTTPLarge, askOrdinary)
	}
	return h.failure(askHTTPUnshowable, askOrdinary)
}
