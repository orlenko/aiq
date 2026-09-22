package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func agyFixture() []string {
	return []string{
		`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-09-04T17:57:50Z","content":"<USER_REQUEST>\nfix the build\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nThe current local time is: 2026-09-04T13:57:50-04:00.\n</ADDITIONAL_METADATA>"}`,
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-09-04T17:57:52Z","content":"","thinking":"…","tool_calls":[{"name":"run_command","args":{"CommandLine":"\"go build ./...\""}}]}`,
		`{"step_index":2,"source":"MODEL","type":"GENERIC","status":"DONE","created_at":"2026-09-04T17:57:55Z","content":"The command exited with code 1."}`,
		`{"step_index":3,"source":"SYSTEM","type":"SYSTEM_MESSAGE","status":"DONE","created_at":"2026-09-04T17:57:56Z","content":"The following is a <SYSTEM_MESSAGE> not actually sent by the user."}`,
		`{"step_index":4,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-09-04T17:58:10Z","content":"Fixed the missing import.","tool_calls":[{"name":"replace_file_content","args":{}},{"name":"run_command","args":{}}]}`,
		`{"step_index":5,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-09-04T18:10:00Z","content":"<USER_REQUEST>\nnow the tests\n</USER_REQUEST>"}`,
		`{"step_index":6,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-09-04T18:10:30Z","content":"All green."}`,
	}
}

func TestParseAgy(t *testing.T) {
	root := t.TempDir()
	path := AgyTranscript(root, "a1b2")
	writeLines(t, path, agyFixture()...)
	s, err := ParseAgy(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Provider != "agy" || s.ID != "a1b2" || len(s.Turns) != 2 {
		t.Fatalf("%+v", s)
	}
	first := s.Turns[0]
	if first.Prompt != "fix the build" || first.Reply != "Fixed the missing import." || first.Tools != 3 {
		t.Errorf("turn 1: %+v", first)
	}
	if first.Started.Format("15:04:05") != "17:57:50" || first.Ended.Format("15:04:05") != "17:58:10" {
		t.Errorf("turn 1 times: %v %v", first.Started, first.Ended)
	}
	if s.Turns[1].Prompt != "now the tests" || s.Turns[1].Reply != "All green." || s.Turns[1].Tools != 0 {
		t.Errorf("turn 2: %+v", s.Turns[1])
	}
	if s.Started.Format("15:04:05") != "17:57:50" || s.Updated.Format("15:04:05") != "18:10:30" {
		t.Errorf("session times: %v %v", s.Started, s.Updated)
	}
}

func TestListAgy(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "work")
	os.MkdirAll(work, 0o700)
	work = canonical(work)
	appData := filepath.Join(root, "antigravity-cli")
	writeLines(t, AgyTranscript(appData, "here1"), agyFixture()...)
	writeLines(t, AgyTranscript(appData, "here2"), agyFixture()[5:]...)
	writeLines(t, AgyTranscript(appData, "elsewhere"), agyFixture()...)
	writeLines(t, AgyTranscript(appData, "gone"), agyFixture()...)
	writeLines(t, filepath.Join(appData, "history.jsonl"),
		`{"display":"fix the build","timestamp":1,"workspace":"`+work+`","conversationId":"here1"}`,
		`{"display":"/usage","timestamp":2,"type":"slash_command","workspace":"`+work+`","conversationId":"here1"}`,
		`{"display":"x","timestamp":3,"workspace":"/elsewhere","conversationId":"elsewhere"}`,
		`{"display":"x","timestamp":4,"workspace":"`+work+`","conversationId":"missing"}`,
	)
	os.MkdirAll(filepath.Join(appData, "cache"), 0o700)
	os.WriteFile(filepath.Join(appData, "cache", "last_conversations.json"), []byte(`{"`+work+`":"here2","/other":"gone"}`), 0o600)

	list, err := List(DefaultRoots(filepath.Join(root, "claude"), filepath.Join(root, "codex"), appData, ""), work)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range list {
		got = append(got, s.Provider+"/"+s.ID+"/"+s.Label())
	}
	want := []string{"agy/here1/fix the build", "agy/here2/now the tests"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}
}
