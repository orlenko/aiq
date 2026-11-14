package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/tmux"
	"github.com/orlenko/aiq/internal/transcript"
)

// Distinct invented PIDs force the resolver to use its supplied process graph.
// Candidate order deliberately puts the nested Claude before the outer agent.
func TestGoldenResolveAncestry(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	projects := filepath.Join(root, "claude", "projects")
	project := filepath.Join(projects, claudeNonAlnum.ReplaceAllString(work, "-"))
	for _, id := range []string{goldenC1, goldenC2} {
		c := &goldenClaude{cwd: work, id: id, at: goldenAt(12, 9, 0, 0)}
		c.typed("invented task")
		c.answer("answer", "Done.")
		goldenWrite(t, filepath.Join(project, id+".jsonl"), c.lines...)
	}
	rollout := filepath.Join(root, "codex", "rollout-"+goldenX1+".jsonl")
	goldenWrite(t, rollout, goldenCodex(goldenX1, work, "cli", goldenAt(12, 10, 0, 0), [2]string{"invented task", "Done."})...)
	st, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, l := range []state.Lease{
		{PID: 210, Pane: "%11", AccountID: "codex/descendant"},
		{PID: 310, Pane: "%12", AccountID: "codex/unrelated"},
	} {
		l.Provider, l.Mode, l.Hostname = "codex", state.ModeLong, "invented-host"
		l.Transcript, l.SessionID, l.Cwd = rollout, goldenX1, work
		l.StartedAt = goldenAt(12, 9, 59, 0).Unix()
		if _, err := st.AddLease(l); err != nil {
			t.Fatal(err)
		}
	}
	parents := map[int]int{
		105: 100, 110: 105, 120: 110, 130: 999,
		205: 200, 210: 205, 220: 210,
		310: 999, 320: 999,
	}
	got := goldenEnv(t, func() {
		r := newConvoResolver(st, transcript.Roots{ClaudeProjects: []string{projects}})
		r.host = func() string { return "invented-host" }
		r.alive = func(int) bool { return true }
		r.parents = func() map[int]int { return parents }
		r.claude = func() []claudeLive {
			return []claudeLive{
				{PID: 120, SessionID: goldenC2, Cwd: work, Tmux: "demo:@1.%10"},
				{PID: 130, SessionID: goldenC2, Cwd: work, Tmux: "demo:@1.%10"},
				{PID: 110, SessionID: goldenC1, Cwd: work, Tmux: "demo:@1.%10"},
				{PID: 220, SessionID: goldenC2, Cwd: work, Tmux: "demo:@1.%11"},
				{PID: 320, SessionID: goldenC2, Cwd: work, Tmux: "demo:@1.%12"},
			}
		}
		r.paneInfo = func(pane string) (tmux.Pane, error) {
			return tmux.Pane{ID: pane, PID: map[string]int{"%10": 100, "%11": 200, "%12": 300}[pane], StartCommand: "shell"}, nil
		}
		for _, pane := range []string{"%10", "%11", "%12"} {
			tgt, err := r.resolvePane(pane)
			if err != nil {
				fmt.Printf("pane %s: error: %v\n", pane, err)
				continue
			}
			fmt.Printf("pane %s: provider %s, path %s, account %q, pane %q\n", pane, tgt.provider, tgt.path, tgt.account, tgt.pane)
		}
	})
	got = strings.ReplaceAll(got, claudeNonAlnum.ReplaceAllString(root, "-"), "<root as a project>")
	checkGolden(t, "resolve-ancestry.golden", strings.ReplaceAll(got, root, "<root>"))
}
