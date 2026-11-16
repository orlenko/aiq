package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/orlenko/aiq/internal/daemon"
	"github.com/orlenko/aiq/internal/navauth"
	"github.com/orlenko/aiq/internal/state"
)

// The mutation is a fixed empty-body POST. Its entire canonical query is in
// the dedicated browser action proof; it never enters the replaceable read
// queue or executes a terminal command.
func (f *navFactory) askAction(w http.ResponseWriter, r *http.Request) {
	write := func(status int, body []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		daemon.WriteNavigatorBody(w, r, w.Header(), status, body)
	}
	if r.Method != http.MethodPost || r.URL == nil || r.URL.RawPath != "" || r.URL.ForceQuery || r.URL.Scheme != "" || r.URL.Host != "" || r.URL.Opaque != "" || r.URL.Fragment != "" || r.URL.RawFragment != "" || r.URL.User != nil ||
		r.ContentLength != 0 || len(r.TransferEncoding) != 0 || r.Body != nil && r.Body != http.NoBody || !navauth.ActionTargetValid(r.URL.RequestURI()) || r.RequestURI != "" && r.RequestURI != r.URL.RequestURI() {
		write(400, []byte(`{"error":"invalid_request"}`))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), f.policy.timeout)
	defer cancel()
	select {
	case f.askSlots <- struct{}{}:
		defer func() { <-f.askSlots }()
	default:
		write(503, []byte(`{"error":"asks_busy"}`))
		return
	}
	query := r.URL.Query()
	id, err := strconv.ParseInt(query.Get("id"), 10, 64)
	if err != nil {
		write(400, []byte(`{"error":"invalid_request"}`))
		return
	}
	to := state.AskDismissed
	if query.Get("action") == "restore" {
		to = state.AskOpen
	}
	store, err := state.OpenHook(f.input.asksPath)
	if err != nil {
		write(503, []byte(`{"error":"asks_unavailable"}`))
		return
	}
	defer store.Close()
	item, err := store.SetAskByPersonRevision(ctx, id, query.Get("revision"), to, f.input.now().Unix(), f.policy.asks.reader.TextBytes)
	if errors.Is(err, state.ErrAskActionChanged) {
		write(409, []byte(`{"error":"ask_changed"}`))
		return
	}
	if err != nil {
		write(503, []byte(`{"error":"asks_unavailable"}`))
		return
	}
	body, err := json.Marshal(struct {
		ID       string `json:"id"`
		State    string `json:"state"`
		Revision string `json:"revision"`
	}{strconv.FormatInt(item.ID, 10), string(item.State), state.AskActionRevision(item)})
	if err != nil {
		write(500, []byte(`{"error":"internal_error"}`))
		return
	}
	write(200, body)
}
