package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/orlenko/aiq/internal/daemon"
	"github.com/orlenko/aiq/internal/diagnostic"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

type navDirectoriesBody struct {
	Source      string            `json:"source"`
	Directories []navDirectoryRow `json:"directories"`
}
type navDirectoryRow struct {
	Path      string `json:"path"`
	UpdatedMS int64  `json:"updated_ms"`
}

func (f *navFactory) directories(w http.ResponseWriter, r *http.Request) {
	var release func()
	defer func() {
		if release != nil {
			release()
		}
	}()
	response := f.prepareDirectories(r, &release)
	if r.Context().Err() != nil {
		response = navListError(503, "request_cancelled")
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	daemon.WriteNavigatorBody(w, r, h, response.status, response.body)
}
func (f *navFactory) prepareDirectories(r *http.Request, release *func()) (out navListResponse) {
	defer func() {
		if v := recover(); v != nil {
			if v == http.ErrAbortHandler {
				panic(v)
			}
			diagnostic.Note(r.Context(), transcript.CapturePanic(v))
			out = navListError(500, "internal_error")
		}
	}()
	if r.URL == nil || r.URL.Path != "/api/nav/directories" || r.URL.RawPath != "" || r.URL.RawQuery != "" && r.URL.RawQuery != "catalog=1" || r.URL.ForceQuery || r.URL.Fragment != "" || r.URL.Opaque != "" || r.RequestURI != "" && r.RequestURI != r.URL.RequestURI() || r.Method != http.MethodGet {
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
	if r.URL.RawQuery == "catalog=1" {
		return f.prepareDirectoryCatalog(ctx)
	}
	if f.input.metadata == nil {
		return navListError(503, "directories_unavailable")
	}
	snapshot, err := f.input.metadata(ctx, state.NavigatorSnapshotLimits{Rows: f.policy.sessions.arrayEntries, TextBytes: f.policy.sessions.rowBytes})
	if err != nil || snapshot == nil || ctx.Err() != nil {
		if err == state.ErrNavigatorSnapshotTooLarge {
			return navListError(503, "list_too_large")
		}
		return navListError(503, "directories_unavailable")
	}
	rows := state.NavigatorDirectories(snapshot)
	if int64(len(rows)) > f.policy.sessions.arrayEntries {
		return navListError(503, "list_too_large")
	}
	body := navDirectoriesBody{Source: "launch metadata", Directories: make([]navDirectoryRow, 0, len(rows))}
	for _, row := range rows {
		// The browser's closed absolute-directory codec has a 4096-byte input
		// ceiling. Unrepresentable rows cannot become selectable scope options.
		if len(row.Path) > 4096 || strings.Contains(row.Path, "\\") || strings.IndexFunc(row.Path, func(r rune) bool { return r < 32 || r == 127 }) >= 0 || len("/sessions?dir=")+len(url.QueryEscape(row.Path)) > 8192 {
			return navListError(503, "directories_unavailable")
		}
		body.Directories = append(body.Directories, navDirectoryRow{row.Path, row.UpdatedMS})
	}
	size, ok := navJSONSize(body, f.policy.sessions.bodyBytes)
	if !ok {
		return navListError(503, "list_too_large")
	}
	encoded, err := json.Marshal(body)
	if err != nil || int64(len(encoded)) != size {
		return navListError(500, "internal_error")
	}
	if ctx.Err() != nil {
		return navListError(503, "directories_unavailable")
	}
	return navListResponse{status: 200, body: encoded}
}
