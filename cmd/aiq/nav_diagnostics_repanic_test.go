package main

import (
	"context"
	"github.com/orlenko/aiq/internal/diagnostic"
	"github.com/orlenko/aiq/internal/transcript"
	"strings"
	"testing"
)

func navHTTPActualLegacyFailure(t *testing.T, unknown bool) (error, diagnostic.Origin) {
	t.Helper()
	restore := transcript.SetParserForTest(func(string) (*transcript.Session, error) {
		if unknown {
			strings.Repeat("invented", -1)
		}
		navHTTPDiagnosticsLeaf()
		return nil, nil
	})
	defer restore()
	_, err := transcript.ParseFile("invented-private-repanic-path.jsonl")
	origin := transcript.PanicOrigin(err)
	file, line, known := origin.Location()
	if err == nil || !origin.Present() || unknown == known || known && (file != "cmd/aiq/nav_diagnostics_leaf_test.go" || line != 3) {
		t.Fatal("actual legacy adapter fixture lost original leaf")
	}
	return err, origin
}

func TestNavDiagnosticsLegacyRepanicInnerGuards(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		carrier, origin := navHTTPActualLegacyFailure(t, unknown)
		for _, where := range []string{"detail", "sessions prepare", "summary", "names"} {
			t.Run(where+map[bool]string{true: " unknown", false: " known"}[unknown], func(t *testing.T) {
				ctx, holder := diagnostic.WithHolder(context.Background())
				if where == "detail" {
					f := detailTestNew(t)
					f.deps.resolve = func(context.Context, string, string) (transcript.Hit, error) { panic(carrier) }
					h := f.handler(t)
					w := detailTestServe(h, detailTestRequest().WithContext(ctx))
					detailTestError(t, w, 500, "internal_error")
					if len(h.slots) != 0 {
						t.Fatal("detail slot retained")
					}
				} else {
					h := navTestHandler(t, []transcript.Summary{{Provider: "codex", ID: navTestID}}, func(d *navSessionsDeps, _ *navSessionsLimits) {
						switch where {
						case "sessions prepare":
							d.listContext = func(context.Context) (context.Context, context.CancelFunc) { panic(carrier) }
						case "summary":
							d.summary = func(context.Context, string) (transcript.Summary, error) { panic(carrier) }
						case "names":
							d.source = navTestSource{candidates: func(context.Context, string) ([]transcript.Candidate, error) {
								return []transcript.Candidate{{Provider: "codex", Path: "invented"}}, nil
							}, names: func(context.Context) map[string]string { panic(carrier) }}
							d.summary = func(context.Context, string) (transcript.Summary, error) {
								return transcript.Summary{Provider: "codex", ID: navTestID}, nil
							}
						}
					})
					w := navTestServe(h, navTestRequest("/fixture/work").WithContext(ctx))
					navTestResponse(t, w, 500, `{"error":"internal_error"}`)
					if len(h.slots) != 0 {
						t.Fatal("Sessions slot retained")
					}
				}
				if holder.Origin() != origin {
					t.Fatal("HTTP guard replaced original legacy leaf with re-panic site")
				}
			})
		}
	}
}
