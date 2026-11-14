package transcript

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// spySource is a Discovery that records whether ByID was asked.
type spySource struct {
	Discovery
	asked bool
}

func (s *spySource) ByID(ctx context.Context, provider, id string) ([]Candidate, error) {
	s.asked = true
	return s.Discovery.ByID(ctx, provider, id)
}

// An id of the wrong shape is refused before any path is built.
func TestFindByIDRefusesBadIDs(t *testing.T) {
	for _, tc := range []struct{ provider, id string }{
		{"claude", ""}, {"claude", "."}, {"claude", ".."}, {"claude", "." + idA[1:]},
		{"claude", strings.Repeat("a", 129)}, {"claude", "a/b"}, {"claude", `a\b`},
		{"claude", idA[:35] + "*"}, {"claude", idA[:35] + "?"}, {"claude", "[" + idA[1:]},
		{"claude", idA[:35] + "\x1b"}, {"claude", idA[:35] + "\x7f"}, {"claude", idA[:35] + "\x00"},
		{"claude", "s1"}, {"codex", "c1"}, {"agy", "here1"}, {"copilot", idA},
		{"codex", "../" + idA}, {"agy", idA + "/.."},
	} {
		spy := &spySource{Discovery: DirectWalk(Roots{})}
		if _, err := FindByID(context.Background(), spy, tc.provider, tc.id, ParseSummary); !errors.Is(err, ErrBadID) || spy.asked {
			t.Errorf("%s %q: %v, source asked %v", tc.provider, tc.id, err, spy.asked)
		}
	}
	for _, p := range []string{"claude", "codex", "agy"} {
		if _, err := FindByID(context.Background(), DirectWalk(Roots{}), p, idA, ParseSummary); !errors.Is(err, ErrNoSuchSession) {
			t.Errorf("%s with no roots: %v", p, err)
		}
	}
}

// findFixture is two Claude roots, two Codex roots and an Antigravity app
// data dir, the second of each labelled as an account's.
func findFixture(t *testing.T) (Roots, string) {
	t.Helper()
	root := t.TempDir()
	r := Roots{
		ClaudeProjects: []string{filepath.Join(root, "real", "projects"), filepath.Join(root, "acct", "projects")},
		CodexSessions:  []string{filepath.Join(root, "real", "sessions"), filepath.Join(root, "acct", "sessions")},
		AgyAppData:     []string{filepath.Join(root, "agy")},
		Labels: map[string]string{
			filepath.Join(root, "acct", "projects"): "claude/acct-b",
			filepath.Join(root, "acct", "sessions"): "codex/acct-b",
		},
	}
	return r, root
}

func setMtime(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

var t0 = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

// The newest copy wins by its file's time, a labelled root's hit carries
// the label it was given, and the only copy in an overlay is that one.
func TestFindByIDNewestAndLabels(t *testing.T) {
	r, _ := findFixture(t)
	real := filepath.Join(r.ClaudeProjects[0], "-w", idA+".jsonl")
	acct := filepath.Join(r.ClaudeProjects[1], "-w", idA+".jsonl")
	writeLines(t, real, "{}")
	writeLines(t, acct, "{}")
	setMtime(t, real, t0)
	setMtime(t, acct, t0.Add(time.Minute))
	hit, err := FindByID(context.Background(), DirectWalk(r), "claude", idA, ParseSummary)
	if err != nil || hit != (Hit{Path: acct, Root: r.ClaudeProjects[1], Account: "claude/acct-b"}) {
		t.Errorf("newer copy in the overlay: %+v %v", hit, err)
	}
	setMtime(t, real, t0.Add(2*time.Minute))
	hit, _ = FindByID(context.Background(), DirectWalk(r), "claude", idA, ParseSummary)
	if hit != (Hit{Path: real, Root: r.ClaudeProjects[0]}) {
		t.Errorf("newer copy under the real home: %+v", hit)
	}

	rollout := filepath.Join(r.CodexSessions[1], "2026", "09", "15", "rollout-2026-09-15T21-08-05-"+idB+".jsonl")
	writeLines(t, rollout, "{}")
	hit, err = FindByID(context.Background(), DirectWalk(r), "codex", idB, ParseSummary)
	if err != nil || hit != (Hit{Path: rollout, Root: r.CodexSessions[1], Account: "codex/acct-b"}) {
		t.Errorf("only copy in an overlay: %+v %v", hit, err)
	}
	if _, err := FindByID(context.Background(), DirectWalk(r), "codex", idA, ParseSummary); !errors.Is(err, ErrNoSuchSession) {
		t.Errorf("an id no rollout is named for: %v", err)
	}

	agy := AgyTranscript(r.AgyAppData[0], idB)
	writeLines(t, agy, agyFixture()...)
	if hit, err := FindByID(context.Background(), DirectWalk(r), "agy", idB, ParseSummary); err != nil || hit.Path != agy || hit.Account != "" {
		t.Errorf("agy: %+v %v", hit, err)
	}
}

// Copies with the same time are told apart by the session's own Updated,
// through parse.
func TestFindByIDTieBrokenByParse(t *testing.T) {
	r, _ := findFixture(t)
	real := filepath.Join(r.ClaudeProjects[0], "-w", idA+".jsonl")
	acct := filepath.Join(r.ClaudeProjects[1], "-w", idA+".jsonl")
	writeLines(t, real, "old")
	writeLines(t, acct, "new")
	setMtime(t, real, t0)
	setMtime(t, acct, t0)
	parse := func(ctx context.Context, path string) (Summary, error) {
		data, _ := os.ReadFile(path)
		if strings.HasPrefix(string(data), "new") {
			return Summary{Updated: t0.Add(time.Hour)}, nil
		}
		return Summary{Updated: t0}, nil
	}
	if hit, _ := FindByID(context.Background(), DirectWalk(r), "claude", idA, parse); hit.Path != acct {
		t.Errorf("tie went to %s", hit.Path)
	}
	none := func(ctx context.Context, path string) (Summary, error) { return Summary{}, ErrUnreadable }
	if hit, _ := FindByID(context.Background(), DirectWalk(r), "claude", idA, none); hit.Path != real {
		t.Errorf("an unbroken tie went to %s, not the first root", hit.Path)
	}
}

// One transcript reached under two roots through a link is one copy: its
// time is the regular file's, not the link's, and it is reported under the
// unlabelled root, so it forces no account. Under two labelled roots it is
// reported under the first.
func TestFindByIDOneFileUnderTwoRoots(t *testing.T) {
	r, _ := findFixture(t)
	real := filepath.Join(r.ClaudeProjects[0], "-w", idA+".jsonl")
	writeLines(t, real, "{}")
	setMtime(t, real, t0)
	// The overlay's project directory is the real one, reached by a link
	// made after the file, and listed first.
	os.MkdirAll(r.ClaudeProjects[1], 0o700)
	if err := os.Symlink(filepath.Dir(real), filepath.Join(r.ClaudeProjects[1], "-w")); err != nil {
		t.Skip("no symlinks here:", err)
	}
	r.ClaudeProjects[0], r.ClaudeProjects[1] = r.ClaudeProjects[1], r.ClaudeProjects[0]
	// A third, regular copy, older than the link but newer than the file.
	other := t.TempDir()
	r.ClaudeProjects = append(r.ClaudeProjects, other)
	third := filepath.Join(other, "-w", idA+".jsonl")
	writeLines(t, third, "{}")
	setMtime(t, third, t0.Add(time.Minute))

	hit, err := FindByID(context.Background(), DirectWalk(r), "claude", idA, ParseSummary)
	if err != nil || hit.Path != third {
		t.Errorf("the linked copy was judged by its link: %+v %v", hit, err)
	}
	setMtime(t, third, t0.Add(-time.Minute))
	hit, _ = FindByID(context.Background(), DirectWalk(r), "claude", idA, ParseSummary)
	if hit.Account != "" || hit.Root != r.ClaudeProjects[1] || hit.Path != real {
		t.Errorf("a file under the real home and an overlay forced %+v", hit)
	}

	r.Labels[r.ClaudeProjects[1]] = "claude/acct-c"
	hit, _ = FindByID(context.Background(), DirectWalk(r), "claude", idA, ParseSummary)
	if hit.Root != r.ClaudeProjects[0] || hit.Account != "claude/acct-b" {
		t.Errorf("under two labelled roots: %+v, want the first in Roots order", hit)
	}
}

// Repeated detail lookups for a Codex session walk the rollouts once per
// TTL and parse nothing; a newer copy under another root is followed with
// no list in between within two TTLs and one walk, each poll past the TTL
// answered from the last scan.
func TestIndexFindFollowsANewerCodexCopy(t *testing.T) {
	r, _ := findFixture(t)
	x, clk := testIndex(r)
	c := NewCache()
	name := "rollout-2026-09-15T21-08-05-" + idA + ".jsonl"
	first := filepath.Join(r.CodexSessions[0], "2026", "09", "15", name)
	writeLines(t, first, codexFixture("/w", idA)...)
	setMtime(t, first, t0)
	for i := 0; i < 5; i++ {
		if hit, err := x.Find(context.Background(), "codex", idA, c.Summary); err != nil || hit.Path != first {
			t.Fatalf("poll %d: %+v %v", i, hit, err)
		}
	}
	if x.Walks() != 1 || c.Stats().Parses != 0 {
		t.Errorf("five polls: %d walks, %d parses", x.Walks(), c.Stats().Parses)
	}

	newer := filepath.Join(r.CodexSessions[1], "2026", "09", "16", name)
	writeLines(t, newer, codexFixture("/w", idA)...)
	setMtime(t, newer, t0.Add(time.Hour))
	start := clk.Now()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		hit, err := x.Find(ctx, "codex", idA, c.Summary)
		cancel()
		if err != nil {
			t.Fatalf("a poll waited on the walk: %v", err)
		}
		if hit.Path == newer {
			break
		}
		if clk.Now().Sub(start) > 2*DiscoveryTTL {
			t.Fatalf("newer copy not followed within two TTLs (walks %d)", x.Walks())
		}
		clk.Add(time.Second)
		time.Sleep(10 * time.Millisecond) // the walk's own time
	}
	if c.Stats().Parses != 0 {
		t.Errorf("%d parses outside a tie", c.Stats().Parses)
	}
}

// Claude has no scan: a lookup is remembered one TTL, after which a newer
// copy under another root is found.
func TestIndexFindFollowsANewerClaudeCopy(t *testing.T) {
	r, _ := findFixture(t)
	x, clk := testIndex(r)
	real := filepath.Join(r.ClaudeProjects[0], "-w", idA+".jsonl")
	writeLines(t, real, "{}")
	setMtime(t, real, t0)
	if hit, _ := x.Find(context.Background(), "claude", idA, ParseSummary); hit.Path != real {
		t.Fatal(hit)
	}
	acct := filepath.Join(r.ClaudeProjects[1], "-w", idA+".jsonl")
	writeLines(t, acct, "{}")
	setMtime(t, acct, t0.Add(time.Hour))
	clk.Add(DiscoveryTTL - time.Second)
	if hit, _ := x.Find(context.Background(), "claude", idA, ParseSummary); hit.Path != real {
		t.Errorf("inside the TTL the lookup was not remembered: %+v", hit)
	}
	clk.Add(time.Second)
	if hit, _ := x.Find(context.Background(), "claude", idA, ParseSummary); hit.Path != acct || hit.Account != "claude/acct-b" {
		t.Errorf("after one TTL: %+v", hit)
	}
}

// A remembered Codex lookup is forgotten as soon as the next scan
// completes, before its TTL is out.
func TestIndexFindForgetsOnANewScan(t *testing.T) {
	r, _ := findFixture(t)
	x, clk := testIndex(r)
	name := "rollout-2026-09-15T21-08-05-" + idA + ".jsonl"
	first := filepath.Join(r.CodexSessions[0], "2026", "09", "15", name)
	writeLines(t, first, codexFixture("/w", idA)...)
	setMtime(t, first, t0)
	x.Find(context.Background(), "codex", idA, ParseSummary)
	newer := filepath.Join(r.CodexSessions[1], "2026", "09", "16", name)
	writeLines(t, newer, codexFixture("/w", idA)...)
	setMtime(t, newer, t0.Add(time.Hour))
	clk.Add(DiscoveryTTL)
	// Past the TTL: answered from the last scan, remembered, and a scan
	// started.
	if hit, _ := x.Find(context.Background(), "codex", idA, ParseSummary); hit.Path != first {
		t.Fatalf("past the TTL: %+v", hit)
	}
	waitWalk(t, x, 2)
	clk.Add(time.Second)
	if hit, _ := x.Find(context.Background(), "codex", idA, ParseSummary); hit.Path != newer {
		t.Errorf("after the next scan completed: %+v, want the newer copy", hit)
	}
}
