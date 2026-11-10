package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// checkPlaceholder holds a parse of two turns whose second prompt was over the
// cap: the first keeps its own answer, and the second is a turn of its own
// with no prompt, the record counted once, and the answer and tools after it.
func checkPlaceholder(t *testing.T, s *Session, first, second string, tools int) {
	t.Helper()
	if s == nil || len(s.Turns) != 2 {
		t.Fatalf("turns: %+v", s)
	}
	t0, t1 := s.Turns[0], s.Turns[1]
	if t0.Reply != first || len(t0.Earlier) != 0 || t0.Skipped != 0 || t0.PromptSkipped {
		t.Errorf("turn 0: %+v", t0)
	}
	if !t1.PromptSkipped || t1.Prompt != "" || t1.Source != Human || t1.Skipped != 1 || t1.Reply != second || t1.Tools != tools {
		t.Errorf("turn 1: %+v", t1)
	}
	if t1.Started.IsZero() || t1.Ended.Before(t1.Started) {
		t.Errorf("turn 1 runs %v to %v", t1.Started, t1.Ended)
	}
	if s.Skipped != 0 {
		t.Errorf("skipped %d before the turns", s.Skipped)
	}
}

func TestClaudeOverCapPromptOpensATurn(t *testing.T) {
	withMaxLine(t, 100<<10)
	c := `"cwd":"/w","sessionId":"s1"`
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	writeLines(t, path,
		`{"type":"user",`+c+`,"timestamp":"2026-11-02T10:00:00Z","origin":{"kind":"human"},"promptSource":"typed","message":{"role":"user","content":"first question"}}`,
		`{"type":"assistant",`+c+`,"timestamp":"2026-11-02T10:00:05Z","message":{"id":"m1","model":"claude-opus-5","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"First answer."}]}}`,
		// The fields after the message are past the opening, the time among them.
		`{"type":"user",`+c+`,"message":{"role":"user","content":"log: `+strings.Repeat("z", 200<<10)+`"},"timestamp":"2026-11-02T10:01:00Z","origin":{"kind":"human"},"promptSource":"typed"}`,
		`{"type":"assistant",`+c+`,"timestamp":"2026-11-02T10:01:05Z","message":{"id":"m2","model":"claude-opus-5","role":"assistant","stop_reason":"tool_use","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]}}`,
		`{"type":"user",`+c+`,"timestamp":"2026-11-02T10:01:06Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`,
		`{"type":"assistant",`+c+`,"timestamp":"2026-11-02T10:01:09Z","message":{"id":"m3","model":"claude-opus-5","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"Your log is fine."}]}}`,
	)
	s, err := ParseClaude(path)
	if err != nil {
		t.Fatal(err)
	}
	checkPlaceholder(t, s, "First answer.", "Your log is fine.", 1)
	if got := s.Turns[1].Started.Format("15:04:05"); got != "10:01:05" {
		t.Errorf("turn 1 starts at %s, the first time it was given", got)
	}
}

// A prompt that shows its own time in its opening starts its turn then; one
// typed while the agent works is a steer, counted on the turn it joins.
func TestClaudeOverCapQueuedPromptIsASteer(t *testing.T) {
	withMaxLine(t, 100<<10)
	c := `"cwd":"/w","sessionId":"s1"`
	big := strings.Repeat("z", 200<<10)
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	writeLines(t, path,
		`{"type":"user",`+c+`,"timestamp":"2026-11-02T10:00:00Z","origin":{"kind":"human"},"promptSource":"typed","message":{"role":"user","content":"`+big+`"}}`,
		`{"type":"assistant",`+c+`,"timestamp":"2026-11-02T10:00:05Z","message":{"id":"m1","role":"assistant","stop_reason":"tool_use","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]}}`,
		`{"type":"attachment",`+c+`,"timestamp":"2026-11-02T10:00:06Z","attachment":{"type":"queued_command","commandMode":"prompt","origin":{"kind":"human"},"prompt":"`+big+`"}}`,
		`{"type":"assistant",`+c+`,"timestamp":"2026-11-02T10:00:09Z","message":{"id":"m2","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"Done."}]}}`,
	)
	s, err := ParseClaude(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Turns) != 1 || !s.Turns[0].PromptSkipped || s.Turns[0].Skipped != 2 || s.Turns[0].Reply != "Done." {
		t.Fatalf("turns %+v", s.Turns)
	}
	if got := s.Turns[0].Started.Format("15:04:05"); got != "10:00:00" {
		t.Errorf("starts at %s", got)
	}
}

func TestCodexOverCapPromptOpensATurn(t *testing.T) {
	withMaxLine(t, 100<<10)
	mk := func(p string) []string {
		return []string{
			`{"timestamp":"2026-09-15T21:08:05Z","type":"session_meta","payload":{"id":"c1","cwd":"/w","source":"cli"}}`,
			`{"timestamp":"2026-09-15T21:13:20Z","type":"event_msg","payload":{"type":"task_started","turn_id":"a"}}`,
			`{"timestamp":"2026-09-15T21:13:21Z","type":"event_msg","payload":{"type":"user_message","message":"first question","turn_id":"a"}}`,
			`{"timestamp":"2026-09-15T21:13:22Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"first answer"}]}}`,
			`{"timestamp":"2026-09-15T21:13:23Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"a","last_agent_message":"first answer"}}`,
			`{"timestamp":"2026-09-15T21:14:20Z","type":"event_msg","payload":{"type":"task_started","turn_id":"b"}}`,
			`{"timestamp":"2026-09-15T21:14:21Z","type":"event_msg","payload":{"type":"user_message","message":"` + p + `","turn_id":"b"}}`,
			`{"timestamp":"2026-09-15T21:14:21Z","type":"response_item","payload":{"type":"function_call","name":"exec"}}`,
			`{"timestamp":"2026-09-15T21:14:22Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"second answer"}]}}`,
			`{"timestamp":"2026-09-15T21:14:23Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"b","last_agent_message":"second answer"}}`,
		}
	}
	path := filepath.Join(t.TempDir(), "rollout-x.jsonl")
	writeLines(t, path, mk(strings.Repeat("x", 200<<10))...)
	s, err := ParseCodex(path)
	if err != nil {
		t.Fatal(err)
	}
	checkPlaceholder(t, s, "first answer", "second answer", 1)
	if got := s.Turns[1].Started.Format("15:04:05"); got != "21:14:21" {
		t.Errorf("turn 1 starts at %s, the time in its opening", got)
	}
}

// A CLI that logs user_message events writes a prompt twice; the parser reads
// the event, so an over-cap prompt counts once.
func TestCodexOverCapPromptCountsOnce(t *testing.T) {
	withMaxLine(t, 100<<10)
	big := strings.Repeat("z", 200<<10)
	path := filepath.Join(t.TempDir(), "rollout-x.jsonl")
	writeLines(t, path,
		`{"timestamp":"2026-09-15T21:08:05Z","type":"session_meta","payload":{"id":"c1","cwd":"/w","source":"cli"}}`,
		`{"timestamp":"2026-09-15T21:13:20Z","type":"event_msg","payload":{"type":"task_started","turn_id":"a"}}`,
		`{"timestamp":"2026-09-15T21:13:21Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"first ask"}]}}`,
		`{"timestamp":"2026-09-15T21:13:21Z","type":"event_msg","payload":{"type":"user_message","message":"first ask","turn_id":"a"}}`,
		`{"timestamp":"2026-09-15T21:13:24Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"First done."}]}}`,
		`{"timestamp":"2026-09-15T21:13:25Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"a","last_agent_message":"First done."}}`,
		`{"timestamp":"2026-09-15T21:14:20Z","type":"event_msg","payload":{"type":"task_started","turn_id":"b"}}`,
		`{"timestamp":"2026-09-15T21:14:21Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"`+big+`"}]}}`,
		`{"timestamp":"2026-09-15T21:14:21Z","type":"event_msg","payload":{"type":"user_message","message":"`+big+`","turn_id":"b"}}`,
		`{"timestamp":"2026-09-15T21:14:22Z","type":"response_item","payload":{"type":"function_call","name":"exec"}}`,
		`{"timestamp":"2026-09-15T21:14:24Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Second done."}]}}`,
		`{"timestamp":"2026-09-15T21:14:25Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"b","last_agent_message":"Second done."}}`,
	)
	s, err := ParseCodex(path)
	if err != nil {
		t.Fatal(err)
	}
	checkPlaceholder(t, s, "First done.", "Second done.", 1)
}

func TestAgyOverCapPromptOpensATurn(t *testing.T) {
	withMaxLine(t, 100<<10)
	path := AgyTranscript(t.TempDir(), "a1")
	writeLines(t, path,
		`{"type":"USER_INPUT","created_at":"2026-11-05T12:00:00Z","content":"<USER_REQUEST>\nfirst ask\n</USER_REQUEST>"}`,
		`{"type":"PLANNER_RESPONSE","created_at":"2026-11-05T12:00:10Z","content":"First done."}`,
		`{"type":"USER_INPUT","created_at":"2026-11-05T12:01:00Z","content":"<USER_REQUEST>\nsecond ask `+strings.Repeat("z", 200<<10)+`\n</USER_REQUEST>"}`,
		`{"type":"PLANNER_RESPONSE","created_at":"2026-11-05T12:01:10Z","content":"Second done.","tool_calls":[{}]}`,
	)
	s, err := ParseAgy(path)
	if err != nil {
		t.Fatal(err)
	}
	checkPlaceholder(t, s, "First done.", "Second done.", 1)
	if got := s.Turns[1].Started.Format("15:04:05"); got != "12:01:00" {
		t.Errorf("turn 1 starts at %s, the time in its opening", got)
	}
}

// A session whose only prompt is over the cap is still listed, under the
// skipped note; one whose only record outside a turn was skipped is too.
func TestListKeepsASessionOfSkippedRecords(t *testing.T) {
	withMaxLine(t, 100<<10)
	root := t.TempDir()
	work := canonical(t.TempDir())
	project := filepath.Join(root, "projects", nonAlnum.ReplaceAllString(work, "-"))
	c := `"cwd":"` + work + `","sessionId":"5e551011-0000-4000-8000-0000000000ab"`
	big := strings.Repeat("z", 200<<10)
	writeLines(t, filepath.Join(project, "5e551011-0000-4000-8000-0000000000ab.jsonl"),
		`{"type":"user",`+c+`,"timestamp":"2026-11-02T10:01:00Z","origin":{"kind":"human"},"promptSource":"typed","message":{"role":"user","content":"dump: `+big+`"}}`,
		`{"type":"assistant",`+c+`,"timestamp":"2026-11-02T10:01:05Z","message":{"id":"m2","model":"claude-opus-5","role":"assistant","content":[{"type":"text","text":"The dump looks fine."}]}}`,
	)
	writeLines(t, filepath.Join(project, "5e551011-0000-4000-8000-0000000000ac.jsonl"),
		`{"type":"user","isMeta":true,"cwd":"`+work+`","timestamp":"2026-11-02T09:00:00Z","message":{"role":"user","content":"`+big+`"}}`,
	)
	list, unreadable, err := List(Roots{ClaudeProjects: []string{filepath.Join(root, "projects")}}, work)
	if err != nil || len(unreadable) != 0 {
		t.Fatal(err, unreadable)
	}
	if len(list) != 2 {
		t.Fatalf("%d sessions listed, want 2", len(list))
	}
	for _, s := range list {
		if s.Label() != "a record over 0 MB was skipped" {
			t.Errorf("%s labelled %q", s.ID, s.Label())
		}
		if row := Summarize(&s); row.Empty || row.Label != s.Label() {
			t.Errorf("%s: row %+v", s.ID, row)
		}
	}
	for _, s := range list {
		if strings.HasSuffix(s.ID, "ab") && (len(s.Turns) != 1 || s.Turns[0].Reply != "The dump looks fine.") {
			t.Errorf("the session whose prompt was skipped: %+v", s.Turns)
		}
	}
}

// An over-cap prompt whose turn_id lies past its opening still belongs to the
// task under way, so a steer to that task joins its turn.
func TestCodexOverCapPromptKeepsItsTask(t *testing.T) {
	withMaxLine(t, 100<<10)
	big := strings.Repeat("z", 200<<10)
	path := filepath.Join(t.TempDir(), "rollout-x.jsonl")
	writeLines(t, path,
		`{"timestamp":"2026-09-15T21:08:05Z","type":"session_meta","payload":{"id":"c1","cwd":"/w","source":"cli"}}`,
		`{"timestamp":"2026-09-15T21:14:20Z","type":"event_msg","payload":{"type":"task_started","turn_id":"b"}}`,
		`{"timestamp":"2026-09-15T21:14:21Z","type":"event_msg","payload":{"type":"user_message","message":"`+big+`","turn_id":"b"}}`,
		`{"timestamp":"2026-09-15T21:14:22Z","type":"response_item","payload":{"type":"function_call","name":"exec"}}`,
		`{"timestamp":"2026-09-15T21:14:23Z","type":"event_msg","payload":{"type":"user_message","message":"also check x","turn_id":"b"}}`,
		`{"timestamp":"2026-09-15T21:14:24Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Done."}]}}`,
		`{"timestamp":"2026-09-15T21:14:25Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"b","last_agent_message":"Done."}}`,
	)
	s, err := ParseCodex(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Turns) != 1 {
		t.Fatalf("turns: %+v", s.Turns)
	}
	if t0 := s.Turns[0]; !t0.PromptSkipped || len(t0.Steers) != 1 || t0.Steers[0] != "also check x" || t0.Reply != "Done." || t0.Tools != 1 {
		t.Errorf("turn: %+v", t0)
	}
}

// A step is a prompt by its own type, not by a tool call's args that hold the
// same key.
func TestAgyOverCapStepIsAPromptByItsOwnType(t *testing.T) {
	withMaxLine(t, 4<<10)
	path := AgyTranscript(t.TempDir(), "a1")
	writeLines(t, path,
		`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","created_at":"2026-09-04T17:57:50Z","content":"<USER_REQUEST>\nfix the build\n</USER_REQUEST>"}`,
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","created_at":"2026-09-04T17:58:00Z","content":"","tool_calls":[{"name":"x","args":{"type":"USER_INPUT","blob":"`+strings.Repeat("y", 8<<10)+`"}}]}`,
		`{"step_index":2,"source":"MODEL","type":"PLANNER_RESPONSE","created_at":"2026-09-04T17:59:00Z","content":"Fixed it."}`,
	)
	s, err := ParseAgy(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Turns) != 1 || s.Turns[0].Prompt != "fix the build" || s.Turns[0].Reply != "Fixed it." || s.Turns[0].Skipped != 1 {
		t.Errorf("turns: %+v", s.Turns)
	}
}

// A Claude record's class comes from its own type and its attachment's,
// whichever order they come in, and not from keys a payload holds.
func TestClaudeOverCapClassByItsOwnType(t *testing.T) {
	pad := strings.Repeat("x", 512)
	c := `"isSidechain":false,"cwd":"/w","sessionId":"s"`
	for _, tc := range []struct {
		name, opening string
		want          int
	}{
		{"prompt", `{` + c + `,"type":"user","message":{"role":"user","content":"` + pad, overPrompt},
		{"tool result holding the queued marker", `{` + c + `,"type":"user","message":{"role":"user","content":[{"tool_use_id":"t1","type":"tool_result","content":"ok"}]},"toolUseResult":{"structuredContent":{"type":"queued_command","pad":"` + pad, overOther},
		{"assistant holding the user marker", `{` + c + `,"type":"assistant","message":{"content":[{"type":"tool_use","input":{"type":"user","q":"` + pad, overOther},
		{"queued, type first", `{"type":"attachment",` + c + `,"attachment":{"type":"queued_command","prompt":"` + pad, overQueued},
		{"queued, attachment first", `{` + c + `,"attachment":{"type":"queued_command","prompt":"` + pad, overQueued},
		{"queued, attachment before type", `{` + c + `,"attachment":{"type":"queued_command","prompt":"x"},"type":"attachment","uuid":"` + pad, overQueued},
		{"other attachment holding the marker", `{"type":"attachment",` + c + `,"attachment":{"type":"file","content":{"type":"queued_command","pad":"` + pad, overOther},
	} {
		if got := claudeOver([]byte(tc.opening)); got != tc.want {
			t.Errorf("%s: class %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestOwnString(t *testing.T) {
	for _, tc := range []struct{ opening, key, want string }{
		{`{"a":{"type":"inner"},"type":"outer"}`, "type", "outer"},
		{`{"a":[{"type":"inner"}],"type":"outer"}`, "type", "outer"},
		{`{"a":"say \"type\":\"no\"","type":"yes"}`, "type", "yes"},
		{`{"a":{"type":"inner"`, "type", ""},
		{`{"type" : "spaced"}`, "type", "spaced"},
		{`{"n":1}`, "type", ""},
		{`{"a":{"b":1}}{"type":"after"}`, "type", ""},
	} {
		if got := ownString([]byte(tc.opening), tc.key); got != tc.want {
			t.Errorf("ownString(%s, %q) = %q, want %q", tc.opening, tc.key, got, tc.want)
		}
	}
}

// A session whose only record is over the cap takes its cwd from that record,
// as from a whole one, so it lists under its own directory and not under a
// sibling whose name encodes the same.
func TestAnOverCapRecordGivesItsCwd(t *testing.T) {
	withMaxLine(t, 4<<10)
	for _, tc := range []struct{ name, content string }{
		{"prompt", `"` + strings.Repeat("z", 8<<10) + `"`},
		{"tool result", `[{"tool_use_id":"t1","type":"tool_result","content":"` + strings.Repeat("z", 8<<10) + `"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			a, b := filepath.Join(base, "my.app"), filepath.Join(base, "my-app")
			for _, d := range []string{a, b} {
				if err := os.MkdirAll(d, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			a, b = canonical(a), canonical(b)
			enc := nonAlnum.ReplaceAllString(a, "-")
			if enc != nonAlnum.ReplaceAllString(b, "-") {
				t.Fatal("the two directories encode differently")
			}
			proj := filepath.Join(base, "projects")
			path := filepath.Join(proj, enc, "5e551011-0000-4000-8000-00000000000b.jsonl")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			writeLines(t, path, `{"parentUuid":null,"isSidechain":false,"cwd":"`+a+`","sessionId":"5e551011-0000-4000-8000-00000000000b","type":"user","message":{"role":"user","content":`+tc.content+`},"timestamp":"2026-09-15T10:00:00Z"}`)
			r := Roots{ClaudeProjects: []string{proj}}
			mine, _, _ := List(r, a)
			other, _, _ := List(r, b)
			if len(mine) != 1 || mine[0].Cwd != a || len(other) != 0 {
				t.Errorf("listed under its own directory: %+v; under the sibling: %+v", mine, other)
			}
		})
	}
}

// A queued command over the cap that the opening shows is a task
// notification is not a prompt: the agent's answer stays on the turn it
// continues.
func TestAnOverCapTaskNotificationIsNoPrompt(t *testing.T) {
	withMaxLine(t, 4<<10)
	c := `"cwd":"/w","sessionId":"s1"`
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	writeLines(t, path,
		`{"type":"user",`+c+`,"timestamp":"2026-09-15T10:00:00Z","origin":{"kind":"human"},"promptSource":"typed","message":{"role":"user","content":"start the job"}}`,
		`{"type":"assistant",`+c+`,"timestamp":"2026-09-15T10:00:05Z","message":{"id":"m1","model":"claude-x","stop_reason":"end_turn","content":[{"type":"text","text":"Started."}]}}`,
		`{"type":"attachment",`+c+`,"timestamp":"2026-09-15T10:05:00Z","attachment":{"type":"queued_command","commandMode":"task-notification","origin":{"kind":"task-notification"},"prompt":"<task-notification>`+strings.Repeat("n", 8<<10)+`</task-notification>"}}`,
		`{"type":"assistant",`+c+`,"timestamp":"2026-09-15T10:05:05Z","message":{"id":"m2","model":"claude-x","stop_reason":"end_turn","content":[{"type":"text","text":"The job finished."}]}}`)
	s, err := ParseClaude(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Turns) != 1 || s.Turns[0].Prompt != "start the job" || s.Turns[0].Reply != "The job finished." || s.Turns[0].PromptSkipped {
		t.Fatalf("turns: %+v", s.Turns)
	}
	if s.Turns[0].Skipped+s.Skipped != 1 {
		t.Errorf("skipped: turn %d, session %d", s.Turns[0].Skipped, s.Skipped)
	}
	for _, opening := range []string{
		`{"type":"attachment",` + c + `,"attachment":{"type":"queued_command","commandMode":"prompt","origin":{"kind":"human"},"prompt":"x`,
		`{"type":"attachment",` + c + `,"attachment":{"type":"queued_command","prompt":"x`,
	} {
		if got := claudeOver([]byte(opening)); got != overQueued {
			t.Errorf("a typed command reads as %d: %s", got, opening)
		}
	}
	for _, opening := range []string{
		`{"type":"attachment",` + c + `,"attachment":{"type":"queued_command","origin":{"kind":"task-notification"},"prompt":"x`,
		`{"type":"attachment",` + c + `,"attachment":{"type":"queued_command","commandMode":"task-notification","prompt":"x`,
		`{"type":"attachment","isSidechain":true,` + c + `,"attachment":{"type":"queued_command","prompt":"x`,
		`{"type":"attachment","isMeta":true,` + c + `,"attachment":{"type":"queued_command","prompt":"x`,
		`{"type":"attachment",` + c + `,"attachment":{"type":"queued_command","isMeta":true,"prompt":"x`,
	} {
		if got := claudeOver([]byte(opening)); got != overOther {
			t.Errorf("not the person's, reads as %d: %s", got, opening)
		}
	}
}

// An over-cap prompt's time, where its opening shows one, is the session's
// latest too, as a whole record's would be.
func TestAnOverCapPromptUpdatesTheSession(t *testing.T) {
	withMaxLine(t, 4<<10)
	c := `"cwd":"/w","sessionId":"s1"`
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	writeLines(t, path,
		`{"type":"user",`+c+`,"timestamp":"2026-09-15T10:00:00Z","message":{"role":"user","content":"first ask"}}`,
		`{"type":"assistant",`+c+`,"timestamp":"2026-09-15T10:00:05Z","message":{"id":"m1","stop_reason":"end_turn","content":[{"type":"text","text":"First done."}]}}`,
		`{"type":"user",`+c+`,"timestamp":"2026-09-15T11:00:00Z","message":{"role":"user","content":"`+strings.Repeat("z", 8<<10)+`"}}`)
	s, err := ParseClaude(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Updated.Format("15:04:05"); got != "11:00:00" {
		t.Errorf("updated at %s, want the over-cap prompt's 11:00:00", got)
	}
}

// Context the CLI injects into a Codex session is no prompt when it is over
// the cap either: the session reads as it does with the same context short.
// A prompt over the cap that is no such context still opens its turn, also
// when it starts with white space, and so does an older CLI's user response
// item, whose first part does not decide whether it was typed.
func TestCodexOverCapInjectedContextIsNoPrompt(t *testing.T) {
	withMaxLine(t, 100<<10)
	head := []string{
		`{"timestamp":"2026-09-15T21:08:05Z","type":"session_meta","payload":{"id":"c1","cwd":"/w","source":"cli"}}`,
		`{"timestamp":"2026-09-15T21:13:20Z","type":"event_msg","payload":{"type":"task_started","turn_id":"a"}}`,
		`{"timestamp":"2026-09-15T21:13:21Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"first ask"}]}}`,
	}
	firstEvent := `{"timestamp":"2026-09-15T21:13:21Z","type":"event_msg","payload":{"type":"user_message","message":"first ask","turn_id":"a"}}`
	answer := []string{
		`{"timestamp":"2026-09-15T21:13:24Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"First done."}]}}`,
		`{"timestamp":"2026-09-15T21:13:25Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"a","last_agent_message":"First done."}}`,
	}
	event := func(text string) string {
		return `{"timestamp":"2026-09-15T21:14:21Z","type":"event_msg","payload":{"type":"user_message","message":"` + text + `","turn_id":"b"}}`
	}
	parse := func(lines ...string) *Session {
		t.Helper()
		path := filepath.Join(t.TempDir(), "rollout-x.jsonl")
		writeLines(t, path, lines...)
		s, err := ParseCodex(path)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	for _, size := range []int{100, 200 << 10} {
		s := parse(append(append(append(head, firstEvent), answer...), event(`\n  <environment_context>`+strings.Repeat("e", size)+`</environment_context>`))...)
		if len(s.Turns) != 1 || s.Turns[0].Prompt != "first ask" || s.Turns[0].Reply != "First done." {
			t.Errorf("context of %d bytes: turns %+v", size, s.Turns)
		}
	}
	big := strings.Repeat("z", 200<<10)
	for name, s := range map[string]*Session{
		"a prompt":                   parse(append(append(append(head, firstEvent), answer...), event("second ask "+big))...),
		"a prompt after white space": parse(append(append(append(head, firstEvent), answer...), event(strings.Repeat(" ", 2<<10)+"second ask "+big))...),
		"an image prompt":            parse(append(append(head, answer...), `{"timestamp":"2026-09-15T21:14:21Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<image name=[Image #1]>"},{"type":"input_text","text":"second ask `+big+`"}]}}`)...),
	} {
		if len(s.Turns) != 2 || !s.Turns[1].PromptSkipped {
			t.Errorf("%s over the cap opens no turn: %+v", name, s.Turns)
		}
	}
}

func TestOpeningMessage(t *testing.T) {
	for _, tc := range []struct{ opening, want string }{
		{`{"type":"event_msg","payload":{"type":"user_message","message":"\n <environment_context>abc`, "\n <environment_context>abc"},
		{`{"type":"event_msg","payload":{"type":"user_message","message":"fix it\u00e`, "fix it"},
		{`{"type":"event_msg","payload":{"type":"user_message","message":"say \"hi\"","turn_id":"b"}}`, `say "hi"`},
		{`{"type":"event_msg","payload":{"type":"user_message","images":[{"message":"no"}],"message":"yes`, "yes"},
		{`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<user_instructions>x`, ""},
	} {
		if got := openingMessage([]byte(tc.opening)); got != tc.want {
			t.Errorf("openingMessage(%s) = %q, want %q", tc.opening, got, tc.want)
		}
	}
}

// An over-cap prompt the file ends inside, before its newline, is a record
// still being written: like a short partial line it opens no turn and counts
// nothing. With its newline it is the placeholder.
func TestAnUnfinishedOverCapTailSaysNothing(t *testing.T) {
	withMaxLine(t, 100<<10)
	big := strings.Repeat("z", 200<<10)
	c := `"cwd":"/w","sessionId":"s1"`
	cases := []struct {
		name  string
		file  func(dir string) string
		parse func(string) (*Session, error)
		whole string // the records before the tail
		tail  string // the over-cap prompt, without its newline
	}{
		{"claude", func(dir string) string { return filepath.Join(dir, "s1.jsonl") }, ParseClaude,
			`{"type":"user",` + c + `,"timestamp":"2026-11-02T10:00:00Z","origin":{"kind":"human"},"promptSource":"typed","message":{"role":"user","content":"first"}}` + "\n" +
				`{"type":"assistant",` + c + `,"timestamp":"2026-11-02T10:00:05Z","message":{"id":"m1","model":"claude-opus-5","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"First answer."}]}}` + "\n",
			`{"type":"user",` + c + `,"timestamp":"2026-11-02T10:01:00Z","origin":{"kind":"human"},"promptSource":"typed","message":{"role":"user","content":"` + big + `"}}`},
		{"codex", func(dir string) string { return filepath.Join(dir, "rollout-x.jsonl") }, ParseCodex,
			`{"timestamp":"2026-09-15T21:08:05Z","type":"session_meta","payload":{"id":"c1","cwd":"/w","source":"cli"}}` + "\n" +
				`{"timestamp":"2026-09-15T21:13:20Z","type":"event_msg","payload":{"type":"task_started","turn_id":"a"}}` + "\n" +
				`{"timestamp":"2026-09-15T21:13:21Z","type":"event_msg","payload":{"type":"user_message","message":"first","turn_id":"a"}}` + "\n" +
				`{"timestamp":"2026-09-15T21:13:22Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"First answer."}]}}` + "\n" +
				`{"timestamp":"2026-09-15T21:13:23Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"a","last_agent_message":"First answer."}}` + "\n" +
				`{"timestamp":"2026-09-15T21:14:20Z","type":"event_msg","payload":{"type":"task_started","turn_id":"b"}}` + "\n",
			`{"timestamp":"2026-09-15T21:14:21Z","type":"event_msg","payload":{"type":"user_message","message":"` + big + `","turn_id":"b"}}`},
		{"agy", func(dir string) string { return AgyTranscript(dir, "a1") }, ParseAgy,
			`{"type":"USER_INPUT","created_at":"2026-11-05T12:00:00Z","content":"first"}` + "\n" +
				`{"type":"PLANNER_RESPONSE","created_at":"2026-11-05T12:00:10Z","content":"First answer."}` + "\n",
			`{"type":"USER_INPUT","created_at":"2026-11-05T12:01:00Z","content":"` + big + `"}`},
	}
	for _, c := range cases {
		for _, newline := range []bool{false, true} {
			path := c.file(t.TempDir())
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			body := c.whole + c.tail
			if newline {
				body += "\n"
			}
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			s, err := c.parse(path)
			if err != nil {
				t.Fatal(c.name, err)
			}
			skipped := s.Skipped
			for _, turn := range s.Turns {
				skipped += turn.Skipped
			}
			switch {
			case !newline && (len(s.Turns) != 1 || skipped != 0):
				t.Errorf("%s, unfinished: %d turns, %d skipped; want 1 and 0", c.name, len(s.Turns), skipped)
			case newline && (len(s.Turns) != 2 || !s.Turns[1].PromptSkipped || skipped != 1):
				t.Errorf("%s, finished: %+v", c.name, s.Turns)
			}
		}
	}
}
