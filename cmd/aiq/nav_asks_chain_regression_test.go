package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/orlenko/aiq/internal/diagnostic"
	"strings"
	"testing"
)

func TestNavAsksResponseChainAdmission(t *testing.T) {
	t.Run("literal bytes", func(t *testing.T) {
		s := responseRows()
		s.Items[0].Chain = strings.Repeat("x", 24)
		o := responseOptions()
		o.inputBytes = 47 // Ordinary raw23 plus24 bytes of Chain; Chain is not output.
		if b, e := buildNavAsksResponse(context.Background(), s, o); e != nil || len(b) == 0 {
			t.Fatalf("exact47 failed: %v", e)
		}
		o.inputBytes = 46
		renders, marshals := 0, 0
		r := func(s string, w int, m bool) ([]webMarkdownLine, bool) {
			renders++
			return webMarkdownLinesChecked(s, w, m)
		}
		j := func(v any) ([]byte, error) { marshals++; return json.Marshal(v) }
		b, e := buildNavAsksResponseWith(context.Background(), s, o, r, j)
		if b != nil || e != errNavAsksResponseTooLarge || renders != 0 || marshals != 0 {
			t.Fatalf("Chain one-short46 admitted: nil%v tooLarge%v renders%d marshals%d", b == nil, e == errNavAsksResponseTooLarge, renders, marshals)
		}
	})
	for _, excluded := range []bool{false, true} {
		name := "selected"
		if excluded {
			name = "excluded"
		}
		t.Run(name, func(t *testing.T) {
			s := responseRows()
			s.Items[1].Chain = "\xff"
			o := responseOptions()
			if excluded {
				o.scope = navAsksScope{"directory", "/wanted"}
				s.Items[0].Workspace = "/wanted"
				s.Items[1].Workspace = "/outside"
				o.aliases = map[string]string{"/wanted": "/wanted", "/outside": "/outside"}
			}
			renders, marshals := 0, 0
			r := func(s string, w int, m bool) ([]webMarkdownLine, bool) {
				renders++
				return webMarkdownLinesChecked(s, w, m)
			}
			j := func(v any) ([]byte, error) { marshals++; return json.Marshal(v) }
			b, e := buildNavAsksResponseWith(context.Background(), s, o, r, j)
			if b != nil || !errors.Is(e, diagnostic.Base(diagnostic.Unshowable)) || renders != 0 || marshals != 0 {
				t.Fatalf("invalid Chain admitted: nil%v unshowable%v renders%d marshals%d", b == nil, errors.Is(e, diagnostic.Base(diagnostic.Unshowable)), renders, marshals)
			}
		})
	}
}
