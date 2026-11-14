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
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

type navAskTurnBody struct {
	Provider   string `json:"provider"`
	SessionID  string `json:"session_id"`
	Anchor     *int64 `json:"anchor"`
	Confidence string `json:"confidence"`
}
type navAskTurnDeps struct {
	read    func(context.Context, int64, int64) (state.AskPlacement, error)
	resolve func(context.Context, string, string) (transcript.Hit, error)
	cache   *transcript.Cache
}
type navAskTurnHandler struct {
	d                     navAskTurnDeps
	slots                 chan struct{} // Shared with the Asks list, not a second allowance.
	inputBytes, bodyBytes int64
}

func newNavAskTurnHandler(d navAskTurnDeps, slots chan struct{}, inputBytes, bodyBytes int64) (*navAskTurnHandler, error) {
	if d.read == nil || d.resolve == nil || d.cache == nil || slots == nil || cap(slots) <= 0 || inputBytes <= 0 || bodyBytes < 64 {
		return nil, errors.New("invalid ask turn configuration")
	}
	return &navAskTurnHandler{d: d, slots: slots, inputBytes: inputBytes, bodyBytes: bodyBytes}, nil
}
func navAskTurnID(req *http.Request) (int64, bool) {
	if req == nil || req.URL == nil {
		return 0, false
	}
	u := req.URL
	if u.Scheme != "" || u.Host != "" || u.User != nil || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.Opaque != "" || req.ContentLength > 0 || len(req.TransferEncoding) > 0 {
		return 0, false
	}
	parts := strings.Split(u.Path, "/")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "api" || parts[2] != "nav" || parts[3] != "asks" || parts[5] != "turn" || req.RequestURI != "" && req.RequestURI != u.Path {
		return 0, false
	}
	id, err := strconv.ParseInt(parts[4], 10, 64)
	return id, err == nil && id > 0 && strconv.FormatInt(id, 10) == parts[4]
}
func (h *navAskTurnHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	var release func()
	defer func() {
		if release != nil {
			release()
		}
	}()
	status, body := h.prepare(req, &release)
	if req != nil && req.Context().Err() != nil {
		status, body = 503, []byte(`{"error":"request_cancelled"}`)
	}
	headers := w.Header()
	headers.Set("Content-Type", "application/json")
	headers.Set("Cache-Control", "no-store")
	if status == 405 {
		headers.Set("Allow", "GET")
	}
	headers.Set("Content-Length", strconv.Itoa(len(body)))
	daemon.WriteNavigatorBody(w, req, headers, status, body)
}
func (h *navAskTurnHandler) prepare(req *http.Request, release *func()) (status int, body []byte) {
	defer func() {
		if value := recover(); value != nil {
			if value == http.ErrAbortHandler {
				panic(value)
			}
			origin := transcript.CapturePanic(value)
			if req != nil {
				diagnostic.Note(req.Context(), origin)
			}
			status, body = 500, []byte(`{"error":"internal_error"}`)
		}
	}()
	id, ok := navAskTurnID(req)
	if !ok {
		return 400, []byte(`{"error":"invalid_request"}`)
	}
	if req.Method != "GET" {
		return 405, []byte(`{"error":"method_not_allowed"}`)
	}
	ctx := req.Context()
	if ctx.Err() != nil {
		return 503, []byte(`{"error":"request_cancelled"}`)
	}
	select {
	case h.slots <- struct{}{}:
		*release = func() { <-h.slots }
	default:
		return 503, []byte(`{"error":"asks_busy"}`)
	}
	ask, err := h.d.read(ctx, id, h.inputBytes)
	diagnostic.Note(ctx, diagnostic.FromError(err))
	if ctx.Err() != nil {
		return 503, []byte(`{"error":"request_cancelled"}`)
	}
	if err != nil {
		if err == state.ErrAskPlacementMissing {
			return 404, []byte(`{"error":"not_found"}`)
		}
		if err == state.ErrAsksSnapshotTooLarge {
			return 503, []byte(`{"error":"asks_too_large"}`)
		}
		return 503, []byte(`{"error":"placement_unavailable"}`)
	}
	if !transcript.IsFullID(ask.Provider, ask.SessionID) {
		return 404, []byte(`{"error":"not_found"}`)
	}
	out := navAskTurnBody{Provider: ask.Provider, SessionID: ask.SessionID, Confidence: "none"}
	hit, err := h.d.resolve(ctx, ask.Provider, ask.SessionID)
	diagnostic.Note(ctx, transcript.PanicOrigin(err))
	if err == nil {
		var s *transcript.Session
		s, _, err = h.d.cache.Session(ctx, hit.Path)
		diagnostic.Note(ctx, transcript.PanicOrigin(err))
		if err == nil {
			if s == nil || s.Provider != ask.Provider || s.ID != ask.SessionID {
				return 409, []byte(`{"error":"conversation_changed"}`)
			}
			out.Anchor, err = navPlaceAsk(ctx, ask, s)
			if out.Anchor != nil {
				out.Confidence = "time+text"
			}
		}
	}
	if ctx.Err() != nil {
		return 503, []byte(`{"error":"request_cancelled"}`)
	}
	if err != nil && !errors.Is(err, transcript.ErrNoSuchSession) {
		return 503, []byte(`{"error":"placement_unavailable"}`)
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return 500, []byte(`{"error":"internal_error"}`)
	}
	if int64(len(encoded)) > h.bodyBytes {
		return 503, []byte(`{"error":"asks_too_large"}`)
	}
	return 200, encoded
}

// An ask's raised_at is observation time, not a prompt timestamp. Require a
// unique literal quote match with chronological support; do not guess an at
// value from seconds or choose between repeated matches. No quote is returned.
func navPlaceAsk(ctx context.Context, a state.AskPlacement, s *transcript.Session) (*int64, error) {
	if ctx == nil || s == nil {
		return nil, errNavConversationInvalid
	}
	var found int64
	for _, turn := range s.Turns {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if turn.Started.IsZero() || turn.Started.Unix() > a.RaisedAt {
			continue
		}
		at := turn.Started.UnixMilli()
		if at < 1 || at > 9999999999999 {
			continue
		}
		matches := false
		contains := func(text string) {
			for _, q := range []string{a.FirstQuote, a.LatestQuote} {
				if len(strings.TrimSpace(q)) >= 16 && strings.Contains(text, q) {
					matches = true
				}
			}
		}
		contains(turn.Reply)
		for _, text := range turn.Earlier {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			contains(text)
		}
		for _, item := range turn.Items {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if item.Kind == transcript.ItemSaid {
				contains(item.Text)
			}
		}
		if matches {
			if found != 0 {
				return nil, nil
			}
			found = at
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if found == 0 {
		return nil, nil
	}
	return &found, nil
}
