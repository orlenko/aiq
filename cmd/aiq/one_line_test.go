package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

// forged is text a directory, a file name or a transcript can carry: control
// characters, and line breaks that would start a line reading as aiq's own.
const forged = "x\x1b]0;t\x07\n\u2028\u2029\r\tLEASE 1 forged"

// checkLines fails when out does not have the given number of lines, or
// has a control character or line separator in it.
func checkLines(t *testing.T, name, out string, lines int) {
	t.Helper()
	if n := strings.Count(strings.TrimSuffix(out, "\n"), "\n") + 1; n != lines {
		t.Errorf("%s: %d lines, want %d:\n%q", name, n, lines, out)
	}
	for _, r := range out {
		if r != '\n' && (r < 0x20 || r == 0x7f || r >= 0x80 && r <= 0x9f || r == 0x2028 || r == 0x2029) {
			t.Errorf("%s: character %U in output:\n%q", name, r, out)
			return
		}
	}
}

func TestOneLine(t *testing.T) {
	for in, want := range map[string]string{
		"plain":                 "plain",
		"a\nb\tc\u2028d\u2029e": "a b c d e",
		"a\x1b[2Jb\rc\x00":      "a[2Jbc",
		"雪 'q'":                 "雪 'q'",
	} {
		if got := oneLine(in); got != want {
			t.Errorf("oneLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLongNoteIsOneLine(t *testing.T) {
	var b bytes.Buffer
	longNote(&b, "aiq: long %s session started in tmux %s (pane %s) for %s", "claude", "aiq-"+forged, "%1", "/w/"+forged)
	checkLines(t, "longNote", b.String(), 1)
	if !strings.HasSuffix(b.String(), "forged\n") {
		t.Errorf("note lost its end: %q", b.String())
	}
}

func TestLongListKeepsOneRowPerSession(t *testing.T) {
	a := renameApp(t)
	for _, ws := range []string{"/w/" + forged, "/w/plain"} {
		if _, err := a.st.AddLease(state.Lease{AccountID: "claude/one", Provider: "claude", Mode: state.ModeLong,
			Hostname: pool.Hostname(), Workspace: ws, Pane: "%1", StartedAt: time.Now().Unix()}); err != nil {
			t.Fatal(err)
		}
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	runErr := a.longList()
	os.Stdout = stdout
	w.Close()
	out, _ := io.ReadAll(r)
	if runErr != nil {
		t.Fatal(runErr)
	}
	checkLines(t, "long list", string(out), 3)
}

func TestLiveSessionsErrKeepsOneLinePerSession(t *testing.T) {
	err := liveSessionsErr("", "/w/"+forged, []convoTarget{
		{provider: "claude", path: "/p/" + forged + ".jsonl", pane: "%1" + forged},
		{provider: "codex", path: "/p/b.jsonl", account: "codex/" + forged},
	})
	checkLines(t, "live sessions", errorText(err), 3)
}

// S63-4: a line separator in the model, the id or the live note stays inside
// its row of the plain list and the --print header.
func TestPlainListKeepsOneRowPerSession(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	s := transcript.Session{Provider: "claude", ID: "ab\u2028cd" + forged, Model: "claude-x" + forged, Title: "t", Updated: at}
	checkLines(t, "sessions", renderSessionsPlain("dir"+forged, []transcript.Session{s},
		map[string]liveNote{s.ID: {note: "live" + forged}}, nil), 2)
	checkLines(t, "no sessions", renderSessionsPlain("dir"+forged, nil, nil, nil), 1)
	checkLines(t, "turns header", renderTurnsPlain(s, nil), 1)
}

func TestAttachHintNamesTheSessionOrNothing(t *testing.T) {
	for name, want := range map[string]string{
		"aiq-plain-1a2b":    "attach with: tmux attach -t aiq-plain-1a2b   (or: aiq long attach)",
		"aiq-a b-1a2b":      "attach with: aiq long attach",
		"aiq-$(x)-1a2b":     "attach with: aiq long attach",
		"aiq-a\\'; x \\'-1": "attach with: aiq long attach",
		"aiq-a\u2028b-1":    "attach with: aiq long attach",
		"aiq-\u202eb-1":     "attach with: aiq long attach",
		"aiq-a\x1b[2J-1":    "attach with: aiq long attach",
	} {
		if got := attachHint(name); got != want {
			t.Errorf("attachHint(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestResumeNoteIsOneLine(t *testing.T) {
	var b bytes.Buffer
	resumeNote(&b, "aiq: session %s is %s; attaching instead of starting a second copy\n", "s"+forged, "running in aiq long (claude/one, aiq-"+forged+")")
	checkLines(t, "resumeNote", b.String(), 1)
}

func TestConvoHeaderIsOneLine(t *testing.T) {
	s := &transcript.Session{Provider: "claude", ID: "a\u2028" + forged, Model: "claude-x" + forged, Cwd: "/w/" + forged}
	checkLines(t, "convo header", convoHeader(s, convoTarget{account: "claude/" + forged, pane: "%1" + forged}, 0, false), 1)
}

// The errors that name this folder keep it on their one line.
func TestFolderErrorsAreOneLine(t *testing.T) {
	a := renameApp(t)
	dir := filepath.Join(t.TempDir(), forged)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("TMUX_TMPDIR", t.TempDir()) // no tmux server here
	if _, err := a.longFind("."); err == nil {
		t.Error("longFind found a session")
	} else {
		checkLines(t, "long drain .", errorText(err), 1)
	}
	if err := cmdLong([]string{"attach"}); err == nil {
		t.Error("long attach found a session")
	} else {
		checkLines(t, "long attach", errorText(err), 1)
	}
	if _, err := newConvoResolver(nil, transcript.Roots{}).resolveDir(dir); err == nil {
		t.Error("convo found a live session")
	} else {
		checkLines(t, "convo", errorText(err), 1)
	}
}
