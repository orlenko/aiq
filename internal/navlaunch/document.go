// Package navlaunch owns the private, script-free browser handoff. It never
// discovers credentials, issues codes, starts processes or prints diagnostics.
package navlaunch

import (
	"errors"
	"html"
	"net/url"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/orlenko/aiq/internal/navauth"
)

var ErrUnavailable = errors.New("navigator private browser handoff unavailable")

const marker = "<!-- aiq navigator private launch v1 -->\n"

// Document validates the complete nested fragment and HTML before returning any
// bytes. Calling it with a canonical 43-character placeholder admits size before
// consuming a one-use code. No directory text is truncated or normalized.
//
// The handoff opens Asks. An empty directory opens every project's Asks
// (/asks?all=true); any other directory must be absolute and clean, and opens
// that directory's Asks (/asks?dir=...). There is no third shape.
func Document(port int, directory, code string) ([]byte, error) {
	if port < 1 || port > 65535 {
		return nil, ErrUnavailable
	}
	next := "/asks?all=true"
	if directory != "" {
		if !utf8.ValidString(directory) || len(directory) > 4096 ||
			!path.IsAbs(directory) || path.Clean(directory) != directory || strings.Contains(directory, "\\") ||
			strings.IndexFunc(directory, func(r rune) bool { return r < 32 || r == 127 }) >= 0 {
			return nil, ErrUnavailable
		}
		next = "/asks?" + url.Values{"dir": {directory}}.Encode()
	}
	if _, err := navauth.Decode32(code); err != nil {
		return nil, ErrUnavailable
	}
	fragment := url.Values{"code": {code}, "next": {next}}.Encode()
	if len(fragment) > 8192 {
		return nil, ErrUnavailable
	}
	u := html.EscapeString("http://127.0.0.1:" + strconv.Itoa(port) + "/#" + fragment)
	doc := marker + `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="referrer" content="no-referrer"><meta http-equiv="Content-Security-Policy" content="default-src 'none'; base-uri 'none'; form-action 'none'"><meta http-equiv="refresh" content="0;url=` + u + `"><title>Open aiq navigator</title><p><a rel="noreferrer" href="` + u + `">Open aiq navigator</a></p></html>`
	if len(doc) > 65536 {
		return nil, ErrUnavailable
	}
	return []byte(doc), nil
}
