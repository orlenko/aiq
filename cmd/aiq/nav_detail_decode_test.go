package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const detailTestID = "00000001-0000-4000-8000-000000000001"
const detailTestPath = "/api/nav/sessions/claude/" + detailTestID

func detailTestRequest() *http.Request { return httptest.NewRequest("GET", detailTestPath, nil) }

func TestNavDetailWholeConditionalGrammar(t *testing.T) {
	fingerprint := strings.Repeat("a", 64)
	tag := `"` + fingerprint + `"`
	for _, tc := range []struct {
		headers []string
		match   bool
	}{
		{nil, false}, {[]string{tag}, true}, {[]string{"W/" + tag}, true}, {[]string{"*"}, true}, {[]string{`"other", W/` + tag}, true}, {[]string{`"other"`, tag}, true},
		{[]string{`"opaque,comma", ` + tag}, true}, {[]string{`"\", ` + tag}, true}, {[]string{`"other"`}, false},
		{[]string{tag + `, invalid`}, false}, {[]string{tag + ` suffix`}, false}, {[]string{tag, `invalid`}, false}, {[]string{`*, ` + tag}, false}, {[]string{`W/ ` + tag}, false}, {[]string{`w/` + tag}, false}, {[]string{`,` + tag}, true}, {[]string{tag + `,`}, true}, {[]string{`"unterminated`}, false}, {[]string{"\"control\x01\", " + tag}, false}, {[]string{tag, `*`}, false},
	} {
		condition, err := navDetailConditions(tc.headers)
		if err != nil || condition.match(fingerprint) != tc.match {
			t.Errorf("condition %q: match=%v err=%v want %v", tc.headers, condition.match(fingerprint), err, tc.match)
		}
	}
	for _, headers := range [][]string{{strings.Repeat("x", 8192)}, {strings.Repeat("x", 8191), ""}} {
		if _, err := navDetailConditions(headers); err != nil {
			t.Fatal("aggregate boundary rejected")
		}
	}
	for _, headers := range [][]string{{strings.Repeat("x", 8193)}, {strings.Repeat("x", 8192), ""}} {
		if _, err := navDetailConditions(headers); err == nil {
			t.Fatal("aggregate condition bound bypassed")
		}
	}
}

func TestNavDetailDecoderLiteralContract(t *testing.T) {
	for _, tc := range []struct {
		query  string
		last   int
		markup bool
		anchor int64
	}{{"", 50, true, 0}, {"last=00001&markup=0&at=0000000000001", 1, false, 1}, {"last=10000&markup=1&at=9999999999999", 10000, true, 9999999999999}, {"%6cast=%35%30", 50, true, 0}} {
		r := detailTestRequest()
		r.URL.RawQuery = tc.query
		r.RequestURI = r.URL.RequestURI()
		q, _, ok := navDetailDecode(r)
		if !ok || q.provider != "claude" || q.id != detailTestID || q.last != tc.last || q.markup != tc.markup || q.anchor != tc.anchor {
			t.Fatalf("literal query %q => %+v ok=%v", tc.query, q, ok)
		}
	}
	for _, query := range []string{"last=0", "last=10001", "last=000001", "last=-1", "last=+1", "last=１", "markup=01", "markup=", "at=0", "at=10000000000000", "at=-1", "at=+1", "last=1&last=2", "last=1&%6cast=2", "from_key=x", "from_turn=1", "extra=x", "last=%GG", "last=1;markup=0", strings.Repeat("x", 4097)} {
		r := detailTestRequest()
		r.URL.RawQuery = query
		r.RequestURI = r.URL.RequestURI()
		if _, _, ok := navDetailDecode(r); ok {
			t.Errorf("invalid query accepted %q", query)
		}
	}
	for _, change := range []func(*http.Request){func(r *http.Request) { r.URL.RawPath = detailTestPath }, func(r *http.Request) { r.URL.Fragment = "fragment" }, func(r *http.Request) { r.URL.Opaque = "opaque" }, func(r *http.Request) { r.URL.ForceQuery = true; r.RequestURI = detailTestPath + "?" }, func(r *http.Request) { r.RequestURI = detailTestPath + "?last=1" }, func(r *http.Request) { r.URL.RawQuery = "last=1" }, func(r *http.Request) { r.RequestURI = "/api/nav/sessions/%63laude/" + detailTestID }} {
		r := detailTestRequest()
		change(r)
		if _, _, ok := navDetailDecode(r); ok {
			t.Fatal("malformed URL/URI pair accepted")
		}
	}
	for _, path := range []string{"/api/nav/sessions/claude/short", detailTestPath + "/state", detailTestPath + "/", "/api/nav/sessions//" + detailTestID, "/api/nav/sessions/unknown/" + detailTestID, "/api/nav/sessions/./claude/" + detailTestID} {
		r := detailTestRequest()
		r.URL.Path = path
		r.RequestURI = path
		if _, _, ok := navDetailDecode(r); ok {
			t.Fatal("invalid path accepted")
		}
	}
}
