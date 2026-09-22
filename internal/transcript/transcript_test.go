package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func claudeFixture(cwd string) []string {
	c := `"cwd":"` + cwd + `","sessionId":"s1","entrypoint":"cli","gitBranch":"main"`
	return []string{
		`{"type":"permission-mode","permissionMode":"bypassPermissions","sessionId":"s1"}`,
		`{"type":"file-history-snapshot","snapshot":{}}`,
		`{"type":"user","isMeta":true,` + c + `,"timestamp":"2026-09-15T10:00:00Z","message":{"role":"user","content":"<local-command-caveat>Caveat</local-command-caveat>"}}`,
		// A local command with no work behind it is dropped.
		`{"type":"user",` + c + `,"timestamp":"2026-09-15T10:00:01Z","message":{"role":"user","content":"<command-name>/model</command-name>\n<command-args>opus</command-args>"}}`,
		`{"type":"user",` + c + `,"timestamp":"2026-09-15T10:00:02Z","message":{"role":"user","content":"<local-command-stdout>Set model</local-command-stdout>"}}`,
		`{"type":"user",` + c + `,"timestamp":"2026-09-15T10:01:00Z","origin":{"kind":"human"},"promptSource":"typed","message":{"role":"user","content":"fix the bug"}}`,
		`{"type":"assistant",` + c + `,"timestamp":"2026-09-15T10:01:05Z","message":{"id":"m1","model":"claude-opus-5","role":"assistant","content":[{"type":"text","text":"Looking."}]}}`,
		`{"type":"assistant",` + c + `,"timestamp":"2026-09-15T10:01:06Z","message":{"id":"m1","model":"claude-opus-5","role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]}}`,
		`{"type":"user",` + c + `,"timestamp":"2026-09-15T10:01:07Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`,
		`{"type":"assistant","isSidechain":true,` + c + `,"timestamp":"2026-09-15T10:01:08Z","message":{"id":"x","role":"assistant","content":[{"type":"text","text":"subagent chatter"}]}}`,
		`{"type":"user",` + c + `,"timestamp":"2026-09-15T10:01:30Z","origin":{"kind":"task-notification"},"promptSource":"system","message":{"role":"user","content":"Background agent \"x\" finished."}}`,
		`{"type":"assistant",` + c + `,"timestamp":"2026-09-15T10:02:00Z","message":{"id":"m2","model":"claude-opus-5","role":"assistant","content":[{"type":"text","text":"Fixed it."}]}}`,
		`{"type":"assistant",` + c + `,"timestamp":"2026-09-15T10:02:01Z","message":{"id":"m2","model":"claude-opus-5","role":"assistant","content":[{"type":"text","text":"Tests pass."}]}}`,
		`{"type":"user",` + c + `,"timestamp":"2026-09-15T10:03:00Z","message":{"role":"user","content":[{"type":"text","text":"<system-reminder>ignore</system-reminder>"},{"type":"text","text":"now push"}]}}`,
		`{"type":"user",` + c + `,"timestamp":"2026-09-15T10:03:02Z","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user]"}]}}`,
		`{"type":"user","isCompactSummary":true,` + c + `,"timestamp":"2026-09-15T10:04:00Z","message":{"role":"user","content":"This session is being continued"}}`,
		`{"type":"custom-title","customTitle":"bugfix","sessionId":"s1"}`,
		`not json`,
	}
}

func TestParseClaude(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	writeLines(t, path, claudeFixture("/work/app")...)
	s, err := ParseClaude(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != "s1" || s.Cwd != "/work/app" || s.Title != "bugfix" || !s.Bypass || s.Worker || s.Model != "claude-opus-5" || s.Branch != "main" {
		t.Fatalf("session fields: %+v", s)
	}
	if len(s.Turns) != 2 {
		t.Fatalf("want 2 turns, got %d: %+v", len(s.Turns), s.Turns)
	}
	t1, t2 := s.Turns[0], s.Turns[1]
	if t1.Prompt != "fix the bug" || t1.Reply != "Fixed it.\n\nTests pass." || t1.Tools != 1 {
		t.Fatalf("turn 1: %+v", t1)
	}
	if t2.Prompt != "now push" || t2.Reply != "" {
		t.Fatalf("turn 2: %+v", t2)
	}
	if got := t1.Ended.Sub(t1.Started).Seconds(); got != 61 {
		t.Fatalf("turn 1 lasted %vs", got)
	}
	if s.Label() != "bugfix" {
		t.Fatalf("label %q", s.Label())
	}
}

func TestParseClaudeWorkerAndCommandWithWork(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w.jsonl")
	c := `"cwd":"/w","sessionId":"w","entrypoint":"sdk-cli"`
	writeLines(t, path,
		`{"type":"user",`+c+`,"timestamp":"2026-09-15T10:00:00Z","message":{"role":"user","content":"<command-message>review</command-message>\n<command-name>/review</command-name>\n<command-args>PR 12</command-args>"}}`,
		`{"type":"assistant",`+c+`,"timestamp":"2026-09-15T10:00:05Z","message":{"id":"m","role":"assistant","content":[{"type":"text","text":"LGTM"}]}}`,
	)
	s, err := ParseClaude(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Worker || len(s.Turns) != 1 || s.Turns[0].Prompt != "/review PR 12" || s.Label() != "/review PR 12" {
		t.Fatalf("got %+v", s)
	}
}

func codexFixture(cwd, id string) []string {
	return []string{
		`{"timestamp":"2026-09-15T21:08:05Z","type":"session_meta","payload":{"id":"` + id + `","timestamp":"2026-09-15T21:08:05Z","cwd":"` + cwd + `","originator":"codex-tui","source":"cli","base_instructions":{"text":"long"},"git":{"branch":"dev"}}}`,
		`{"timestamp":"2026-09-15T21:13:20Z","type":"event_msg","payload":{"type":"task_started","turn_id":"a"}}`,
		`{"timestamp":"2026-09-15T21:13:20Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"rules"}]}}`,
		`{"timestamp":"2026-09-15T21:13:20Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions\n\n<INSTRUCTIONS>x</INSTRUCTIONS>"},{"type":"input_text","text":"<environment_context>\n<cwd>/x</cwd></environment_context>"}]}}`,
		`{"timestamp":"2026-09-15T21:13:20Z","type":"turn_context","payload":{"cwd":"` + cwd + `","approval_policy":"never","sandbox_policy":{"type":"danger-full-access"},"model":"gpt-6"}}`,
		`{"timestamp":"2026-09-15T21:13:21Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<image name=[Image #1]>"},{"type":"input_image","image_url":"x"},{"type":"input_text","text":"</image>"},{"type":"input_text","text":"write the post"}]}}`,
		`{"timestamp":"2026-09-15T21:13:24Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"Reading first."}]}}`,
		`{"timestamp":"2026-09-15T21:13:25Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec","input":"ls"}}`,
		`{"timestamp":"2026-09-15T21:13:26Z","type":"response_item","payload":{"type":"function_call","name":"spawn_agent"}}`,
		`{"timestamp":"2026-09-15T21:13:27Z","type":"response_item","payload":{"type":"custom_tool_call_output","output":"x"}}`,
		`{"timestamp":"2026-09-15T21:16:00Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Draft ready."}]}}`,
		`{"timestamp":"2026-09-15T21:16:53Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"a","last_agent_message":"Draft ready (final)."}}`,
		`{"timestamp":"2026-09-15T21:20:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"publish"}]}}`,
		`{"timestamp":"2026-09-15T21:20:05Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"Publishing now."}]}}`,
		`{"timestamp":"2026-09-15T21:20:06Z","type":"event_msg","payload":{"type":"turn_aborted"}}`,
	}
}

func TestParseCodex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-x.jsonl")
	writeLines(t, path, codexFixture("/work/blog", "c1")...)
	s, err := ParseCodex(path)
	if err != nil || s == nil {
		t.Fatal(s, err)
	}
	if s.ID != "c1" || s.Cwd != "/work/blog" || !s.Bypass || s.Worker || s.Model != "gpt-6" || s.Branch != "dev" {
		t.Fatalf("session fields: %+v", s)
	}
	if len(s.Turns) != 2 {
		t.Fatalf("want 2 turns, got %+v", s.Turns)
	}
	if t1 := s.Turns[0]; t1.Prompt != "write the post" || t1.Reply != "Draft ready (final)." || t1.Tools != 2 {
		t.Fatalf("turn 1: %+v", t1)
	}
	if d := s.Turns[0].Ended.Sub(s.Turns[0].Started).Seconds(); d != 212 {
		t.Fatalf("turn 1 lasted %vs, want 212", d)
	}
	// An aborted turn falls back to the last progress message.
	if t2 := s.Turns[1]; t2.Prompt != "publish" || t2.Reply != "Publishing now." {
		t.Fatalf("turn 2: %+v", t2)
	}
}

func TestParseCodexLegacyEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-old.jsonl")
	writeLines(t, path,
		`{"timestamp":"2025-10-01T10:00:00Z","type":"session_meta","payload":{"id":"old","cwd":"/w","source":"exec","originator":"codex_exec"}}`,
		`{"timestamp":"2025-10-01T10:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}}`,
		`{"timestamp":"2025-10-01T10:00:01Z","type":"event_msg","payload":{"type":"user_message","message":"hello"}}`,
		`{"timestamp":"2025-10-01T10:00:02Z","type":"response_item","payload":{"type":"local_shell_call"}}`,
		`{"timestamp":"2025-10-01T10:00:03Z","type":"event_msg","payload":{"type":"agent_message","message":"hi there"}}`,
	)
	s, err := ParseCodex(path)
	if err != nil || s == nil {
		t.Fatal(s, err)
	}
	if !s.Worker || len(s.Turns) != 1 || s.Turns[0].Prompt != "hello" || s.Turns[0].Reply != "hi there" || s.Turns[0].Tools != 1 {
		t.Fatalf("got %+v", s)
	}
}

func TestParseCodexSubagentSkipped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-sub.jsonl")
	writeLines(t, path,
		`{"timestamp":"2026-09-15T10:00:00Z","type":"session_meta","payload":{"id":"sub","cwd":"/w","source":{"subagent":{"thread_spawn":{"parent_thread_id":"p"}}}}}`,
		`{"timestamp":"2026-09-15T10:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"review"}]}}`,
	)
	s, err := ParseCodex(path)
	if err != nil || s != nil {
		t.Fatalf("want nil session, got %+v %v", s, err)
	}
}

func TestList(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "work", "my.app")
	other := filepath.Join(root, "work", "my-app") // encodes to the same project dir
	for _, d := range []string{work, other} {
		os.MkdirAll(d, 0o700)
	}
	work, other = canonical(work), canonical(other)
	enc := nonAlnum.ReplaceAllString(work, "-")

	claudeHome := filepath.Join(root, "claude")
	codexHome := filepath.Join(root, "codex")
	overlays := filepath.Join(root, "overlays")

	writeLines(t, filepath.Join(claudeHome, "projects", enc, "s1.jsonl"), claudeFixture(work)...)
	writeLines(t, filepath.Join(claudeHome, "projects", enc, "s2.jsonl"), strings.ReplaceAll(strings.Join(claudeFixture(other), "\n"), `"s1"`, `"s2"`))
	// Subagent transcripts one level down are not sessions of their own.
	writeLines(t, filepath.Join(claudeHome, "projects", enc, "s1", "subagents", "a.jsonl"), claudeFixture(work)...)
	// An empty session is left out.
	writeLines(t, filepath.Join(claudeHome, "projects", enc, "s3.jsonl"), `{"type":"permission-mode","permissionMode":"default"}`)
	// The same session under an overlay home that never got linked: listed once.
	writeLines(t, filepath.Join(overlays, "acct", "projects", enc, "s1.jsonl"), claudeFixture(work)[:6]...)

	writeLines(t, filepath.Join(codexHome, "sessions", "2026", "09", "15", "rollout-a.jsonl"), codexFixture(work, "c1")...)
	writeLines(t, filepath.Join(codexHome, "sessions", "2026", "09", "15", "rollout-b.jsonl"), codexFixture("/elsewhere", "c2")...)
	writeLines(t, filepath.Join(codexHome, "session_index.jsonl"),
		`{"id":"c1","thread_name":"old name"}`,
		`{"id":"c1","thread_name":"blog post"}`,
	)

	list, err := List(DefaultRoots(claudeHome, codexHome, "", overlays), work)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range list {
		got = append(got, s.Provider+"/"+s.ID+"/"+s.Label())
	}
	want := []string{"codex/c1/blog post", "claude/s1/bugfix"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}
	if len(list[1].Turns) != 2 {
		t.Fatalf("the fuller copy of s1 should win: %+v", list[1].Turns)
	}
}

func TestInjected(t *testing.T) {
	for text, want := range map[string]bool{
		"<environment_context>x":        true,
		"<system-reminder>\nx":          true,
		"</image>":                      true,
		"# AGENTS.md instructions\n":    true,
		"   ":                           true,
		"fix <b>this</b>":               false,
		"< 5 items please":              false,
		"<3 thanks":                     false,
		"why does <div> collapse here?": false,
	} {
		if got := injected(text); got != want {
			t.Errorf("injected(%q) = %v, want %v", text, got, want)
		}
	}
}
