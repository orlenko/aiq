package daemon

import (
	"bytes"
	"embed"
	"errors"
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

// Only this fixed production dependency closure is embedded/served. Browser
// fixtures, examples and tests remain outside the asset namespace.
//
//go:embed ui/app.js ui/tab_proof.js ui/read_broker.js ui/session_controller.js ui/controller_data.js ui/feed.js ui/feed_data.js ui/dom.js ui/asks_controller.js ui/asks_view.js ui/asks_data.js ui/app.css ui/session_controller.css ui/feed.css ui/dom.css ui/asks_controller.css ui/asks_view.css
var navAppFiles embed.FS

type navStaticAsset struct {
	name, mime string
	body       []byte
}

var navAdditionalAssets = func() []navStaticAsset {
	var assets []navStaticAsset
	for _, name := range []string{"app.js", "tab_proof.js", "read_broker.js", "session_controller.js", "controller_data.js", "feed.js", "feed_data.js", "dom.js", "asks_controller.js", "asks_view.js", "asks_data.js", "app.css", "session_controller.css", "feed.css", "dom.css", "asks_controller.css", "asks_view.css"} {
		body, err := navAppFiles.ReadFile("ui/" + name)
		if err != nil {
			panic("missing navigator production asset")
		}
		mime := "text/javascript; charset=utf-8"
		if strings.HasSuffix(name, ".css") {
			mime = "text/css; charset=utf-8"
		}
		assets = append(assets, navStaticAsset{name, mime, body})
	}
	return assets
}()

// NavigatorPageLimits is public code-owned admission metadata, never private
// host/directory/credential data. Callers must choose positive finite policy.
type NavigatorPageLimits struct{ BodyBytes, PlacementBytes, ArrayEntries, StringUnits, Rows int64 }

func NavigatorPagesWithLimits(l NavigatorPageLimits) (http.Handler, error) {
	for _, n := range []int64{l.BodyBytes, l.PlacementBytes, l.ArrayEntries, l.StringUnits, l.Rows} {
		if n <= 0 || n > 1<<53-1 {
			return nil, errors.New("invalid navigator page limits")
		}
	}
	return navigatorPages(&l, false), nil
}
func NavigatorUnavailablePages() http.Handler { return navigatorPages(nil, true) }

// NavigatorBuild is the exact listener/asset binding, without I/O or discovery.
func NavigatorBuild() string { return navBuild() }

var navPageDetail = regexp.MustCompile(`^/sessions/(claude|codex|agy)/[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
var navPageAnchor = regexp.MustCompile(`^[1-9][0-9]{0,12}$`)

func navPageRoute(u *url.URL) bool {
	if u == nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.RawPath != "" || u.Fragment != "" || u.Opaque != "" || u.ForceQuery ||
		(u.Path != "/" && u.Path != "/sessions" && u.Path != "/asks" && u.Path != "/help" && !navPageDetail.MatchString(u.Path)) || len(u.Path)+len(u.RawQuery)+1 > 8192 {
		return false
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil || q.Encode() != u.RawQuery {
		return false
	}
	for key, values := range q {
		if len(values) != 1 || key != "dir" && key != "all" && key != "at" {
			return false
		}
		v := values[0]
		if key == "at" && (!navPageDetail.MatchString(u.Path) || !navPageAnchor.MatchString(v)) {
			return false
		}
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
	return navigatorPages(nil, false)
}
func navigatorPages(l *NavigatorPageLimits, unavailable bool) http.Handler {
	build := navBuild()
	shell := bytes.ReplaceAll(navShellTemplate, []byte("__AIQ_NAV_BUILD__"), []byte(build))
	limits := ""
	if l != nil {
		limits = strconv.FormatInt(l.BodyBytes, 10) + ":" + strconv.FormatInt(l.PlacementBytes, 10) + ":" + strconv.FormatInt(l.ArrayEntries, 10) + ":" + strconv.FormatInt(l.StringUnits, 10) + ":" + strconv.FormatInt(l.Rows, 10)
	}
	shell = bytes.ReplaceAll(shell, []byte("__AIQ_NAV_LIMITS__"), []byte(limits))
	refusal := ""
	if unavailable {
		refusal = "1"
	}
	shell = bytes.ReplaceAll(shell, []byte("__AIQ_NAV_UNAVAILABLE__"), []byte(refusal))
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
	for _, a := range navAdditionalAssets {
		assets["/assets/"+build+"/"+a.name] = asset{bytes.Clone(a.body), a.mime}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var out asset
		status := http.StatusOK
		if navPageRoute(r.URL) {
			out = asset{shell, "text/html; charset=utf-8"}
			if unavailable {
				status = http.StatusServiceUnavailable
			}
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
		w.WriteHeader(status)
		if r.Method == "GET" {
			_, _ = w.Write(out.body)
		}
	})
}
