package main

import (
	"reflect"
	"testing"
)

func TestLongArgsPerProvider(t *testing.T) {
	f := runFlags{rest: []string{"--model", "x"}, resumeSession: "s1", nudge: "carry on"}
	cases := map[string][]string{
		"claude": {"--settings", `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"/bin/aiq claude-hook sessionstart","timeout":30}]}],"UserPromptSubmit":[{"hooks":[{"type":"command","command":"/bin/aiq claude-hook userpromptsubmit","timeout":30}]}],"Stop":[{"hooks":[{"type":"command","command":"/bin/aiq claude-hook stop","timeout":30}]}]}}`,
			"--model", "x", "--resume", "s1", "carry on"},
		"agy": {"--model", "x", "--conversation", "s1", "--prompt-interactive", "carry on"},
	}
	for provider, want := range cases {
		if got := longArgs(provider, "/bin/aiq", f); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %q, want %q", provider, got, want)
		}
	}
	codex := longArgs("codex", "/bin/aiq", f)
	if n := len(codex); n < 6 || codex[n-3] != "resume" || codex[n-2] != "s1" || codex[n-1] != "carry on" {
		t.Errorf("codex: %q", codex)
	}
}

func TestTakeoverNameIsAnArgumentNotPromptText(t *testing.T) {
	const title = "Harp 雪 'quoted'"
	f := parseRunFlags([]string{"--long", "--session-name", title, "--nudge", "carry on", "--", "--model", "x"})
	if f.sessionName != title || f.nudge != "carry on" {
		t.Fatalf("flags: %+v", f)
	}
	for _, provider := range []string{"claude", "copilot"} {
		args := longArgs(provider, "/bin/aiq", f)
		found := false
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--name" && args[i+1] == title {
				found = true
			}
		}
		if !found {
			t.Errorf("%s omitted inherited name: %q", provider, args)
		}
	}
	// Codex uses a pre-named thread and resumes it with the original nudge.
	f.resumeSession = "named-thread"
	args := longArgs("codex", "/bin/aiq", f)
	if got := args[len(args)-3:]; !reflect.DeepEqual(got, []string{"resume", "named-thread", "carry on"}) {
		t.Fatalf("Codex resume: %q", args)
	}
}

func TestAutoWorkerArgsPerProvider(t *testing.T) {
	r := autoRequest{print: true, hasPrompt: true, prompt: "do it"}
	cases := map[string][]string{
		"claude":  {"-p", "--dangerously-skip-permissions", "--", "do it"},
		"codex":   {"exec", "--yolo", "--", "do it"},
		"agy":     {"--dangerously-skip-permissions", "-p", "do it"},
		"copilot": {"--yolo", "-p", "do it"},
	}
	for provider, want := range cases {
		if got := r.args(provider); !reflect.DeepEqual(got, want) {
			t.Errorf("%s worker: got %q, want %q", provider, got, want)
		}
	}
	interactive := autoRequest{}
	for provider, want := range map[string][]string{"claude": {"--dangerously-skip-permissions"}, "codex": {"--yolo"}, "agy": {"--dangerously-skip-permissions"}, "copilot": {"--yolo"}} {
		if got := interactive.args(provider); !reflect.DeepEqual(got, want) {
			t.Errorf("%s interactive: got %q, want %q", provider, got, want)
		}
	}
	first := autoRequest{hasPrompt: true, prompt: "start"}
	for provider, want := range map[string][]string{"claude": {"--dangerously-skip-permissions", "--", "start"}, "copilot": {"--yolo", "-i", "start"}} {
		if got := first.args(provider); !reflect.DeepEqual(got, want) {
			t.Errorf("%s first prompt: got %q, want %q", provider, got, want)
		}
	}
}

func TestProviderList(t *testing.T) {
	if got := providerList(); got != "claude, codex, agy or copilot" {
		t.Errorf("providerList: %q", got)
	}
	if got := joinOr([]string{"one"}); got != "one" {
		t.Errorf("joinOr one: %q", got)
	}
}
