package claude

import (
	"reflect"
	"regexp"
	"testing"
)

func TestSession(t *testing.T) {
	uuid := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	fresh := [][]string{
		nil,
		{"--dangerously-skip-permissions"},
		{"--model", "opus", "fix the bug"},
		{"--model", "agents", "hello"}, // a flag value that looks like a subcommand
		{"why do agents stall?"},
	}
	for _, args := range fresh {
		id, out := Session(args)
		if !uuid.MatchString(id) {
			t.Errorf("%v: id %q", args, id)
		}
		if want := append([]string{"--session-id", id}, args...); !reflect.DeepEqual(out, want) {
			t.Errorf("%v: got %v", args, out)
		}
	}
	known := map[string][]string{
		"s1": {"--resume", "s1"},
		"s2": {"--dangerously-skip-permissions", "-r", "s2"},
		"s3": {"--resume=s3"},
		"s4": {"--session-id", "s4", "hi"},
		"s5": {"--session-id=s5"},
	}
	for want, args := range known {
		if id, out := Session(args); id != want || !reflect.DeepEqual(out, args) {
			t.Errorf("%v: got %q %v", args, id, out)
		}
	}
	unknown := [][]string{
		{"--continue"}, {"-c", "more"}, {"--resume"}, {"-r", "--model", "x"},
		{"--resume", "s1", "--fork-session"}, {"--bg", "task"}, {"-p", "hi"},
		{"mcp", "list"}, {"agents"}, {"--model", "opus", "plugin", "install", "x"},
	}
	for _, args := range unknown {
		if id, out := Session(args); id != "" || !reflect.DeepEqual(out, args) {
			t.Errorf("%v: got %q %v", args, id, out)
		}
	}
}
