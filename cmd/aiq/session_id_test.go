package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

func TestCheckSessionID(t *testing.T) {
	for _, id := range []string{
		"5e551011-2b3c-4d5e-8f90-123456789abc", "s1", "named-thread", "019a2b3c_d.e", "A", strings.Repeat("a", 128),
	} {
		if err := checkSessionID(id); err != nil {
			t.Errorf("%q refused: %v", id, err)
		}
	}
	for _, id := range []string{
		"", "-", "-x", "--yolo", "--dangerously-bypass-approvals-and-sandbox", "-c=x",
		".hidden", "_x", "../x", "a/b", `a\b`, "a b", "a\tb", "a\nb", "a\x1b]52;c;eA==\x07", "a;b", "a=b", "aé", "a b",
		strings.Repeat("a", 129),
	} {
		if err := checkSessionID(id); err == nil {
			t.Errorf("%q accepted", id)
		}
	}
}

// Every way aiq builds a resume refuses an id the CLI would read as a flag.
func TestResumeRefusesFlagLikeSessionIDs(t *testing.T) {
	for _, id := range []string{"--dangerously-bypass-approvals-and-sandbox", "--yolo", "-c"} {
		for _, provider := range config.Providers {
			if verb, err := resumeVerb(provider, id); err == nil {
				t.Errorf("resumeVerb(%s, %q) = %q", provider, id, verb)
			}
			if args, err := resumeArgs(transcript.Session{Provider: provider, ID: id}, true, nil); err == nil {
				t.Errorf("resumeArgs(%s, %q) = %q", provider, id, args)
			}
			if args, err := longArgs(provider, "/bin/aiq", runFlags{resumeSession: id}); err == nil {
				t.Errorf("longArgs(%s, %q) = %q", provider, id, args)
			}
		}
		// aiq long resume and a takeover hand the id on as --resume-session.
		if f := parseRunFlags([]string{"--long", "--resume-session", id, "--"}); f.flagsErr == nil {
			t.Errorf("run --resume-session %q: no error", id)
		}
	}
	if f := parseRunFlags([]string{"--long", "--resume-session", "s1", "--"}); f.flagsErr != nil || f.resumeSession != "s1" {
		t.Errorf("run --resume-session s1: %+v", f)
	}
}

// A session id read from a transcript is checked when the session is picked,
// before anything is printed or started.
func TestResumePickRefusesPlantedSessionIDs(t *testing.T) {
	// A tmux that records what it was asked to start; nothing starts.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(`#!/bin/sh
case "$1" in
  has-session) exit 1 ;;
  new-session) printf '%s' "$9" > "$AIQ_TEST_OUT/command" ;;
  list-panes) printf '%%1\n' ;;
esac
`), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AIQ_TEST_OUT", bin)
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	root := t.TempDir()
	t.Setenv("AIQ_DATA_DIR", root)
	t.Setenv("AIQ_BYPASS", "")
	claudeHome, codexHome := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeHome)
	t.Setenv("CODEX_HOME", codexHome)
	work, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)
	t.Setenv("PWD", work)

	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Codex reads its id from inside the rollout.
	codexID := "c3 --dangerously-bypass-approvals-and-sandbox"
	write(filepath.Join(codexHome, "sessions", "2026", "11", "09", "rollout-2026-11-09T10-00-00-c3.jsonl"),
		`{"timestamp":"2026-11-09T10:00:00Z","type":"session_meta","payload":{"id":"`+codexID+`","cwd":"`+work+`","source":"cli"}}
{"timestamp":"2026-11-09T10:00:01Z","type":"event_msg","payload":{"type":"task_started","turn_id":"a"}}
{"timestamp":"2026-11-09T10:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the bug"}],"internal_chat_message_metadata_passthrough":{"turn_id":"a","content_item_kinds":["user.text"]}}}
{"timestamp":"2026-11-09T10:00:03Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Fixed."}]}}
{"timestamp":"2026-11-09T10:00:04Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"a","last_agent_message":"Fixed."}}
`)
	// Claude takes its id from the file name.
	claudeID := "k4;x"
	c := `"cwd":"` + work + `","sessionId":"` + claudeID + `"`
	write(filepath.Join(claudeHome, "projects", regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(work, "-"), claudeID+".jsonl"),
		`{"type":"user",`+c+`,"timestamp":"2026-11-09T10:01:00Z","origin":{"kind":"human"},"promptSource":"typed","message":{"role":"user","content":"fix the bug"}}
{"type":"assistant",`+c+`,"timestamp":"2026-11-09T10:01:05Z","message":{"id":"m1","model":"claude-opus-5","role":"assistant","content":[{"type":"text","text":"Fixed."}]}}
`)
	sessions, _, err := transcript.List(defaultRoots(), work)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, s := range sessions {
		found[s.ID] = true
	}
	if !found[codexID] || !found[claudeID] {
		t.Fatalf("planted sessions not listed: %v", found)
	}

	st, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, acc := range []state.Account{
		{ID: "claude/one", Provider: "claude", Name: "one", Enabled: true},
		{ID: "codex/one", Provider: "codex", Name: "one", Enabled: true},
	} {
		if err := st.AddAccount(acc); err != nil {
			t.Fatal(err)
		}
	}
	a := &app{cfg: config.Default(), st: st}
	for _, prefix := range []string{"c3", "k4"} {
		provider, run, err := a.runAutoResumeArgs([]string{"--bare", prefix})
		if err == nil || !strings.Contains(err.Error(), "not one aiq can resume") {
			t.Errorf("run auto resume %s: %s %q, err = %v", prefix, provider, run, err)
		}
		// long resume hands the id to a new tmux session; it refuses first.
		for name, resume := range map[string]func([]string) error{
			"long resume":      func(args []string) error { return a.longResume(args, false) },
			"long auto resume": a.longAutoResume,
		} {
			if err := resume([]string{"--bare", prefix}); err == nil || !strings.Contains(err.Error(), "not one aiq can resume") {
				t.Errorf("%s %s: err = %v", name, prefix, err)
			}
			if c, err := os.ReadFile(filepath.Join(bin, "command")); err == nil {
				t.Fatalf("%s %s reached tmux: %s", name, prefix, c)
			}
		}
	}
}
