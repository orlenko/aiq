package transcript

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/proc"
)

// ClaudeOpen maps the ids of Claude sessions that are open right now to
// the pid holding them. Claude Code writes sessions/<pid>.json while it
// runs; a file whose process is gone is ignored.
func ClaudeOpen(realClaude string) map[string]int {
	out := map[string]int{}
	files, _ := filepath.Glob(filepath.Join(realClaude, "sessions", "*.json"))
	for _, f := range files {
		data, err := ReadRegular(f)
		if err != nil {
			continue
		}
		var e struct {
			PID       int    `json:"pid"`
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(data, &e) != nil || e.PID <= 0 || e.SessionID == "" {
			continue
		}
		if proc.Alive(e.PID) {
			out[e.SessionID] = e.PID
		}
	}
	return out
}

// Written lists the transcripts of interactive Codex or Antigravity
// sessions in dir written at or after since, newest first. aiq runs a short
// session of either CLI without hooks, so no hook reports its transcript;
// this is how to find it from the session's lease.
func Written(r Roots, provider, dir string, since time.Time) []string {
	return DirectWalk(r).Written(context.Background(), provider, dir, since)
}

func (w directWalk) Written(ctx context.Context, provider, dir string, since time.Time) []string {
	dir = canonical(dir)
	var paths []Candidate
	switch provider {
	case "codex":
		for _, f := range codexFiles(ctx, w.r.CodexSessions, dir, since) {
			meta, _ := codexMeta(f.Path)
			if !interactive(meta) {
				continue
			}
			paths = append(paths, f)
		}
	case "agy":
		paths = agyFiles(ctx, w.r.AgyAppData, dir)
	}
	return newestWritten(paths, since)
}

// interactive says a rollout's head is a session someone typed into: not
// codex exec, not a subagent.
func interactive(meta codexPayload) bool {
	var source string
	json.Unmarshal(meta.Source, &source)
	return !(source == "exec" || strings.HasSuffix(meta.Originator, "_exec") || isSubagent(meta.Source))
}

// newestWritten is the paths of cands modified at or after since, newest
// first, each file once.
func newestWritten(cands []Candidate, since time.Time) []string {
	type written struct {
		path string
		mod  time.Time
	}
	var out []written
	seen := map[string]bool{}
	for _, f := range cands {
		info, err := os.Stat(f.Path)
		if err != nil || info.ModTime().Before(since) || seen[canonical(f.Path)] {
			continue
		}
		seen[canonical(f.Path)] = true
		out = append(out, written{f.Path, info.ModTime()})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].mod.After(out[j].mod) })
	list := make([]string, len(out))
	for i, w := range out {
		list[i] = w.path
	}
	return list
}
