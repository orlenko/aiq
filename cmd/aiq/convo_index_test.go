package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

// On the navigator path the resolver finds a bare Codex lease's transcript
// through the index: the same transcript as the direct walk, and one walk
// of the rollouts however often it is asked within the TTL.
func TestUnreportedThroughTheIndex(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "work")
	os.MkdirAll(work, 0o700)
	work = canonicalPath(work)
	codexHome := filepath.Join(root, "codex")
	rollout := filepath.Join(codexHome, "sessions", "2026", "10", "21", "rollout-2026-10-21T10-00-00-5e551011-0000-4000-8000-0000000000c1.jsonl")
	os.MkdirAll(filepath.Dir(rollout), 0o700)
	meta := `{"timestamp":"2026-10-21T10:00:00Z","type":"session_meta","payload":{"id":"5e551011-0000-4000-8000-0000000000c1","cwd":"` + work + `","originator":"codex-tui","source":"cli"}}` + "\n"
	if err := os.WriteFile(rollout, []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	roots := transcript.DefaultRoots(filepath.Join(root, "claude"), codexHome, "", "")
	lease := state.Lease{ID: 7, Provider: "codex", Mode: state.ModeLong, Cwd: work, StartedAt: time.Now().Add(-time.Hour).Unix()}

	want := (&convoResolver{st: st, roots: roots}).unreported([]state.Lease{lease})
	if want[7] != rollout {
		t.Fatalf("direct walk: %v", want)
	}
	idx := transcript.NewIndex(roots)
	r := &convoResolver{st: st, roots: roots, src: idx}
	for i := 0; i < 3; i++ {
		if got := r.unreported([]state.Lease{lease}); got[7] != rollout {
			t.Fatalf("poll %d through the index: %v", i, got)
		}
	}
	if idx.Walks() != 1 {
		t.Errorf("%d rollout walks for three polls", idx.Walks())
	}
}
