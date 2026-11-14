package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/orlenko/aiq/internal/navauth"
)

func TestNavInputCreatorBorrowerAndClosure(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/nav/login", strings.NewReader(`{"action":"issue"}`))
	r.TransferEncoding = []string{"chunked"}
	r.ContentLength = -1
	owner, creator := ownNavInput(r)
	if !creator || owner == nil || owner.original != r {
		t.Fatal("original input not acquired")
	}
	copy := r.Clone(r.Context())
	borrowed, created := ownNavInput(copy)
	if borrowed != owner || created || !owner.open(copy) {
		t.Fatal("clone replaced input owner")
	}
	owner.close()
	borrowed, created = ownNavInput(copy)
	if borrowed != owner || created || owner.open(copy) || owner.original != nil {
		t.Error("closed input reopened or retained original")
	}
}

func TestNavAuthHTTPMissingClosedAndCachedInput(t *testing.T) {
	for _, route := range []string{"login", "revoke"} {
		for _, mode := range []string{"missing", "closed", "closed mutated transfer", "cached closed", "cached mutated transfer"} {
			t.Run(route+"/"+mode, func(t *testing.T) {
				f := newNavAuthHTTPFixture(t)
				body := `{"action":"issue"}`
				want := 400
				if route == "revoke" {
					body = `{}`
					want = 401
				}
				r, proof := f.request(t, "POST", "/api/nav/"+route, body, true)
				reader := &navAuthHTTPReader{body: strings.NewReader(body)}
				r.Body = reader
				r.ContentLength = -1
				r.TransferEncoding = []string{"chunked"}
				if mode != "missing" {
					owner, created := ownNavInput(r)
					if !created {
						t.Fatal("input fixture failed")
					}
					if strings.HasPrefix(mode, "cached") {
						if _, ok := f.a.ownedBody(r); !ok {
							t.Fatal("cached fixture did not validate")
						}
					}
					owner.close()
					if strings.Contains(mode, "mutated transfer") {
						r.TransferEncoding = nil
						r.ContentLength = int64(len(body))
					}
				}
				reader.reads = 0
				issue, exchange, sign := navAuthHTTPCountActions(f)
				revokes := 0
				originalRevoke := f.a.revoke
				f.a.revoke = func(v *navauth.Facts) error { revokes++; return originalRevoke(v) }
				gate := &navGate{verifier: f.a, login: http.HandlerFunc(f.a.Login), revoke: http.HandlerFunc(f.a.Revoke)}
				w := httptest.NewRecorder()
				gate.ServeHTTP(w, r)
				if w.Code != want || reader.reads != 0 || *issue != 0 || *exchange != 0 || *sign != 0 || revokes != 0 {
					t.Error("unowned/closed input read, admitted or acted")
				}
				if route == "login" && w.Body.String() != `{"error":"invalid_request"}` || route == "revoke" && w.Body.Len() != 0 {
					t.Error("route-specific refusal changed")
				}
				fresh, _ := f.request(t, "POST", "/api/nav/"+route, body, false)
				fresh.Header.Set("X-Aiq-Proof", proof.String())
				positive := httptest.NewRecorder()
				gate.ServeHTTP(positive, fresh)
				if positive.Code != 200 {
					t.Error("pre-admission refusal consumed nonce")
				}
			})
		}
	}
}

func TestNavInputOriginalAndCurrentTrailerMaps(t *testing.T) {
	for _, side := range []string{"original", "current"} {
		t.Run(side, func(t *testing.T) {
			f := newNavAuthHTTPFixture(t)
			original, _ := f.request(t, "POST", "/api/nav/login", `{"action":"issue"}`, true)
			original.ContentLength = -1
			original.TransferEncoding = []string{"chunked"}
			owner, _ := ownNavInput(original)
			defer owner.close()
			current := original.Clone(original.Context())
			reader := &navAuthHTTPReader{body: strings.NewReader(`{"action":"issue"}`)}
			reader.before = func() {
				target := original
				if side == "current" {
					target = current
				}
				target.Trailer = http.Header{"X-Invented": []string{"value"}}
			}
			current.Body = reader
			if _, ok := f.a.ownedBody(current); ok {
				t.Error("trailer map not refused after traversal")
			}
		})
	}
}

type navInputSetupContext struct{ context.Context }
type navInputFaultKey struct{}

func (c navInputSetupContext) Value(key any) any {
	if _, ok := key.(navInputKey); ok {
		return nil
	}
	panic("invented setup failure")
}

func TestNavInputClearsBeforeFinalCompletion(t *testing.T) {
	for _, mode := range []string{"normal", "panic", "abort", "header", "status", "partial", "parent context panic"} {
		t.Run(mode, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/api/nav/login", strings.NewReader(`{}`))
			r.ContentLength = -1
			r.TransferEncoding = []string{"chunked"}
			if mode == "parent context panic" {
				r = r.WithContext(navInputSetupContext{r.Context()})
			}
			completed := 0
			h := navRecover(nil, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				owner := req.Context().Value(navInputKey{}).(*navInput)
				completion := req.Context().Value(navCompletionKey{}).(*navCompletion)
				if !completion.adopt(func() {
					completed++
					if owner.original != nil {
						t.Error("input retained when final slot completion ran")
					}
				}) {
					t.Fatal("completion fixture failed")
				}
				switch mode {
				case "parent context panic":
					req.Context().Value(navInputFaultKey{})
				case "panic", "header", "status":
					navAuthHTTPLeaf()
				case "abort":
					panic(http.ErrAbortHandler)
				case "partial":
					w.Write([]byte("partial"))
					navAuthHTTPLeaf()
				default:
					w.WriteHeader(204)
				}
			}))
			writer := &navAuthHTTPWriter{header: make(http.Header), mode: mode, value: "invented output failure"}
			value, panicked := navAuthHTTPCatch(func() { h.ServeHTTP(writer, r) })
			wantAbort := mode == "abort" || mode == "header" || mode == "status" || mode == "partial"
			if wantAbort && (!panicked || value != http.ErrAbortHandler) || !wantAbort && panicked {
				t.Error("wrong completion escape")
			}
			if mode == "parent context panic" && writer.status != 500 {
				t.Error("parent-context fixture did not exercise recovery")
			}
			owner, ok := r.Context().Value(navInputKey{}).(*navInput)
			if !ok || owner.original != nil {
				t.Error("final cleanup did not clear original request")
			}
			if completed != 1 {
				t.Error("final completion count changed")
			}
		})
	}
}

func TestNavInputBorrowedEnvelopeDoesNotCloseCreator(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/nav/login", strings.NewReader(`{}`))
	r.ContentLength = -1
	r.TransferEncoding = []string{"chunked"}
	owner, created := ownNavInput(r)
	if !created {
		t.Fatal("creator fixture failed")
	}
	defer owner.close()
	navRecover(nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })).ServeHTTP(httptest.NewRecorder(), r)
	if !owner.open(r) || owner.original != r {
		t.Error("nested envelope closed creator input")
	}
}
