package daemon

import (
	"context"
	"net/http"

	"github.com/orlenko/aiq/internal/navauth"
)

type navTabFactsKey struct{}

// AuthorizeRead is the production-only request verifier. Generic gate fixtures
// retain their explicitly supplied legacy Credential seam, never this adapter.
func (a *navAuthAdapter) AuthorizeRead(r *http.Request) (*http.Request, bool) {
	if a == nil || a.state == nil || r == nil || r.URL == nil || r.Method != "GET" ||
		r.ContentLength != 0 || len(r.TransferEncoding) != 0 || r.Body != nil && r.Body != http.NoBody ||
		r.URL.Scheme != "" || r.URL.Host != "" || r.URL.User != nil || r.URL.Opaque != "" || r.URL.Fragment != "" ||
		navTabHasHeader(r.Header, "If-None-Match") {
		return nil, false
	}
	value, n, canonical := navAuthHeader(r.Header, "Authorization")
	if n != 1 || !canonical {
		return nil, false
	}
	p, err := navauth.ParseTabProof(value)
	if err != nil {
		return nil, false
	}
	target := r.URL.RequestURI()
	if r.RequestURI != "" && r.RequestURI != target {
		return nil, false
	}
	f, err := a.state.AdmitTab(r.Method, target, p)
	if err != nil {
		return nil, false
	}
	return r.WithContext(context.WithValue(r.Context(), navTabFactsKey{}, f)), true
}

// WriteNavigatorBody commits an already admitted, complete JSON response. The
// body stays caller-owned. No extra buffering, stream or second encoding occurs.
// If a credential was retired while building, no prepared private body is sent.
func WriteNavigatorBody(w http.ResponseWriter, r *http.Request, h http.Header, status int, body []byte) {
	if r != nil {
		if facts, ok := r.Context().Value(navTabFactsKey{}).(*navauth.TabFacts); ok {
			mac, err := facts.SignResponse(status, body)
			if err != nil || h.Get("Content-Type") != "application/json" {
				for _, name := range []string{"ETag", "Allow", "Retry-After", "X-Aiq-Tab-Proof"} {
					h.Del(name)
				}
				h.Set("Content-Length", "0")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			h.Set("X-Aiq-Tab-Proof", navauth.Encode32(mac))
		}
	}
	w.WriteHeader(status)
	if status != http.StatusNotModified {
		_, _ = w.Write(body)
	}
}

func navTabHasHeader(h http.Header, name string) bool {
	_, _, present := navAuthHeader(h, name)
	return present
}
