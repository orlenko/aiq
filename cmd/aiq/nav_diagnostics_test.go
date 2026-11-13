package main

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/orlenko/aiq/internal/diagnostic"
	"github.com/orlenko/aiq/internal/transcript"
)

func navHTTPDiagnosticsOrigin() (out diagnostic.Origin) {
	defer func() {
		if value := recover(); value != nil {
			out = diagnostic.Capture(value)
		}
	}()
	navHTTPDiagnosticsLeaf()
	return
}

func TestNavDiagnosticsCandidatesReturnedBeforeCancellation(t *testing.T) {
	for _, origin := range []diagnostic.Origin{navHTTPDiagnosticsOrigin(), diagnostic.Capture(nil), {}} {
		ctx, holder := diagnostic.WithHolder(context.Background())
		var calls atomic.Int32
		h := navTestHandler(t, nil, func(d *navSessionsDeps, l *navSessionsLimits) {
			d.source = navTestSource{candidates: func(context.Context, string) ([]transcript.Candidate, error) {
				calls.Add(1)
				// This dependency does not self-Note. The adapter cancels its
				// child after returning, and ListWith reduces the error.
				return nil, diagnostic.Failure(diagnostic.Inspection, origin)
			}}
		})
		w := navTestServe(h, navTestRequest("/fixture/work").WithContext(ctx))
		navTestResponse(t, w, 500, `{"error":"internal_error"}`)
		if calls.Load() != 1 || holder.Origin() != origin || len(h.slots) != 0 {
			t.Fatalf("lost returned origin/cleanup: calls=%d slots=%d", calls.Load(), len(h.slots))
		}
	}
}

func TestNavDiagnosticsListExistingRecoveryGuards(t *testing.T) {
	for _, where := range []string{"summary", "names", "prepare"} {
		t.Run(where, func(t *testing.T) {
			ctx, holder := diagnostic.WithHolder(context.Background())
			h := navTestHandler(t, []transcript.Summary{{Provider: "codex", ID: navTestID}}, func(d *navSessionsDeps, l *navSessionsLimits) {
				switch where {
				case "summary":
					d.summary = func(context.Context, string) (transcript.Summary, error) {
						navHTTPDiagnosticsLeaf()
						return transcript.Summary{}, nil
					}
				case "names":
					d.source = navTestSource{candidates: func(context.Context, string) ([]transcript.Candidate, error) {
						return []transcript.Candidate{{Provider: "codex", Path: "invented"}}, nil
					}, names: func(context.Context) map[string]string { navHTTPDiagnosticsLeaf(); return nil }}
					d.summary = func(context.Context, string) (transcript.Summary, error) {
						return transcript.Summary{Provider: "codex", ID: navTestID}, nil
					}
				case "prepare":
					d.listContext = func(context.Context) (context.Context, context.CancelFunc) { navHTTPDiagnosticsLeaf(); return nil, nil }
				}
			})
			w := navTestServe(h, navTestRequest("/fixture/work").WithContext(ctx))
			navTestResponse(t, w, 500, `{"error":"internal_error"}`)
			file, line, known := holder.Origin().Location()
			if !known || file != "cmd/aiq/nav_diagnostics_leaf_test.go" || line != 3 || len(h.slots) != 0 {
				t.Fatal("existing recovery lost literal origin/slot")
			}
		})
	}
}

func TestNavDiagnosticsDetailReturnedMarshalAndReplay(t *testing.T) {
	for _, origin := range []diagnostic.Origin{navHTTPDiagnosticsOrigin(), diagnostic.Capture(nil), {}} {
		f := detailTestNew(t)
		var encodes atomic.Int32
		f.deps.marshal = func(any) ([]byte, error) {
			encodes.Add(1)
			return nil, diagnostic.Failure(diagnostic.Projection, origin)
		}
		h := f.handler(t)
		for range 2 {
			ctx, holder := diagnostic.WithHolder(context.Background())
			w := detailTestServe(h, detailTestRequest().WithContext(ctx))
			detailTestError(t, w, 500, "unshowable")
			if holder.Origin() != origin {
				t.Fatal("returned marshal/replay lost original value")
			}
		}
		if encodes.Load() != 1 || len(h.slots) != 0 {
			t.Fatalf("broken replay rebuilt or retained slot: %d/%d", encodes.Load(), len(h.slots))
		}
	}
}

func TestNavDiagnosticsDetailEarlyReturnedAndRecovery(t *testing.T) {
	for _, where := range []string{"resolve", "load", "facts", "raw prepare", "project", "marshal length"} {
		t.Run(where, func(t *testing.T) {
			f := detailTestNew(t)
			a := navHTTPDiagnosticsOrigin()
			fault := diagnostic.Failure(diagnostic.Inspection, a)
			switch where {
			case "resolve":
				f.deps.resolve = func(context.Context, string, string) (transcript.Hit, error) { return transcript.Hit{}, fault }
			case "load":
				f.deps.load = func(context.Context, string) (*transcript.Session, transcript.Snapshot, error) {
					return nil, transcript.Snapshot{}, fault
				}
			case "facts":
				f.deps.facts = func(context.Context, transcript.Hit, *transcript.Session) (navDetailFacts, error) {
					return navDetailFacts{}, fault
				}
			case "raw prepare":
				f.deps.resolve = func(context.Context, string, string) (transcript.Hit, error) {
					navHTTPDiagnosticsLeaf()
					return transcript.Hit{}, nil
				}
			case "project":
				f.deps.project = func(*transcript.Session, navConversationOptions) (*navConversationWindow, error) { return nil, fault }
			case "marshal length":
				f.deps.marshal = func(any) ([]byte, error) { return []byte("invented invalid length"), nil }
			}
			ctx, holder := diagnostic.WithHolder(context.Background())
			h := f.handler(t)
			w := detailTestServe(h, detailTestRequest().WithContext(ctx))
			if w.status != 500 || len(h.slots) != 0 {
				t.Fatalf("response/cleanup=%d/%d", w.status, len(h.slots))
			}
			if where == "marshal length" {
				if holder.Origin().Present() {
					t.Fatal("length control fabricated origin")
				}
			} else {
				file, line, known := holder.Origin().Location()
				if !known || file != "cmd/aiq/nav_diagnostics_leaf_test.go" || line != 3 {
					t.Fatal("lost early first origin")
				}
			}
		})
	}
}

func TestNavDiagnosticsDetailReturnedProjectBridge(t *testing.T) {
	// A closed normally returned project error crosses the detail-local and
	// operation-owned holders without becoming a different ordinary result.
	for _, origin := range []diagnostic.Origin{navHTTPDiagnosticsOrigin(), diagnostic.Capture(nil)} {
		f := detailTestNew(t)
		f.deps.project = func(*transcript.Session, navConversationOptions) (*navConversationWindow, error) {
			return nil, diagnostic.Failure(diagnostic.Unshowable, origin)
		}
		h := f.handler(t)
		ctx, holder := diagnostic.WithHolder(context.Background())
		w := detailTestServe(h, detailTestRequest().WithContext(ctx))
		if w.status != 500 || holder.Origin() != origin {
			t.Fatal("detail local/worker value bridge")
		}
		if !strings.Contains(string(w.body), "unshowable") || !errors.Is(diagnostic.Failure(diagnostic.Unshowable, origin), transcript.ErrUnshowable) {
			t.Fatal("classification changed")
		}
	}
}
