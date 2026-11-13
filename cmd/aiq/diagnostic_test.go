package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/orlenko/aiq/internal/diagnostic"
	"github.com/orlenko/aiq/internal/transcript"
)

func assertPanicDiagnostic(t *testing.T, err, base error, wantFile string) diagnostic.Origin {
	t.Helper()
	if !errors.Is(err, base) || err.Error() != base.Error() {
		t.Fatal("fixed failure classification/text changed")
	}
	o := diagnostic.FromError(err)
	file, line, known := o.Location()
	if !o.Present() || !known || file != wantFile || line == 0 {
		t.Fatalf("panic leaf=%q:%d known=%v present=%v", file, line, known, o.Present())
	}
	for _, word := range []string{"invented", "private", "payload", "/home/", "/var/"} {
		if strings.Contains(err.Error(), word) {
			t.Fatal("failure text acquired panic/path contents")
		}
	}
	return o
}

func diagnosticSession() *transcript.Session {
	return wireTestSession(transcript.Turn{Prompt: "p", Reply: "answer"})
}

func diagnosticStyled(s *transcript.Session, opts convoStyledOptions, leaf func()) ([]convoStyledMessage, error) {
	return renderConvoStyledWith(s, opts, webMarkdownPalette(), func(string, int, bool) ([]webMarkdownLine, bool) { leaf(); return nil, true })
}

func TestDiagnosticFullRenderingChainLiteralFirstLeaf(t *testing.T) {
	s := diagnosticSession()
	w, b, f, err := buildNavConversationWith(s, wireTestOptions(), func(s *transcript.Session, o convoStyledOptions) ([]convoStyledMessage, error) {
		return diagnosticStyled(s, o, diagnosticLeafA)
	}, json.Marshal)
	wireTestWholeFailure(t, w, b, f, err, transcript.ErrUnshowable)
	o := assertPanicDiagnostic(t, err, transcript.ErrUnshowable, "cmd/aiq/diagnostic_leaf_test.go")
	if _, line, _ := o.Location(); line != 3 {
		t.Fatal("catcher/later origin replaced literal first operation")
	}
	if w, b, _, err := buildNavConversation(s, wireTestOptions()); err != nil || w == nil || len(b) == 0 {
		t.Fatal("fault leaked to healthy next invocation")
	}
}

func TestDiagnosticCaughtRendererThenOuterPanic(t *testing.T) {
	for _, external := range []bool{false, true} {
		first := diagnosticLeafA
		if external {
			first = func() { strings.Repeat("fixture", -1) }
		}
		w, b, f, err := buildNavConversationWith(diagnosticSession(), wireTestOptions(), func(s *transcript.Session, o convoStyledOptions) ([]convoStyledMessage, error) {
			_, inner := diagnosticStyled(s, o, first)
			if !errors.Is(inner, transcript.ErrUnshowable) {
				t.Fatal("renderer did not fail")
			}
			diagnosticLeafB()
			return nil, nil
		}, json.Marshal)
		wireTestWholeFailure(t, w, b, f, err, transcript.ErrUnshowable)
		o := diagnostic.FromError(err)
		file, line, known := o.Location()
		if !o.Present() || external && known || !external && (!known || file != "cmd/aiq/diagnostic_leaf_test.go" || line != 3) {
			t.Fatal("later outer B replaced first renderer origin")
		}
		if err.Error() != transcript.ErrUnshowable.Error() {
			t.Fatal("error text changed")
		}
	}
}

func TestDiagnosticTrustedRepanicAcrossWire(t *testing.T) {
	_, first := diagnosticStyled(diagnosticSession(), styledTestOptions(), diagnosticLeafA)
	w, b, f, err := buildNavConversationWith(diagnosticSession(), wireTestOptions(), func(*transcript.Session, convoStyledOptions) ([]convoStyledMessage, error) { panic(first) }, json.Marshal)
	wireTestWholeFailure(t, w, b, f, err, transcript.ErrUnshowable)
	if diagnostic.FromError(err) != diagnostic.FromError(first) {
		t.Fatal("trusted re-panic replaced first operation")
	}
	assertPanicDiagnostic(t, err, transcript.ErrUnshowable, "cmd/aiq/diagnostic_leaf_test.go")
}

func TestDiagnosticEncodingLeafAndEarlierLatch(t *testing.T) {
	for _, earlier := range []bool{false, true} {
		render := navConversationRender(renderConvoStyled)
		if earlier {
			render = func(s *transcript.Session, o convoStyledOptions) ([]convoStyledMessage, error) {
				_, err := diagnosticStyled(s, o, diagnosticLeafA)
				if !errors.Is(err, transcript.ErrUnshowable) {
					t.Fatal("first renderer failure missing")
				}
				return renderConvoStyled(s, o)
			}
		}
		w, b, f, err := buildNavConversationWith(diagnosticSession(), wireTestOptions(), render, func(any) ([]byte, error) { diagnosticLeafB(); return nil, nil })
		wireTestWholeFailure(t, w, b, f, err, transcript.ErrUnshowable)
		o := assertPanicDiagnostic(t, err, transcript.ErrUnshowable, "cmd/aiq/diagnostic_leaf_test.go")
		want := uint32(5)
		if earlier {
			want = 3
		}
		if _, line, _ := o.Location(); line != want {
			t.Fatal("encoding guard lost original leaf or earlier latch")
		}
	}
}

func TestDiagnosticConcurrentCallsKeepDistinctOrigins(t *testing.T) {
	start := make(chan struct{})
	result := make(chan error, 2)
	for _, leaf := range []func(){diagnosticLeafA, diagnosticLeafB} {
		go func(leaf func()) {
			<-start
			_, _, _, err := buildNavConversationWith(diagnosticSession(), wireTestOptions(), func(s *transcript.Session, o convoStyledOptions) ([]convoStyledMessage, error) {
				return diagnosticStyled(s, o, leaf)
			}, json.Marshal)
			result <- err
		}(leaf)
	}
	close(start)
	lines := map[uint32]bool{}
	for range 2 {
		o := assertPanicDiagnostic(t, <-result, transcript.ErrUnshowable, "cmd/aiq/diagnostic_leaf_test.go")
		_, line, _ := o.Location()
		lines[line] = true
	}
	if len(lines) != 2 || !lines[3] || !lines[5] {
		t.Fatal("parallel invocations shared their first origin")
	}
}

func TestDiagnosticOrdinaryRefusalsHaveNoOrigin(t *testing.T) {
	if _, err := renderConvoProjection(nil, convoProjectionOptions{}); err != errConvoProjectionSession || diagnostic.FromError(err).Present() {
		t.Fatal("ordinary projection refusal changed")
	}
	s := diagnosticSession()
	o := wireTestOptions()
	o.bodyBytes = 1
	w, b, f, err := buildNavConversation(s, o)
	wireTestWholeFailure(t, w, b, f, err, errNavConversationTooLarge)
	if err != errNavConversationTooLarge || diagnostic.FromError(err).Present() {
		t.Fatal("budget refusal gained a panic")
	}
	w, b, f, err = buildNavConversationWith(s, wireTestOptions(), renderConvoStyled, func(any) ([]byte, error) { return nil, errors.New("invented ordinary cause") })
	wireTestWholeFailure(t, w, b, f, err, transcript.ErrUnshowable)
	if err != transcript.ErrUnshowable || diagnostic.FromError(err).Present() {
		t.Fatal("ordinary marshal refusal changed identity")
	}
}
