package codex

import "testing"

func TestSession(t *testing.T) {
	for want, cases := range map[string][][]string{
		"t1": {{"resume", "t1"}, {"--yolo", "resume", "t1"}, {"--dangerously-bypass-approvals-and-sandbox", "resume", "t1", "go on"},
			{"resume", "--yolo", "t1"}},
		"": {nil, {"resume"}, {"resume", "--last"}, {"fork", "t1"}, {"write a resume", "x"}, {"--", "resume", "t1"},
			{"exec", "resume", "t1"}},
	} {
		for _, args := range cases {
			if got := Session(args); got != want {
				t.Errorf("%v: got %q, want %q", args, got, want)
			}
		}
	}
}
