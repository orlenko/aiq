package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fuzzParse writes data where parse expects a transcript and parses it, with
// the line cap at its own value or, when the first byte is odd, low enough
// for a fuzzed line to reach it. A parse must not panic, and what it returns
// must hold together.
func fuzzParse(f *testing.F, name string, seeds []string, parse func(string) (*Session, error)) {
	for _, s := range seeds {
		f.Add([]byte("\x00" + s))
		f.Add([]byte("\x01" + s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) == 0 {
			return
		}
		old := MaxLine
		defer func() { MaxLine = old }()
		if data[0]&1 == 1 {
			MaxLine = 64
		}
		path := filepath.Join(t.TempDir(), name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data[1:], 0o600); err != nil {
			t.Fatal(err)
		}
		s, err := parse(path)
		if err != nil || s == nil {
			return
		}
		if s.Skipped < 0 {
			t.Fatalf("skipped %d", s.Skipped)
		}
		for _, turn := range s.Turns {
			if turn.Skipped < 0 || turn.Tools < 0 {
				t.Fatalf("turn %+v", turn)
			}
			for _, it := range turn.Items {
				if it.Kind > ItemAnswer {
					t.Fatalf("item %+v", it)
				}
			}
		}
		Summarize(s)
	})
}

func FuzzParseClaude(f *testing.F) {
	fuzzParse(f, "s1.jsonl", []string{
		strings.Join(claudeFixture("/w"), "\n") + "\n",
		`{"type":"user","cwd":"/w","timestamp":"2026-09-01T14:00:00Z","message":{"role":"user","content":"go"}}` + "\n" +
			`{"type":"assistant","timestamp":"2026-09-01T14:00:05Z","message":{"id":"m1","role":"assistant","stop_reason":"end_turn","content":[{"type":"thinking","thinking":"hm"}]}}`,
		`{"type":"attachment","attachment":{"type":"queued_command","prompt":"also this","commandMode":"prompt"}}`,
	}, ParseClaude)
}

func FuzzParseCodex(f *testing.F) {
	fuzzParse(f, "rollout-x.jsonl", []string{
		strings.Join(codexFixture("/w", "c1"), "\n") + "\n",
		`{"timestamp":"2026-09-15T21:08:05Z","type":"session_meta","payload":{"id":"c1","cwd":"/w","source":{"subagent":{}}}}`,
		`{"timestamp":"2026-09-15T21:13:20Z","ordinal":3,"type":"event_msg","payload":{"type":"token_count"}}` + "\n",
	}, ParseCodex)
}

func FuzzParseAgy(f *testing.F) {
	fuzzParse(f, filepath.Join("brain", "a1", ".system_generated", "logs", "transcript.jsonl"), []string{
		strings.Join(agyFixture(), "\n") + "\n",
		`{"type":"PLANNER_RESPONSE","created_at":"2026-09-04T17:58:00Z","content":"x","tool_calls":[{},{}]}`,
	}, ParseAgy)
}
