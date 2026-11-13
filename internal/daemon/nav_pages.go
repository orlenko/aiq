package daemon

import (
	"bytes"
	_ "embed"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

//go:embed ui/auth_shell.html
var navShellTemplate []byte

//go:embed ui/auth_bootstrap.js
var navBootstrapJS []byte

//go:embed ui/auth_session.js
var navSessionJS []byte

//go:embed ui/auth_codec.js
var navCodecJS []byte

//go:embed ui/auth.css
var navAuthCSS []byte

// NavigatorBuild is the exact listener/asset binding, without I/O or discovery.
func NavigatorBuild() string { return navBuild() }

var navPageDetail = regexp.MustCompile(`^/sessions/(claude|codex|agy)/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func navPageRoute(u *url.URL) bool {
	if u == nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.RawPath != "" || u.Fragment != "" || u.Opaque != "" || u.ForceQuery ||
		(u.Path != "/" && u.Path != "/sessions" && u.Path != "/asks" && !navPageDetail.MatchString(u.Path)) || len(u.Path)+len(u.RawQuery)+1 > 8192 {
		return false
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil || q.Encode() != u.RawQuery {
		return false
	}
	for key, values := range q {
		if len(values) != 1 || key != "dir" && key != "all" {
			return false
		}
		v := values[0]
		if key == "dir" && (v == "" || len(v) > 4096 || !utf8.ValidString(v) || !path.IsAbs(v) || path.Clean(v) != v || strings.Contains(v, "\\") || strings.IndexFunc(v, func(r rune) bool { return r < 32 || r == 127 }) >= 0) {
			return false
		}
		if key == "all" && (u.Path != "/asks" || v != "true" && v != "false") {
			return false
		}
	}
	return !(q.Get("all") == "true" && q.Has("dir"))
}

// NavigatorPages returns only code-owned bytes. Mount it before Run through
// MountNavigatorPages; the listener supplies its guards, CSP and recovery.
// The API/data factory is deliberately a separate dependency.
func NavigatorPages() http.Handler {
	build := navBuild()
	shell := bytes.ReplaceAll(navShellTemplate, []byte("__AIQ_NAV_BUILD__"), []byte(build))
	type asset struct {
		body []byte
		mime string
	}
	assets := map[string]asset{}
	for _, a := range []struct {
		name, mime string
		body       []byte
	}{
		{"auth_bootstrap.js", "text/javascript; charset=utf-8", navBootstrapJS},
		{"auth_session.js", "text/javascript; charset=utf-8", navSessionJS},
		{"auth_codec.js", "text/javascript; charset=utf-8", navCodecJS},
		{"auth.css", "text/css; charset=utf-8", navAuthCSS},
	} {
		assets["/assets/"+build+"/"+a.name] = asset{bytes.Clone(a.body), a.mime}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var out asset
		if navPageRoute(r.URL) {
			out = asset{shell, "text/html; charset=utf-8"}
		} else if r.URL != nil && r.URL.RawQuery == "" && !r.URL.ForceQuery && r.URL.RawPath == "" {
			out = assets[r.URL.Path]
		}
		if out.body == nil {
			http.NotFound(w, r)
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", out.mime)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Length", strconv.Itoa(len(out.body)))
		w.WriteHeader(http.StatusOK)
		if r.Method == "GET" {
			_, _ = w.Write(out.body)
		}
	})
}
