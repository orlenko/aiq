package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/orlenko/aiq/internal/diagnostic"
	"github.com/orlenko/aiq/internal/transcript"
)

// This handler is not mounted. Production construction and numerical limits
// require the separate resource-policy decision; no policy defaults live here.
type navSessionsLimits struct {
	rowBytes, bodyBytes int64
	builders            int
}

type navSessionsDeps struct {
	roots       transcript.Roots
	source      transcript.Discovery
	summary     func(context.Context, string) (transcript.Summary, error)
	listContext func(context.Context) (context.Context, context.CancelFunc)
}

type navSessionsHandler struct {
	deps   navSessionsDeps
	limits navSessionsLimits
	roots  []string // configured coverage, not a visited-root trace
	slots  chan struct{}
}

func newNavSessionsHandler(deps navSessionsDeps, limits navSessionsLimits) (*navSessionsHandler, error) {
	if deps.source == nil || deps.summary == nil || limits.rowBytes <= 0 || limits.bodyBytes <= 0 || limits.builders <= 0 {
		return nil, errors.New("invalid navigator list configuration")
	}
	for _, code := range []string{"invalid_request", "not_found", "method_not_allowed", "internal_error", "list_too_large", "request_cancelled", "list_busy"} {
		if int64(len(navListError(500, code).body)) > limits.bodyBytes {
			return nil, errors.New("navigator response limit cannot hold errors")
		}
	}
	// A supplied Discovery is trusted to match this scope and keep its maps
	// stable. A later production factory must clone BEFORE constructing Index.
	deps.roots = cloneNavRoots(deps.roots)
	if deps.listContext == nil {
		deps.listContext = func(ctx context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(ctx, transcript.ListBudget)
		}
	}
	roots := make([]string, 0)
	roots = append(roots, deps.roots.ClaudeProjects...)
	roots = append(roots, deps.roots.CodexSessions...)
	roots = append(roots, deps.roots.AgyAppData...)
	return &navSessionsHandler{deps: deps, limits: limits, roots: roots, slots: make(chan struct{}, limits.builders)}, nil
}

func cloneNavRoots(r transcript.Roots) transcript.Roots {
	clone := func(in []string) []string {
		out := make([]string, len(in))
		for i, s := range in {
			out[i] = strings.Clone(s)
		}
		return out
	}
	r.ClaudeProjects, r.CodexSessions = clone(r.ClaudeProjects), clone(r.CodexSessions)
	r.CodexIndex, r.AgyAppData = clone(r.CodexIndex), clone(r.AgyAppData)
	r.CopilotAppData = clone(r.CopilotAppData)
	labels := make(map[string]string, len(r.Labels))
	for k, v := range r.Labels {
		labels[strings.Clone(k)] = strings.Clone(v)
	}
	r.Labels = labels
	return r
}

type navListResponse struct {
	status int
	body   []byte
	allow  string
}

func navListError(status int, code string) navListResponse {
	return navListResponse{status: status, body: []byte(`{"error":"` + code + `"}`)}
}

func (h *navSessionsHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	var release func()
	defer func() {
		if release != nil {
			release()
		}
	}()
	p := h.prepare(req, &release)
	if req != nil && req.Context().Err() != nil {
		p = navListError(http.StatusServiceUnavailable, "request_cancelled")
	}
	// The writer is deliberately outside builder recovery. No second response
	// is attempted if transport writes fail or panic; admission lasts through it.
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if p.allow != "" {
		w.Header().Set("Allow", p.allow)
	}
	w.WriteHeader(p.status)
	_, _ = w.Write(p.body)
}

type navListQuery struct {
	dir     string
	workers bool
}

func navListDecode(req *http.Request) (navListQuery, navListResponse, bool) {
	bad := navListError(http.StatusBadRequest, "invalid_request")
	if req == nil || req.URL == nil {
		return navListQuery{}, bad, false
	}
	u := req.URL
	if u.RawPath != "" || u.Fragment != "" || u.Opaque != "" || strings.Contains(u.Path, "%") || u.ForceQuery && u.RawQuery == "" {
		return navListQuery{}, bad, false
	}
	if u.Path != "/api/nav/sessions" {
		return navListQuery{}, navListError(http.StatusNotFound, "not_found"), false
	}
	// A direct caller can supply an inconsistent URL/RequestURI pair. Require
	// the literal origin-form path here too; query decoding remains below.
	if req.RequestURI != "" {
		target, _, _ := strings.Cut(req.RequestURI, "?")
		if target != u.Path {
			return navListQuery{}, bad, false
		}
	}
	if req.Method != http.MethodGet {
		p := navListError(http.StatusMethodNotAllowed, "method_not_allowed")
		p.allow = http.MethodGet
		return navListQuery{}, p, false
	}
	if len(u.RawQuery) > 3*4096+64 {
		return navListQuery{}, bad, false
	}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return navListQuery{}, bad, false
	}
	for k, v := range values {
		if (k != "dir" && k != "workers") || len(v) != 1 {
			return navListQuery{}, bad, false
		}
	}
	dir := values.Get("dir")
	if len(dir) == 0 || len(dir) > 4096 || !utf8.ValidString(dir) || strings.IndexByte(dir, 0) >= 0 || !filepath.IsAbs(dir) {
		return navListQuery{}, bad, false
	}
	workers := false
	if v, exists := values["workers"]; exists {
		if v[0] != "0" && v[0] != "1" {
			return navListQuery{}, bad, false
		}
		workers = v[0] == "1"
	}
	return navListQuery{dir: filepath.Clean(dir), workers: workers}, navListResponse{}, true
}

func (h *navSessionsHandler) prepare(req *http.Request, release *func()) (out navListResponse) {
	defer func() {
		if value := recover(); value != nil {
			origin := diagnostic.Capture(value)
			if req != nil {
				diagnostic.Note(req.Context(), origin)
			}
			out = navListError(http.StatusInternalServerError, "internal_error")
		}
	}()
	q, p, ok := navListDecode(req)
	if !ok {
		return p
	}
	parent := req.Context()
	if parent.Err() != nil {
		return navListError(http.StatusServiceUnavailable, "request_cancelled")
	}
	child, cancel := h.deps.listContext(parent)
	defer cancel()
	select {
	case h.slots <- struct{}{}:
		*release = func() { <-h.slots }
	case <-child.Done():
		if parent.Err() != nil {
			return navListError(http.StatusServiceUnavailable, "request_cancelled")
		}
		return navListError(http.StatusServiceUnavailable, "list_busy")
	}
	if parent.Err() != nil {
		return navListError(http.StatusServiceUnavailable, "request_cancelled")
	}
	if child.Err() != nil {
		return navListError(http.StatusServiceUnavailable, "list_busy")
	}
	run := &navListRun{Discovery: h.deps.source, parse: h.deps.summary, cancel: cancel, limit: h.limits.rowBytes, needed: make(map[string]struct{})}
	if !run.reserve(512, q.dir) {
		return navListError(http.StatusServiceUnavailable, "list_too_large")
	}
	for _, root := range h.roots {
		if !run.reserve(16, root) {
			return navListError(http.StatusServiceUnavailable, "list_too_large")
		}
	}
	result, err := transcript.ListWith(child, run, q.dir, run.summary)
	diagnostic.Note(child, transcript.PanicOrigin(err))
	// Dependency seams latched actual interruption, including empty discovery
	// and names. Later child expiry must not rewrite completed collection.
	flags := run.flags()
	if parent.Err() != nil {
		return navListError(http.StatusServiceUnavailable, "request_cancelled")
	}
	if flags.internal {
		return navListError(http.StatusInternalServerError, "internal_error")
	}
	if flags.resource {
		return navListError(http.StatusServiceUnavailable, "list_too_large")
	}
	if flags.discovery || err != nil {
		return navListError(http.StatusInternalServerError, "internal_error")
	}
	eligible := make([]transcript.Summary, 0, len(result.Rows))
	for _, row := range result.Rows {
		if !row.Empty && (q.workers || !row.Worker) {
			eligible = append(eligible, row)
		}
	}
	sort.Slice(eligible, func(i, j int) bool {
		a, b := eligible[i], eligible[j]
		if !a.Updated.Equal(b.Updated) {
			return a.Updated.After(b.Updated)
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return a.Provider < b.Provider
	})
	partial := result.Partial || flags.partial || len(eligible) > transcript.ListRows
	selected := make([]transcript.Summary, min(len(eligible), transcript.ListRows))
	copy(selected, eligible)
	clear(result.Rows)
	clear(eligible)
	result.Rows, eligible = nil, nil
	body := navSessionsBody{Dir: q.dir, RootsSearched: append([]string{}, h.roots...), Partial: partial,
		Scanned: result.Scanned, Of: result.Of, UnreadableCount: len(result.Unreadable), Sessions: make([]navSessionRow, 0, len(selected))}
	for _, row := range selected {
		body.Sessions = append(body.Sessions, navSessionDTO(row))
	}
	clear(selected)
	size, fits := navJSONSize(body, h.limits.bodyBytes)
	if !fits {
		return navListError(http.StatusServiceUnavailable, "list_too_large")
	}
	encoded, err := json.Marshal(body)
	if err != nil || int64(len(encoded)) != size || int64(len(encoded)) > h.limits.bodyBytes {
		return navListError(http.StatusInternalServerError, "internal_error")
	}
	if parent.Err() != nil {
		return navListError(http.StatusServiceUnavailable, "request_cancelled")
	}
	return navListResponse{status: http.StatusOK, body: encoded}
}

type navSessionsBody struct {
	Dir             string          `json:"dir"`
	RootsSearched   []string        `json:"roots_searched"`
	Partial         bool            `json:"partial"`
	Scanned         int             `json:"scanned"`
	Of              int             `json:"of"`
	UnreadableCount int             `json:"unreadable_count"`
	Sessions        []navSessionRow `json:"sessions"`
}

type navSessionRow struct {
	Provider string           `json:"provider"`
	ID       string           `json:"id"`
	Cwd      string           `json:"cwd"`
	Label    string           `json:"label"`
	Title    string           `json:"title"`
	Model    string           `json:"model"`
	Branch   string           `json:"branch"`
	Started  string           `json:"started"`
	Updated  string           `json:"updated"`
	Turns    int              `json:"turns"`
	Tools    int              `json:"tools"`
	Worker   bool             `json:"worker"`
	Bypass   bool             `json:"bypass"`
	Commands []navListCommand `json:"commands"`
}

type navListCommand struct {
	Verb      string `json:"verb"`
	FreshOnly bool   `json:"fresh_only"`
}

func navSessionDTO(s transcript.Summary) navSessionRow {
	commands := make([]navListCommand, 0)
	if transcript.IsFullID(s.Provider, s.ID) {
		commands = append(commands, navListCommand{Verb: "convo"})
		if s.Provider != "agy" {
			commands = append(commands, navListCommand{Verb: "convo_follow"})
		}
	}
	return navSessionRow{Provider: s.Provider, ID: s.ID, Cwd: s.Cwd, Label: s.Label, Title: s.Title, Model: s.Model, Branch: s.Branch,
		Started: s.Started.UTC().Format(time.RFC3339Nano), Updated: s.Updated.UTC().Format(time.RFC3339Nano),
		Turns: s.Turns, Tools: s.Tools, Worker: s.Worker, Bypass: s.Bypass, Commands: commands}
}
