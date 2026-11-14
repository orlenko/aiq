package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/orlenko/aiq/internal/daemon"
	"github.com/orlenko/aiq/internal/diagnostic"
	"github.com/orlenko/aiq/internal/navauth"
	"github.com/orlenko/aiq/internal/terminal"
	"github.com/orlenko/aiq/internal/transcript"
)

type navStateQuery struct{ provider, id, selection string }
type navStateMismatch struct {
	Build     string `json:"build"`
	Provider  string `json:"provider"`
	ID        string `json:"id"`
	Selection string `json:"selection"`
	Match     bool   `json:"match"`
	Reason    string `json:"reason"`
}
type navStateReady struct {
	Build         string      `json:"build"`
	Provider      string      `json:"provider"`
	ID            string      `json:"id"`
	Selection     string      `json:"selection"`
	Match         bool        `json:"match"`
	Cached        bool        `json:"cached"`
	CachedTurns   int         `json:"cached_turns"`
	CachedUpdated navWireTime `json:"cached_updated"`
	DiskChanged   bool        `json:"disk_changed"`
	Live          string      `json:"live"`
	WorkingKnown  bool        `json:"working_known"`
	Working       bool        `json:"working"`
	ElapsedMS     int64       `json:"elapsed_ms"`
}
type navStateReply struct {
	navListResponse
	query   navStateQuery
	record  navSelectedParse
	matched bool
}

func navStateDecode(r *http.Request) (navStateQuery, bool) {
	var q navStateQuery
	if r == nil || r.URL == nil || r.Method != "GET" {
		return q, false
	}
	u := r.URL
	if u.RawPath != "" || u.Fragment != "" || u.RawFragment != "" || u.Opaque != "" || u.ForceQuery || strings.Contains(u.Path, "%") {
		return q, false
	}
	rest, ok := strings.CutPrefix(u.Path, "/api/nav/sessions/")
	if !ok {
		return q, false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || parts[2] != "state" || !transcript.IsFullID(parts[0], parts[1]) {
		return q, false
	}
	selection, ok := strings.CutPrefix(u.RawQuery, "selection=")
	if !ok {
		return q, false
	}
	if _, err := navauth.Decode32(selection); err != nil {
		return q, false
	}
	if r.RequestURI != "" && r.RequestURI != u.RequestURI() {
		return q, false
	}
	return navStateQuery{parts[0], parts[1], selection}, true
}
func (f *navFactory) stateJSON(v any) navListResponse {
	size, ok := navJSONSize(v, f.policy.detail.bodyBytes)
	if !ok {
		return navListError(503, "state_too_large")
	}
	body, err := json.Marshal(v)
	if err != nil || int64(len(body)) != size {
		return navListError(500, "internal_error")
	}
	return navListResponse{status: 200, body: body}
}
func (f *navFactory) stateMismatch(q navStateQuery) navListResponse {
	return f.stateJSON(navStateMismatch{Build: daemon.NavigatorBuild(), Provider: q.provider, ID: q.id, Selection: q.selection, Reason: "selection_changed"})
}
func (f *navFactory) serveConversationState(bundle *navFactoryBundle, w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), f.policy.timeout)
	defer cancel()
	var release func()
	defer func() {
		if release != nil {
			release()
		}
	}()
	p := f.prepareConversationState(ctx, bundle, r, &release)
	h := w.Header() // Transport and header callouts stay outside builder recovery.
	if ctx.Err() != nil {
		p.navListResponse = navListError(503, "request_cancelled")
	} else if p.matched && !f.stateCurrent(bundle, p.record) {
		p.navListResponse = f.stateMismatch(p.query)
	}
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Del("ETag")
	h.Set("Content-Length", strconv.Itoa(len(p.body)))
	daemon.WriteNavigatorBody(w, r, h, p.status, p.body)
}
func (f *navFactory) prepareConversationState(ctx context.Context, bundle *navFactoryBundle, r *http.Request, release *func()) (p navStateReply) {
	defer func() {
		if v := recover(); v != nil {
			if v == http.ErrAbortHandler {
				panic(v)
			}
			diagnostic.Note(ctx, transcript.CapturePanic(v))
			p = navStateReply{navListResponse: navListError(500, "internal_error")}
		}
	}()
	q, ok := navStateDecode(r)
	if !ok {
		return navStateReply{navListResponse: navListError(400, "invalid_request")}
	}
	p.query = q
	select {
	case f.detailSlots <- struct{}{}:
		*release = func() { <-f.detailSlots }
	case <-ctx.Done():
		p.navListResponse = navListError(503, "request_cancelled")
		return p
	}
	if bundle == nil {
		p.navListResponse = f.stateMismatch(q)
		return p
	}
	record, ok := bundle.selections.lookup(q.provider, q.id, q.selection)
	if !ok || !bundle.selections.current(record) {
		p.navListResponse = f.stateMismatch(q)
		return p
	}
	s, snap, ok := f.cache.CachedSession(record.path)
	if !ok || snap != record.snapshot {
		p.navListResponse = f.stateMismatch(q)
		return p
	}
	current, err := transcript.Stat(record.path)
	if err != nil {
		p.navListResponse = f.stateMismatch(q)
		return p
	}
	if current.Dev != snap.Dev || current.Ino != snap.Ino {
		p.navListResponse = f.stateMismatch(q)
		return p
	}
	aux, err := f.liveFacts(ctx, s, false)
	if err != nil {
		code := "state_unavailable"
		if errors.Is(err, errNavAuxTooLarge) {
			code = "state_too_large"
		}
		p.navListResponse = navListError(503, code)
		return p
	}
	if aux.context != record.context {
		p.navListResponse = f.stateMismatch(q)
		return p
	}
	updated, err := navConversationTime(s.Updated)
	if err != nil {
		p.navListResponse = navListError(503, "state_unavailable")
		return p
	}
	live := "unknown"
	switch aux.observation {
	case terminal.Running:
		live = "live"
	case terminal.Ended:
		live = "ended"
	}
	value := navStateReady{Build: daemon.NavigatorBuild(), Provider: q.provider, ID: q.id, Selection: q.selection, Match: true, Cached: true, CachedTurns: len(s.Turns), CachedUpdated: updated, DiskChanged: current.Size != snap.Size || current.Mtime != snap.Mtime, Live: live, WorkingKnown: aux.workingKnown, Working: aux.working, ElapsedMS: aux.elapsedMS}
	p.navListResponse = f.stateJSON(value)
	p.record = record
	p.matched = p.status == 200
	return p
}

func (f *navFactory) stateCurrent(bundle *navFactoryBundle, record navSelectedParse) bool {
	f.mu.Lock()
	current := f.bundle == bundle
	f.mu.Unlock()
	return current && bundle != nil && bundle.selections.current(record)
}
