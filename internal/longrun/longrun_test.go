package longrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionNameAndHandoffPathAreStable(t *testing.T) {
	ws := "/home/me/code/my.project"
	a := SessionName("aiq", ws)
	b := SessionName("aiq", ws)
	if a != b || !strings.HasPrefix(a, "aiq-my-project-") {
		t.Fatalf("%s %s", a, b)
	}
	if SessionName("aiq", "/home/me/code/other") == a {
		t.Fatal("different workspaces must not collide")
	}
	if HandoffPath(ws) != HandoffPath(ws) || !strings.HasSuffix(HandoffPath(ws), ".md") {
		t.Fatal("handoff path must be deterministic")
	}
}

func TestPromptsMentionTheNote(t *testing.T) {
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	ws := "/w"
	d := DrainInstruction(ws, 3, ", resets 14:00")
	if !strings.Contains(d, HandoffPath(ws)) || !strings.Contains(d, "3% left, resets 14:00") {
		t.Fatalf("%s", d)
	}
	p := TakeoverPrompt(ws, false, "claude")
	if !strings.Contains(p, "No handoff note") || !strings.Contains(p, "from a claude session") {
		t.Fatalf("%s", p)
	}
	os.MkdirAll(filepath.Dir(HandoffPath(ws)), 0o700)
	os.WriteFile(HandoffPath(ws), []byte("# note"), 0o600)
	p = TakeoverPrompt(ws, true, "claude")
	if !strings.Contains(p, "Read the handoff note") || !strings.Contains(p, "conversation above is yours") {
		t.Fatalf("%s", p)
	}
}

func TestSplitArgs(t *testing.T) {
	got := splitArgs(`["--model","x y","-p"]`)
	if len(got) != 3 || got[1] != "x y" {
		t.Fatalf("%v", got)
	}
	if got := splitArgs("a b"); len(got) != 2 {
		t.Fatalf("fallback: %v", got)
	}
}
