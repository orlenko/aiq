package daemon

import (
	"context"
	"net/http"
	"strings"

	"github.com/orlenko/aiq/internal/navauth"
)

type navTabFactsKey struct{}

// AuthorizeRead is the production-only request verifier. Generic gate fixtures
// retain their explicitly supplied legacy Credential seam, never this adapter.
func (a *navAuthAdapter) AuthorizeRead(r *http.Request) (*http.Request, bool) {
	if a == nil || a.state == nil || r == nil || r.URL == nil || r.Method != "GET" ||
		r.ContentLength != 0 || len(r.TransferEncoding) != 0 || r.Body != nil && r.Body != http.NoBody ||
		r.URL.Scheme != "" || r.URL.Host != "" || r.URL.User != nil || r.URL.Opaque != "" || r.URL.Fragment != "" {
		return nil, false
	}
	value, n, canonical := navAuthHeader(r.Header, "Authorization")
	if n != 1 || !canonical {
		return nil, false
	}
	target := r.URL.RequestURI()
	if r.RequestURI != "" && r.RequestURI != target {
		return nil, false
	}
	var f *navauth.TabFacts
	var err error
	if strings.HasPrefix(value, "AiqTab2 ") {
		condition, valid := navTabSingleHeader(r.Header, "If-None-Match")
		if !valid {
			return nil, false
		}
		p, e := navauth.ParseConditionalTabProof(value)
		if e != nil {
			return nil, false
		}
		f, err = a.state.AdmitConditionalTab(r.Method, target, condition, p)
	} else {
		if navTabHasHeader(r.Header, "If-None-Match") {
			return nil, false
		}
		p, e := navauth.ParseTabProof(value)
		if e != nil {
			return nil, false
		}
		f, err = a.state.AdmitTab(r.Method, target, p)
	}
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
			var mac [32]byte
			var err error
			mimeOK := h.Get("Content-Type") == "application/json"
			if facts.Conditional() {
				etag, valid := navTabSingleHeader(h, "ETag")
				if !valid {
					mimeOK = false
				}
				if status == http.StatusNotModified {
					h.Del("Content-Type")
					mimeOK = mimeOK && len(body) == 0
				}
				mac, err = facts.SignConditionalResponse(status, etag, body)
			} else {
				mac, err = facts.SignResponse(status, body)
			}
			if err != nil || !mimeOK {
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

func navTabSingleHeader(h http.Header, name string) (value string, valid bool) {
	valid = true
	seen := false
	canonical := http.CanonicalHeaderKey(name)
	for key, values := range h {
		if strings.EqualFold(key, name) {
			if seen || key != canonical || len(values) != 1 {
				return "", false
			}
			seen = true
			value = values[0]
		}
	}
	return value, true
}
