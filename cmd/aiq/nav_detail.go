package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/transcript"
)

// Unmounted: authentication, original-site diagnostics, production facts/policy,
// delta/state and the final guarded factory remain separate prerequisites.
type navDetailHandler struct {
	deps   navDetailDeps
	limits navDetailLimits
	slots  chan struct{}
	errors [navDetailCodeCount][6][]byte
}

func newNavDetailHandler(d navDetailDeps, limits navDetailLimits) (*navDetailHandler, error) {
	if d.cache == nil || d.resolve == nil || d.facts == nil || d.now == nil || d.location == nil ||
		d.skippedNote == "" || limits.bodyBytes <= 0 || limits.requests <= 0 || len(d.build) == 0 || len(d.build) > 128 {
		return nil, errNavDetailConfiguration
	}
	for _, c := range d.build {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._+-", c)) {
			return nil, errNavDetailConfiguration
		}
	}
	d.build, d.skippedNote, d.liveMark = strings.Clone(d.build), strings.Clone(d.skippedNote), strings.Clone(d.liveMark)
	if d.requestContext == nil {
		d.requestContext = func(parent context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(parent, 5*time.Second)
		}
	}
	if d.load == nil {
		d.load = d.cache.Session
	}
	if d.project == nil {
		d.project = projectNavConversation
	}
	if d.marshal == nil {
		d.marshal = json.Marshal
	}
	h := &navDetailHandler{deps: d, limits: limits, slots: make(chan struct{}, limits.requests)}
	for code := navDetailInvalid; code < navDetailCodeCount; code++ {
		for waited := 0; waited <= 5; waited++ {
			e := navDetailError{Build: d.build, Error: navDetailCodes[code].name, WaitedS: waited}
			size, ok := navJSONSize(e, limits.bodyBytes)
			if !ok {
				return nil, errNavDetailConfiguration
			}
			body, err := json.Marshal(e)
			if err != nil || int64(len(body)) != size {
				return nil, errNavDetailConfiguration
			}
			h.errors[code][waited] = body
		}
	}
	return h, nil
}

func (h *navDetailHandler) failure(code navDetailCode, started time.Time) navDetailResponse {
	waited := 0
	if code == navDetailPreparing {
		waited = max(0, min(5, int(time.Since(started)/time.Second)))
	}
	return navDetailResponse{status: navDetailCodes[code].status, code: code, body: h.errors[code][waited]}
}

func (h *navDetailHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	started := time.Now()
	var release, cancel func()
	var child context.Context
	defer func() {
		if cancel != nil {
			cancel()
		}
		if release != nil {
			release()
		}
	}()
	p := h.prepare(req, started, &child, &cancel, &release)
	// Header acquisition is outside builder recovery, like the actual writes.
	// The child stays alive through this final precommit check and transport.
	headers := w.Header()
	if req != nil && req.Context().Err() != nil {
		p = h.failure(navDetailCancelled, started)
	} else if child != nil && child.Err() != nil {
		p = h.failure(navDetailPreparing, started)
	}
	headers.Set("Content-Type", "application/json")
	headers.Set("Cache-Control", "no-store")
	headers.Del("ETag")
	headers.Del("Retry-After")
	headers.Del("Allow")
	headers.Del("Content-Length")
	if p.etag != "" {
		headers.Set("ETag", "\""+p.etag+"\"")
	}
	if p.code == navDetailPreparing {
		headers.Set("Retry-After", "2")
	}
	if p.code == navDetailMethod {
		headers.Set("Allow", http.MethodGet)
	}
	if p.status != http.StatusNotModified {
		headers.Set("Content-Length", strconv.Itoa(len(p.body)))
	}
	w.WriteHeader(p.status)
	if p.status != http.StatusNotModified {
		_, _ = w.Write(p.body)
	}
}

func (h *navDetailHandler) prepare(req *http.Request, started time.Time, child *context.Context, cancel, release *func()) (p navDetailResponse) {
	defer func() {
		if recover() != nil {
			p = h.failure(navDetailInternal, started)
		}
	}()
	q, code, ok := navDetailDecode(req)
	if !ok {
		return h.failure(code, started)
	}
	parent := req.Context()
	ctx, stop := h.deps.requestContext(parent)
	*cancel = stop
	*child = ctx
	if ctx == nil || stop == nil {
		return h.failure(navDetailInternal, started)
	}
	check := func() (navDetailResponse, bool) {
		if parent.Err() != nil {
			return h.failure(navDetailCancelled, started), true
		}
		if ctx.Err() != nil {
			return h.failure(navDetailPreparing, started), true
		}
		return navDetailResponse{}, false
	}
	if failure, stopped := check(); stopped {
		return failure
	}
	select {
	case h.slots <- struct{}{}:
		*release = func() { <-h.slots }
	case <-ctx.Done():
		if failure, stopped := check(); stopped {
			return failure
		}
		return h.failure(navDetailPreparing, started)
	case <-parent.Done():
		return h.failure(navDetailCancelled, started)
	}
	if failure, stopped := check(); stopped {
		return failure
	}
	hit, err := h.deps.resolve(ctx, q.provider, q.id)
	if failure, stopped := check(); stopped {
		return failure
	}
	if err != nil {
		return h.failure(navDetailErrorCode(err), started)
	}
	s, snap, err := h.deps.load(ctx, hit.Path)
	if failure, stopped := check(); stopped {
		return failure
	}
	if err != nil {
		return h.failure(navDetailErrorCode(err), started)
	}
	if s == nil {
		return h.failure(navDetailInternal, started)
	}
	if s.Provider != q.provider || s.ID != q.id {
		return h.failure(navDetailSelectionChanged, started)
	}
	_, _, err = navConversationBounds(s.Turns, q.last, q.anchor)
	if failure, stopped := check(); stopped {
		return failure
	}
	if err != nil {
		return h.failure(navDetailErrorCode(err), started)
	}
	facts, err := h.deps.facts(ctx, hit, s)
	if failure, stopped := check(); stopped {
		return failure
	}
	if err != nil {
		return h.failure(navDetailErrorCode(err), started)
	}
	facts, err = navDetailOwnFacts(facts, len(s.Turns))
	if failure, stopped := check(); stopped {
		return failure
	}
	if err != nil {
		return h.failure(navDetailInternal, started)
	}
	o := navConversationOptions{provider: q.provider, id: q.id, last: q.last, anchor: q.anchor, markup: q.markup,
		now: h.deps.now(), location: h.deps.location, live: facts.live, hold: facts.hold, stale: facts.stale,
		accounts: facts.accounts, skippedNote: h.deps.skippedNote, liveMark: h.deps.liveMark, bodyBytes: h.limits.bodyBytes}
	if failure, stopped := check(); stopped {
		return failure
	}
	key, err := navDetailContextKey(parent, ctx, s, snap, q, facts, o, h.deps.build)
	if failure, stopped := check(); stopped {
		return failure
	}
	if err != nil {
		return h.failure(navDetailErrorCode(err), started)
	}
	view, err := h.deps.cache.ViewContext(ctx, key, func() (*transcript.View, error) {
		return h.buildView(s, snap, facts, o)
	})
	if failure, stopped := check(); stopped {
		return failure
	}
	if err != nil {
		return h.failure(navDetailErrorCode(err), started)
	}
	value, ok := navDetailViewValue(view, h.limits.bodyBytes)
	if !ok {
		return h.failure(navDetailInternal, started)
	}
	if q.condition.match(value.fingerprint) {
		return navDetailResponse{status: http.StatusNotModified, etag: value.fingerprint}
	}
	return navDetailResponse{status: http.StatusOK, body: value.body, etag: value.fingerprint}
}

func navDetailErrorCode(err error) navDetailCode {
	switch {
	case errors.Is(err, transcript.ErrBadID):
		return navDetailInvalid
	case errors.Is(err, transcript.ErrNoSuchSession), errors.Is(err, os.ErrNotExist):
		return navDetailSessionMissing
	case errors.Is(err, errNavConversationAnchor):
		return navDetailAnchorMissing
	case errors.Is(err, transcript.ErrTooLarge):
		return navDetailTranscriptLarge
	case errors.Is(err, errNavConversationTooLarge):
		return navDetailViewLarge
	case errors.Is(err, errNavDetailChanged):
		return navDetailSelectionChanged
	case errors.Is(err, transcript.ErrPreparing), errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return navDetailPreparing
	case errors.Is(err, transcript.ErrUnshowable):
		return navDetailUnshowable
	default:
		return navDetailInternal
	}
}

func (h *navDetailHandler) buildView(s *transcript.Session, snap transcript.Snapshot, facts navDetailFacts, o navConversationOptions) (*transcript.View, error) {
	// This check runs AFTER view admission. Snapshot equality alone cannot
	// prove that a closure captured the current parse incarnation.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	current, currentSnap, err := h.deps.cache.Session(ctx, snap.Path)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	if current != s || currentSnap != snap {
		return nil, errNavDetailChanged
	}
	window, err := h.deps.project(s, o)
	if err != nil {
		return nil, err
	}
	if window == nil {
		return nil, transcript.ErrUnshowable
	}
	e := navDetailEnvelope{Build: h.deps.build, Session: window.Session, EarlierHidden: window.EarlierHidden,
		LaterHidden: window.LaterHidden, WindowStart: window.WindowStart, Reset: window.Reset, Markup: window.Markup, Messages: window.Messages}
	size, fits := navJSONSize(e, math.MaxInt64)
	if !fits {
		return nil, transcript.ErrUnshowable
	}
	if size > h.limits.bodyBytes {
		return nil, errNavConversationTooLarge
	}
	body, err := h.deps.marshal(e)
	if err != nil || int64(len(body)) != size {
		return nil, transcript.ErrUnshowable
	}
	digest := sha256.Sum256(body)
	value := &navDetailView{envelope: e, body: body, fingerprint: hex.EncodeToString(digest[:]), encodedLength: size}
	retained, ok := navDetailRetainedBytes(value)
	if !ok {
		return nil, errNavDetailConfiguration
	}
	return &transcript.View{Value: value, ETag: value.fingerprint, Bytes: retained,
		Settled: facts.leaseKnown && transcript.Settled(s, facts.hold)}, nil
}

func navDetailViewValue(view *transcript.View, limit int64) (*navDetailView, bool) {
	if view == nil {
		return nil, false
	}
	v, ok := view.Value.(*navDetailView)
	if !ok || v == nil || len(v.body) == 0 || len(v.fingerprint) != 64 || v.fingerprint != view.ETag ||
		v.encodedLength != int64(len(v.body)) || int64(len(v.body)) > limit {
		return nil, false
	}
	for _, c := range v.fingerprint {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return nil, false
		}
	}
	return v, true
}
