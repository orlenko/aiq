package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeTitleSkipsLargeTurnsAndPartialRecords(t *testing.T) {
	p := filepath.Join(t.TempDir(), "session.jsonl")
	data := `{"type":"custom-title","customTitle":"Harp"}` + "\n" +
		`{"type":"assistant","message":{"content":"` + strings.Repeat("x", 2<<20) + `"}}` + "\n" +
		`{"type":"custom-title","customTitle":`
	if err := os.WriteFile(p, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if got := ClaudeTitle(p); got != "Harp" {
		t.Fatalf("title = %q", got)
	}
	if got := ClaudeTitle(p + ".missing"); got != "" {
		t.Fatalf("missing title = %q", got)
	}
}
