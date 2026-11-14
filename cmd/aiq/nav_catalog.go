package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

type navCatalogDirectory struct {
	Path         string `json:"path"`
	UpdatedMS    int64  `json:"updated_ms"`
	UpdatedKnown bool   `json:"updated_known"`
	Sessions     int    `json:"sessions"`
	Workers      int    `json:"workers"`
}
type navCatalogBody struct {
	Source             string                `json:"source"`
	Partial            bool                  `json:"partial"`
	MetadataKnown      bool                  `json:"metadata_known"`
	Scanned            int                   `json:"scanned"`
	Of                 int                   `json:"of"`
	UnreadableCount    int                   `json:"unreadable_count"`
	UnknownDirectories int                   `json:"unknown_directories"`
	DefaultDir         string                `json:"default_dir"`
	DefaultReason      string                `json:"default_reason"`
	Directories        []navCatalogDirectory `json:"directories"`
}

func navCatalogPath(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path && utf8.ValidString(path) &&
		len(path) <= 4096 && !strings.Contains(path, "\\") &&
		strings.IndexFunc(path, func(r rune) bool { return r < 32 || r == 127 }) < 0 &&
		len("/sessions?dir=")+len(url.QueryEscape(path)) <= 8192
}

// The existing one-builder admission is held by prepareDirectories through the
// response write. Metadata and summaries share its admitted-row allowance.
func (f *navFactory) prepareDirectoryCatalog(parent context.Context) navListResponse {
	ctx, cancel := context.WithTimeout(parent, transcript.ListBudget)
	defer cancel()
	bundle, err := f.get(ctx)
	if err != nil {
		return navListError(503, "directories_unavailable")
	}
	run := &navListRun{Discovery: bundle.discovery, parse: f.cache.Summary, cancel: cancel, limit: f.policy.sessions.rowBytes, needed: map[string]struct{}{}}
	if !run.reserve(512) {
		return navListError(503, "list_too_large")
	}
	rows := map[string]*navCatalogDirectory{}
	eligible := map[string]bool{}
	choose := func(path string) *navCatalogDirectory {
		if row := rows[path]; row != nil {
			return row
		}
		row := &navCatalogDirectory{Path: strings.Clone(path)}
		rows[row.Path] = row
		return row
	}
	body := navCatalogBody{Source: "session index", Directories: []navCatalogDirectory{}}
	if f.input.metadata != nil {
		snapshot, e := f.input.metadata(ctx, state.NavigatorSnapshotLimits{Rows: f.policy.sessions.arrayEntries, TextBytes: f.policy.sessions.rowBytes})
		if errors.Is(e, state.ErrNavigatorSnapshotTooLarge) {
			return navListError(503, "list_too_large")
		}
		if e == nil && snapshot != nil {
			body.MetadataKnown = true
			body.Source = "session index and launch metadata"
			for _, v := range snapshot.Launches {
				if ctx.Err() != nil {
					return navListError(503, "directories_unavailable")
				}
				if !run.reserve(256, v.Provider, v.AccountID, v.Launcher, v.Cwd, v.Mode, v.SessionID) {
					return navListError(503, "list_too_large")
				}
			}
			for _, v := range snapshot.Leases {
				if ctx.Err() != nil {
					return navListError(503, "directories_unavailable")
				}
				if !run.reserve(256, v.Provider, v.AccountID, v.Launcher, v.Cwd, v.Mode, v.Workspace, v.Pane, v.SessionID, v.Drain) {
					return navListError(503, "list_too_large")
				}
			}
			for _, v := range state.NavigatorDirectories(snapshot) {
				if !navCatalogPath(v.Path) {
					return navListError(503, "directories_unavailable")
				}
				if !run.reserve(128, v.Path) {
					return navListError(503, "list_too_large")
				}
				row := choose(v.Path)
				row.UpdatedMS = v.UpdatedMS
				row.UpdatedKnown = true
				eligible[row.Path] = true
			}
		}
	}
	result, err := transcript.ListAll(ctx, run, run.summary)
	flags := run.flags()
	if parent.Err() != nil {
		return navListError(503, "request_cancelled")
	}
	if flags.internal {
		return navListError(500, "internal_error")
	}
	if flags.resource {
		return navListError(503, "list_too_large")
	}
	if flags.discovery || err != nil {
		return navListError(503, "directories_unavailable")
	}
	body.Partial = result.Partial || flags.partial || flags.unavailable || !body.MetadataKnown
	body.Scanned, body.Of, body.UnreadableCount = result.Scanned, result.Of, len(result.Unreadable)
	body.Partial = body.Partial || body.UnreadableCount > 0
	for _, v := range result.Rows {
		if v.Empty {
			continue
		}
		if !navCatalogPath(v.Cwd) {
			body.UnknownDirectories++
			body.Partial = true
			continue
		}
		if !run.reserve(128, v.Cwd) {
			return navListError(503, "list_too_large")
		}
		row := choose(v.Cwd)
		if v.Worker {
			row.Workers++
		} else {
			row.Sessions++
			eligible[row.Path] = true
		}
		if !v.Updated.IsZero() {
			ms := v.Updated.UnixMilli()
			if ms >= -navConversationSafeInteger && ms <= navConversationSafeInteger && (!row.UpdatedKnown || ms > row.UpdatedMS) {
				row.UpdatedMS = ms
				row.UpdatedKnown = true
			}
		}
	}
	body.Directories = make([]navCatalogDirectory, 0, len(rows))
	for _, v := range rows {
		body.Directories = append(body.Directories, *v)
	}
	sort.Slice(body.Directories, func(i, j int) bool {
		a, b := body.Directories[i], body.Directories[j]
		if a.UpdatedKnown != b.UpdatedKnown {
			return a.UpdatedKnown
		}
		if a.UpdatedMS != b.UpdatedMS {
			return a.UpdatedMS > b.UpdatedMS
		}
		return a.Path < b.Path
	})
	for _, v := range body.Directories {
		if eligible[v.Path] {
			body.DefaultDir = v.Path
			body.DefaultReason = "directory fallback"
			if v.UpdatedKnown {
				body.DefaultReason = "most recent activity"
			}
			break
		}
	}
	if len(body.Directories) > transcript.ListRows {
		body.Directories = body.Directories[:transcript.ListRows]
		body.Partial = true
	}
	// A default outside the bounded response cannot become an invisible scope.
	if body.DefaultDir != "" {
		found := false
		for _, v := range body.Directories {
			if v.Path == body.DefaultDir {
				found = true
				break
			}
		}
		if !found {
			body.DefaultDir = ""
			body.DefaultReason = ""
		}
	}
	if f.policy.sessions.arrayEntries > 0 && int64(len(body.Directories)) > f.policy.sessions.arrayEntries {
		return navListError(503, "list_too_large")
	}
	size, ok := navJSONSize(body, f.policy.sessions.bodyBytes)
	if !ok {
		return navListError(503, "list_too_large")
	}
	encoded, err := json.Marshal(body)
	if err != nil || int64(len(encoded)) != size {
		return navListError(500, "internal_error")
	}
	if parent.Err() != nil {
		return navListError(503, "request_cancelled")
	}
	return navListResponse{status: 200, body: encoded}
}
