package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/orlenko/aiq/internal/daemon"
	"github.com/orlenko/aiq/internal/diagnostic"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/terminal"
	"github.com/orlenko/aiq/internal/transcript"
)

type navLongQuery struct {
	dir     string
	history bool
}

func navLongDecode(r *http.Request) (navLongQuery, bool) {
	var q navLongQuery
	if r == nil || r.URL == nil || r.Method != "GET" {
		return q, false
	}
	u := r.URL
	if u.Path != "/api/nav/long" || u.RawPath != "" || u.Fragment != "" || u.RawFragment != "" || u.Opaque != "" || u.ForceQuery || len(u.RawQuery) > 3*4096+64 || r.RequestURI != "" && r.RequestURI != u.RequestURI() {
		return q, false
	}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil || values.Encode() != u.RawQuery {
		return q, false
	}
	for key, rows := range values {
		if len(rows) != 1 || key != "dir" && key != "history" {
			return q, false
		}
	}
	q.history = true
	if value, ok := values["dir"]; ok {
		if !navCatalogPath(value[0]) {
			return q, false
		}
		q.dir = value[0]
	}
	if value, ok := values["history"]; ok {
		if q.dir == "" || value[0] != "0" && value[0] != "1" {
			return q, false
		}
		q.history = value[0] == "1"
	}
	return q, true
}

func (f *navFactory) longMetadata(w http.ResponseWriter, r *http.Request) {
	var release func()
	defer func() {
		if release != nil {
			release()
		}
	}()
	result := f.prepareLongMetadata(r, &release)
	h := w.Header()
	if r.Context().Err() != nil {
		result = navListError(503, "request_cancelled")
	}
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	daemon.WriteNavigatorBody(w, r, h, result.status, result.body)
}
func (f *navFactory) prepareLongMetadata(r *http.Request, release *func()) (out navListResponse) {
	defer func() {
		if v := recover(); v != nil {
			if v == http.ErrAbortHandler {
				panic(v)
			}
			diagnostic.Note(r.Context(), transcript.CapturePanic(v))
			out = navListError(500, "internal_error")
		}
	}()
	q, ok := navLongDecode(r)
	if !ok {
		return navListError(400, "invalid_request")
	}
	ctx, cancel := context.WithTimeout(r.Context(), f.policy.timeout)
	defer cancel()
	select {
	case f.sessionSlots <- struct{}{}:
		*release = func() { <-f.sessionSlots }
	case <-ctx.Done():
		return navListError(503, "list_busy")
	}
	if f.input.metadata == nil {
		return navListError(503, "long_unavailable")
	}
	snapshot, err := f.input.metadata(ctx, state.NavigatorSnapshotLimits{Rows: f.policy.sessions.arrayEntries, TextBytes: f.policy.sessions.rowBytes})
	if errors.Is(err, state.ErrNavigatorSnapshotTooLarge) {
		return navListError(503, "list_too_large")
	}
	if err != nil || snapshot == nil || ctx.Err() != nil {
		return navListError(503, "long_unavailable")
	}
	run := &navListRun{parse: f.cache.Summary, cancel: cancel, limit: f.policy.sessions.rowBytes, needed: map[string]struct{}{}}
	if !run.reserve(512, q.dir) {
		return navListError(503, "list_too_large")
	}
	for _, v := range snapshot.Launches {
		if !run.reserve(256, v.Provider, v.AccountID, v.Launcher, v.Cwd, v.Mode, v.SessionID) {
			return navListError(503, "list_too_large")
		}
	}
	for _, v := range snapshot.Leases {
		if !run.reserve(256, v.Provider, v.AccountID, v.Launcher, v.Cwd, v.Mode, v.Workspace, v.Pane, v.SessionID, v.Drain) {
			return navListError(503, "list_too_large")
		}
	}
	body, err := f.buildLongMetadata(ctx, q, snapshot, run)
	if r.Context().Err() != nil {
		return navListError(503, "request_cancelled")
	}
	if errors.Is(err, state.ErrNavigatorSnapshotTooLarge) || run.flags().resource {
		return navListError(503, "list_too_large")
	}
	if err != nil || run.flags().internal {
		return navListError(503, "long_unavailable")
	}
	size, ok := navJSONSize(body, f.policy.sessions.bodyBytes)
	if !ok {
		return navListError(503, "list_too_large")
	}
	encoded, err := json.Marshal(body)
	if err != nil || int64(len(encoded)) != size {
		return navListError(500, "internal_error")
	}
	return navListResponse{status: 200, body: encoded}
}

type navWorkspaceResult struct {
	root  string
	known bool
}
type navWorkspaceResolver struct {
	ctx   context.Context
	probe func(context.Context, string) (string, bool)
	memo  map[string]navWorkspaceResult
}

func (r *navWorkspaceResolver) resolve(dir string) navWorkspaceResult {
	if value, ok := r.memo[dir]; ok {
		return value
	}
	value := navWorkspaceResult{}
	if navCatalogPath(dir) && r.ctx.Err() == nil && r.probe != nil {
		root, known := r.probe(r.ctx, dir)
		if known && navCatalogPath(root) {
			value = navWorkspaceResult{root, true}
		}
	}
	r.memo[dir] = value
	return value
}

type navLongTranscript struct {
	available bool
	workspace navWorkspaceResult
}

func (f *navFactory) buildLongMetadata(ctx context.Context, q navLongQuery, snapshot *state.NavigatorSnapshot, run *navListRun) (navLongBody, error) {
	body := navLongBody{Build: daemon.NavigatorBuild(), Kind: "list", RequestedDir: q.dir, State: "records", HistoryPresent: q.history,
		CurrentSpans: []navLongSpan{}, Earlier: []navLongSession{}, Workspaces: []navLongWorkspace{}}
	observed, err := navConversationTime(f.input.now())
	if err != nil {
		return body, err
	}
	body.Observed = observed
	resolver := &navWorkspaceResolver{ctx: ctx, probe: f.input.workspace, memo: map[string]navWorkspaceResult{}}
	workspaces := map[string]*navLongWorkspace{}
	addWorkspace := func(path string) *navLongWorkspace {
		if value := workspaces[path]; value != nil {
			return value
		}
		value := &navLongWorkspace{Workspace: strings.Clone(path)}
		workspaces[value.Workspace] = value
		return value
	}
	for _, lease := range snapshot.Leases {
		if lease.Mode == state.ModeLong && lease.Depth == 0 && navCatalogPath(lease.Workspace) {
			addWorkspace(lease.Workspace).Recorded++
		}
	}
	if q.dir != "" {
		body.Kind = "workspace"
		body.Workspace = q.dir
		if workspaces[q.dir] != nil {
			body.WorkspaceKnown = true
		} else if resolved := resolver.resolve(q.dir); resolved.known {
			body.Workspace = resolved.root
			body.WorkspaceKnown = true
		} else {
			body.Partial = true
		}
	}
	var selected *state.NavigatorLease
	ambiguous := false
	if body.Kind == "workspace" {
		for i := range snapshot.Leases {
			lease := &snapshot.Leases[i]
			if lease.Mode == state.ModeLong && lease.Depth == 0 && lease.Workspace == body.Workspace {
				if selected != nil {
					ambiguous = true
					break
				}
				selected = lease
			}
		}
	}
	transcripts := map[string]navLongTranscript{}
	var bundle *navFactoryBundle
	lookup := func(provider, id string) navLongTranscript {
		key := provider + "/" + id
		if value, ok := transcripts[key]; ok {
			return value
		}
		value := navLongTranscript{}
		if !transcript.IsFullID(provider, id) || ctx.Err() != nil {
			return value
		}
		if bundle == nil {
			bundle, _ = f.get(ctx)
		}
		if bundle != nil {
			hit, e := bundle.discovery.Find(ctx, provider, id, run.summary)
			diagnostic.Note(ctx, transcript.PanicOrigin(e))
			if e == nil {
				s, e := run.summary(ctx, hit.Path)
				if e == nil && s.Provider == provider && s.ID == id {
					value.available = true
					if s.Cwd != "" {
						if !q.history && body.WorkspaceKnown && s.Cwd == body.Workspace {
							value.workspace = navWorkspaceResult{body.Workspace, true}
						} else {
							value.workspace = resolver.resolve(s.Cwd)
						}
					}
				}
			}
		}
		transcripts[key] = value
		return value
	}
	recordedKey := ""
	if selected != nil && !ambiguous {
		status, e := f.observeNavLease(ctx, snapshot, selected, navLeaseLaunchID(ctx, snapshot, selected, true))
		if e != nil && ctx.Err() == nil {
			return body, e
		}
		live := "unknown"
		if status == terminal.Running {
			live = "live"
		} else if status == terminal.Ended {
			live = "ended"
		}
		drain := selected.Drain
		switch drain {
		case state.DrainNone, state.DrainRequested, state.DrainDraining, state.DrainReady, state.DrainWaiting:
		default:
			drain = "unknown"
		}
		lease := &navLongLease{ID: strconv.FormatInt(selected.ID, 10), Started: strconv.FormatInt(selected.StartedAt, 10), Provider: strings.Clone(selected.Provider),
			Account: strings.Clone(selected.AccountID), Launcher: strings.Clone(selected.Launcher), Pane: strings.Clone(selected.Pane), Live: live, Drain: drain, TurnStarted: navLongSeconds(selected.TurnStartedAt)}
		if status == terminal.Running && selected.Provider != "agy" && lease.TurnStarted.Known && selected.TurnEndedAt >= 0 && selected.TurnEndedAt <= navConversationSafeInteger/1000 && observed.MS >= lease.TurnStarted.MS {
			lease.WorkingKnown = true
			lease.Working = selected.TurnStartedAt > selected.TurnEndedAt
		}
		body.Lease = lease
		if transcript.IsFullID(selected.Provider, selected.SessionID) {
			if status != terminal.Ended {
				recordedKey = selected.Provider + "/" + selected.SessionID
			}
			info := lookup(selected.Provider, selected.SessionID)
			if status == terminal.Running && info.available && (!info.workspace.known || info.workspace.root == body.Workspace) {
				body.Current = &navLongScope{Provider: strings.Clone(selected.Provider), ID: strings.Clone(selected.SessionID)}
			}
		}
	}
	records := []navLongLaunch{}
	rootGroups := map[string]map[string]bool{}
	if q.history {
		for index, row := range snapshot.Launches {
			if row.Mode != state.ModeLong {
				continue
			}
			if ctx.Err() != nil {
				body.Partial = true
				for _, remaining := range snapshot.Launches[index:] {
					if remaining.Mode == state.ModeLong {
						body.UnmappedLaunches++
					}
				}
				break
			}
			scope := resolver.resolve(row.Cwd)
			if !scope.known {
				body.Partial = true
				body.UnmappedLaunches++
			}
			matches := body.Kind == "workspace" && (scope.known && scope.root == body.Workspace || !scope.known && row.Cwd == body.Workspace)
			if body.Kind == "list" {
				if !scope.known {
					continue
				}
				info := lookup(row.Provider, row.SessionID)
				if info.workspace.known && info.workspace.root != scope.root {
					continue
				}
				addWorkspace(scope.root)
				if rootGroups[scope.root] == nil {
					rootGroups[scope.root] = map[string]bool{}
				}
				key := row.Provider + "/" + row.SessionID
				if !transcript.IsFullID(row.Provider, row.SessionID) {
					key = "unknown:" + strconv.FormatInt(row.ID, 10)
				}
				rootGroups[scope.root][key] = true
				continue
			}
			if !matches {
				continue
			}
			info := lookup(row.Provider, row.SessionID)
			if info.workspace.known && info.workspace.root != body.Workspace {
				continue
			}
			records = append(records, navLongLaunch{NavigatorLaunch: row, available: info.available, workspaceKnown: scope.known})
		}
	}
	if body.Kind == "list" {
		for path, row := range workspaces {
			row.Sessions = len(rootGroups[path])
			body.Workspaces = append(body.Workspaces, *row)
		}
		sort.Slice(body.Workspaces, func(i, j int) bool { return body.Workspaces[i].Workspace < body.Workspaces[j].Workspace })
	} else {
		for _, group := range navLongGroups(body.Workspace, records) {
			if recordedKey != "" && group.Provider+"/"+group.SessionID == recordedKey {
				body.CurrentSpans = group.Spans
			} else {
				body.Earlier = append(body.Earlier, group)
			}
		}
		body.State = "none"
		if ambiguous {
			body.State = "unknown"
			body.Partial = true
			body.Lease = nil
			body.Current = nil
		} else if body.Lease != nil {
			if body.Lease.Live == "live" {
				body.State = "live"
			} else if body.Lease.Live == "unknown" {
				body.State = "unknown"
			}
		} else if len(body.Earlier) == 0 {
			if body.Partial || !q.history {
				body.State = "unknown"
			} else {
				body.State = "never"
			}
		}
		if q.history && f.input.notes != nil && ctx.Err() == nil {
			left := f.policy.sessions.rowBytes - run.used
			rows := f.policy.sessions.arrayEntries - int64(len(snapshot.Launches)+len(snapshot.Leases))
			if left <= 0 || rows <= 0 {
				return body, state.ErrNavigatorSnapshotTooLarge
			}
			body.Note, err = f.input.notes(ctx, body.Workspace, state.NavigatorSnapshotLimits{Rows: rows, TextBytes: left})
			if err != nil {
				return body, err
			}
		}
	}
	if ctx.Err() != nil {
		body.Partial = true
	}
	return body, nil
}
