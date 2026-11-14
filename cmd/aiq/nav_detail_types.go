package main

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/orlenko/aiq/internal/transcript"
)

var (
	errNavDetailConfiguration = errors.New("invalid conversation HTTP configuration")
	errNavDetailChanged       = errors.New("conversation changed")
)

type navDetailLimits struct {
	bodyBytes int64
	requests  int
	// Both zero preserves the unmounted component's earlier byte-only policy.
	// The production factory must supply both positive structural allowances.
	arrayEntries, stringUnits int64
}

type navDetailFacts struct {
	live, hold, stale, leaseKnown bool
	leaseIdentity                 string
	accounts                      []convoAccountFact
}

type navDetailDeps struct {
	cache       *transcript.Cache
	resolve     func(context.Context, string, string) (transcript.Hit, error)
	facts       func(context.Context, transcript.Hit, *transcript.Session) (navDetailFacts, error)
	now         func() time.Time
	location    *time.Location
	build       string
	skippedNote string
	liveMark    string
	// Controlled instance-local seams. Production defaults bind initial load
	// to this exact Cache and use the ordinary pure projection/JSON encoder.
	requestContext func(context.Context) (context.Context, context.CancelFunc)
	load           func(context.Context, string) (*transcript.Session, transcript.Snapshot, error)
	project        func(*transcript.Session, navConversationOptions) (*navConversationWindow, error)
	marshal        func(any) ([]byte, error)
}

// Explicit closed envelope: no private structs, embedding or second encoding.
type navDetailEnvelope struct {
	Build         string                   `json:"build"`
	Session       navConversationSession   `json:"session"`
	EarlierHidden int                      `json:"earlier_hidden"`
	LaterHidden   int                      `json:"later_hidden"`
	WindowStart   int                      `json:"window_start"`
	Reset         bool                     `json:"reset"`
	Markup        bool                     `json:"markup"`
	Messages      []navConversationMessage `json:"messages"`
}

type navDetailView struct {
	envelope      navDetailEnvelope
	body          []byte
	fingerprint   string
	encodedLength int64
}

type navDetailError struct {
	Build   string `json:"build"`
	Error   string `json:"error"`
	WaitedS int    `json:"waited_s"`
}

type navDetailCode int

const (
	navDetailOK navDetailCode = iota
	navDetailInvalid
	navDetailNotFound
	navDetailMethod
	navDetailSessionMissing
	navDetailAnchorMissing
	navDetailTranscriptLarge
	navDetailViewLarge
	navDetailSelectionChanged
	navDetailPreparing
	navDetailCancelled
	navDetailUnshowable
	navDetailInternal
	navDetailCodeCount
)

var navDetailCodes = [...]struct {
	status int
	name   string
}{
	navDetailOK:               {http.StatusOK, ""},
	navDetailInvalid:          {http.StatusBadRequest, "invalid_request"},
	navDetailNotFound:         {http.StatusNotFound, "not_found"},
	navDetailMethod:           {http.StatusMethodNotAllowed, "method_not_allowed"},
	navDetailSessionMissing:   {http.StatusNotFound, "session_not_found"},
	navDetailAnchorMissing:    {http.StatusNotFound, "anchor_not_found"},
	navDetailTranscriptLarge:  {http.StatusRequestEntityTooLarge, "transcript_too_large"},
	navDetailViewLarge:        {http.StatusServiceUnavailable, "view_too_large"},
	navDetailSelectionChanged: {http.StatusConflict, "conversation_changed"},
	navDetailPreparing:        {http.StatusServiceUnavailable, "preparing"},
	navDetailCancelled:        {http.StatusServiceUnavailable, "request_cancelled"},
	navDetailUnshowable:       {http.StatusInternalServerError, "unshowable"},
	navDetailInternal:         {http.StatusInternalServerError, "internal_error"},
}

type navDetailResponse struct {
	status int
	code   navDetailCode
	body   []byte
	etag   string
}
