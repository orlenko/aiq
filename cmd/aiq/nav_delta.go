package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/orlenko/aiq/internal/diagnostic"
	"github.com/orlenko/aiq/internal/transcript"
)

func navDeltaKey(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// A proof over every rendered field before the changing tail, including raw
// copy text, origin/account facts, skipped notices and paint. The private source
// scope also binds the selected copy, inode, root bundle and managed context.
func navDeltaPrefix(ctx context.Context, e *navDetailEnvelope, turn int64, source string) (string, error) {
	h := sha256.New()
	frame := navDetailFrame{h: h, parent: ctx, child: ctx}
	if err := frame.add("nav-complete-prefix-v1", source, e.Build, e.Session.Provider, e.Session.ID, strconv.Itoa(e.WindowStart), navDetailBool(e.Markup), strconv.FormatInt(turn, 10)); err != nil {
		return "", err
	}
	encoder := json.NewEncoder(h)
	for _, message := range e.Messages {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if int64(message.Turn) >= turn {
			break
		}
		if err := encoder.Encode(message); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Only this request's shallow envelope is changed. Cached full windows and
// their ETags remain immutable, and delta parameters never multiply view keys.
func (h *navDetailHandler) deltaResponse(ctx context.Context, value *navDetailView, q navDetailQuery, source string) (navDetailResponse, bool, error) {
	full := navDetailResponse{status: http.StatusOK, body: value.body, etag: value.fingerprint}
	e := value.envelope
	if q.fromTurn < int64(e.WindowStart) || q.fromTurn > int64(e.TailTurn) || !e.Reset || e.LaterHidden != 0 {
		return full, true, nil
	}
	key, err := navDeltaPrefix(ctx, &e, q.fromTurn, source)
	if err != nil {
		return navDetailResponse{}, false, err
	}
	if !hmac.Equal([]byte(key), []byte(q.fromKey)) {
		return full, true, nil
	}
	cut := 0
	for cut < len(e.Messages) && int64(e.Messages[cut].Turn) < q.fromTurn {
		cut++
	}
	e.Messages = e.Messages[cut:]
	e.Reset = false
	e.FromTurn = q.fromTurn
	if err := navDetailAdmitStructure(ctx, &e, h.limits.arrayEntries, h.limits.stringUnits); err != nil {
		return navDetailResponse{}, false, err
	}
	size, ok := navJSONSize(e, h.limits.bodyBytes)
	if !ok {
		return navDetailResponse{}, false, errNavConversationTooLarge
	}
	body, err := h.deps.marshal(e)
	if err != nil {
		return navDetailResponse{}, false, diagnostic.Failure(diagnostic.Unshowable, transcript.PanicOrigin(err))
	}
	if int64(len(body)) != size {
		return navDetailResponse{}, false, transcript.ErrUnshowable
	}
	if err := ctx.Err(); err != nil {
		return navDetailResponse{}, false, err
	}
	digest := sha256.Sum256(body)
	etag := hex.EncodeToString(digest[:])
	if q.condition.match(etag) {
		return navDetailResponse{status: http.StatusNotModified, etag: etag}, true, nil
	}
	return navDetailResponse{status: http.StatusOK, body: body, etag: etag}, true, nil
}
