package codex

import (
	"strings"
	"testing"
)

func TestTailWriterLastLine(t *testing.T) {
	w := &tailWriter{max: 64}
	if w.lastLine() != "" {
		t.Fatal("nothing written: no note")
	}
	w.Write([]byte("\x1b[2m2026-09-24T23:33:26Z\x1b[0m warming up\n"))
	w.Write([]byte("aiq: codex not found on PATH\n\n"))
	if got := w.lastLine(); got != "aiq: codex not found on PATH" {
		t.Fatalf("got %q", got)
	}

	// Only the tail is kept, and a long line is cut.
	w = &tailWriter{max: 16}
	w.Write([]byte("first line that is long enough to fall off\nlast\n"))
	if got := w.lastLine(); got != "last" {
		t.Fatalf("got %q", got)
	}
	w = &tailWriter{max: 4096}
	w.Write([]byte(strings.Repeat("x", 400)))
	if got := w.lastLine(); len(got) != 300+len("…") {
		t.Fatalf("long line not cut: %d", len(got))
	}
}
