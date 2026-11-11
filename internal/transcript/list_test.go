package transcript

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// listFixture builds a directory with sessions of every provider, a copy of
// one under an overlay home, an empty one, one of another directory sharing
// its encoded name, and a Codex thread name.
func listFixture(t *testing.T) (Roots, string) {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "work", "my.app")
	other := filepath.Join(root, "work", "my-app")
	for _, d := range []string{work, other} {
		os.MkdirAll(d, 0o700)
	}
	work, other = canonical(work), canonical(other)
	enc := nonAlnum.ReplaceAllString(work, "-")
	claudeHome, codexHome := filepath.Join(root, "claude"), filepath.Join(root, "codex")
	overlays := filepath.Join(root, "overlays")
	appData := filepath.Join(root, "antigravity-cli")

	writeLines(t, filepath.Join(claudeHome, "projects", enc, "s1.jsonl"), claudeFixture(work)...)
	writeLines(t, filepath.Join(claudeHome, "projects", enc, "s2.jsonl"), strings.ReplaceAll(strings.Join(claudeFixture(other), "\n"), `"s1"`, `"s2"`))
	writeLines(t, filepath.Join(claudeHome, "projects", enc, "s3.jsonl"), `{"type":"permission-mode","permissionMode":"default"}`)
	writeLines(t, filepath.Join(claudeHome, "projects", enc, "s4.jsonl"), strings.ReplaceAll(strings.Join(claudeFixture(work), "\n"), `"s1"`, `"s4"`))
	writeLines(t, filepath.Join(overlays, "acct", "projects", enc, "s1.jsonl"), claudeFixture(work)[:6]...)
	day := filepath.Join(codexHome, "sessions", "2026", "09", "15")
	writeLines(t, filepath.Join(day, "rollout-a.jsonl"), codexFixture(work, "c1")...)
	writeLines(t, filepath.Join(day, "rollout-b.jsonl"), codexFixture("/elsewhere", "c2")...)
	writeLines(t, filepath.Join(day, "rollout-c.jsonl"), strings.ReplaceAll(strings.Join(codexFixture(work, "c3"), "\n"), `"source":"cli"`, `"source":"exec"`))
	writeLines(t, filepath.Join(codexHome, "session_index.jsonl"), `{"id":"c1","thread_name":"blog post"}`)
	writeLines(t, AgyTranscript(appData, "here1"), agyFixture()...)
	writeLines(t, filepath.Join(appData, "history.jsonl"), `{"display":"x","timestamp":1,"workspace":"`+work+`","conversationId":"here1"}`)
	return DefaultRoots(claudeHome, codexHome, appData, "", overlays), work
}

// List and ListWith over the direct walk find the same sessions, in the same
// order, and each row is what the session it stands for summarises to.
func TestListWithMatchesList(t *testing.T) {
	r, work := listFixture(t)
	list, unreadable, err := List(r, work)
	if err != nil || len(unreadable) != 0 {
		t.Fatal(err, unreadable)
	}
	res, err := ListWith(context.Background(), DirectWalk(r), work, ParseSummary)
	if err != nil {
		t.Fatal(err)
	}
	if res.Partial || res.Scanned != res.Of || res.Of != 8 || len(res.Unreadable) != 0 {
		t.Fatalf("scanned %d of %d, partial %v, unreadable %v", res.Scanned, res.Of, res.Partial, res.Unreadable)
	}
	var rows []Summary
	for _, row := range res.Rows {
		if row.Empty {
			if row.ID != "s3" {
				t.Errorf("%s/%s marked empty", row.Provider, row.ID)
			}
			continue
		}
		rows = append(rows, row)
	}
	if len(rows) != len(list) {
		t.Fatalf("ListWith has %d sessions, List %d", len(rows), len(list))
	}
	var ids []string
	for i := range list {
		ids = append(ids, list[i].Provider+"/"+list[i].ID)
		if want := Summarize(&list[i]); !reflect.DeepEqual(rows[i], want) {
			t.Errorf("row %d:\n got %+v\nwant %+v", i, rows[i], want)
		}
	}
	if got, want := strings.Join(ids, ","), "codex/c1,codex/c3,claude/s1,claude/s4,agy/here1"; got != want {
		t.Errorf("sessions %s, want %s", got, want)
	}
	if rows[0].Label != "blog post" || list[0].Title != "blog post" {
		t.Errorf("the Codex thread name was not applied: %q %q", rows[0].Label, list[0].Title)
	}
}

// A parse that panics is recovered: List leaves the file out, names it in its
// second value, and prints nothing; the panic's value goes nowhere.
func TestListRecoversAPanickingParse(t *testing.T) {
	r, work := listFixture(t)
	bad := filepath.Join(r.ClaudeProjects[0], nonAlnum.ReplaceAllString(work, "-"), "s4.jsonl")
	defer SetParserForTest(func(path string) (*Session, error) {
		if path == bad {
			panic("secret transcript text")
		}
		return parseByPath(path)
	})()

	stdout, stderr := os.Stdout, os.Stderr
	rd, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = w, w
	list, unreadable, err := List(r, work)
	os.Stdout, os.Stderr = stdout, stderr
	w.Close()
	printed, _ := io.ReadAll(rd)
	if err != nil {
		t.Fatal(err)
	}
	if len(printed) != 0 {
		t.Errorf("List printed %q", printed)
	}
	if len(unreadable) != 1 || unreadable[0] != bad {
		t.Fatalf("unreadable %v, want [%s]", unreadable, bad)
	}
	for _, s := range list {
		if s.ID == "s4" {
			t.Error("the unreadable session is listed")
		}
	}
	if len(list) != 4 {
		t.Errorf("%d sessions listed, want the 4 others", len(list))
	}

	_, err = ParseFile(bad)
	if !errors.Is(err, ErrUnreadable) || strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), bad) {
		t.Errorf("ParseFile: %v", err)
	}
	res, _ := ListWith(context.Background(), DirectWalk(r), work, ParseSummary)
	if len(res.Unreadable) != 1 || res.Unreadable[0] != bad || res.Scanned != res.Of {
		t.Errorf("ListWith: unreadable %v, scanned %d of %d", res.Unreadable, res.Scanned, res.Of)
	}
}

// Claude's candidates come in one order when the working directory is dir
// by a link, and so has a project directory of its own.
func TestClaudeCandidatesInAStableOrder(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skip(err)
	}
	t.Chdir(link)
	t.Setenv("PWD", link)
	root := t.TempDir()
	for _, p := range []string{dir, link} {
		writeLines(t, filepath.Join(root, nonAlnum.ReplaceAllString(p, "-"), "s1.jsonl"), `{"type":"permission-mode"}`)
	}
	first := claudeFiles(context.Background(), []string{root}, dir)
	if len(first) != 2 {
		t.Fatalf("candidates %v, want one under each name", first)
	}
	for range 200 {
		if got := claudeFiles(context.Background(), []string{root}, dir); !reflect.DeepEqual(got, first) {
			t.Fatalf("candidates %v, then %v", first, got)
		}
	}
}

// A parse handed to ListWith that panics itself is recovered there: the
// file is unreadable, the rest are listed, and the value goes nowhere.
func TestListWithRecoversItsParse(t *testing.T) {
	r, work := listFixture(t)
	var bad string
	parse := func(ctx context.Context, path string) (Summary, error) {
		if filepath.Base(path) == "s4.jsonl" {
			bad = path
			panic("secret transcript text")
		}
		return ParseSummary(ctx, path)
	}
	res, err := ListWith(context.Background(), DirectWalk(r), work, parse)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Unreadable) != 1 || res.Unreadable[0] != bad || res.Scanned != res.Of || res.Partial {
		t.Fatalf("unreadable %v, scanned %d of %d, partial %v", res.Unreadable, res.Scanned, res.Of, res.Partial)
	}
	for _, row := range res.Rows {
		if row.ID == "s4" {
			t.Error("the unreadable session is listed")
		}
	}
	if len(res.Rows) != 5 {
		t.Errorf("%d rows, want the 5 others", len(res.Rows))
	}
}

// The error for an unreadable transcript quotes its path: a name that holds
// an escape or a newline reaches a terminal or a log as text.
func TestUnreadableErrorQuotesThePath(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "s\x1b]0;title\a\nfake line.jsonl")
	defer SetParserForTest(func(path string) (*Session, error) {
		panic("secret transcript text")
	})()
	_, err := ParseFile(bad)
	if !errors.Is(err, ErrUnreadable) {
		t.Fatalf("ParseFile: %v", err)
	}
	msg := err.Error()
	if strings.ContainsAny(msg, "\x1b\a\n") || strings.Contains(msg, "secret") || !strings.Contains(msg, fmt.Sprintf("%q", bad)) {
		t.Errorf("Error() = %q", msg)
	}
}

// A parser that records another path than the one it was given does not
// take List down: List finds the session by the path the parse recorded.
func TestListUsesThePathTheParseRecorded(t *testing.T) {
	r, work := listFixture(t)
	defer SetParserForTest(func(path string) (*Session, error) {
		s, err := parseByPath(path)
		if s != nil {
			s.Path += ".moved"
		}
		return s, err
	})()
	list, unreadable, err := List(r, work)
	if err != nil || len(unreadable) != 0 {
		t.Fatal(err, unreadable)
	}
	if len(list) != 5 {
		t.Errorf("%d sessions listed, want 5", len(list))
	}
	for _, s := range list {
		if !strings.HasSuffix(s.Path, ".moved") {
			t.Errorf("%s/%s listed under %s", s.Provider, s.ID, s.Path)
		}
	}
}

// A parse out of time counts in Of and makes the list partial; discovery
// itself stops once the context ends.
func TestListWithBudget(t *testing.T) {
	r, work := listFixture(t)
	parse := func(ctx context.Context, path string) (Summary, error) {
		if filepath.Base(path) == "rollout-a.jsonl" {
			return Summary{}, ErrBudget
		}
		return ParseSummary(ctx, path)
	}
	res, err := ListWith(context.Background(), DirectWalk(r), work, parse)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Partial || res.Of != 8 || res.Scanned != 7 {
		t.Errorf("scanned %d of %d, partial %v", res.Scanned, res.Of, res.Partial)
	}
	for _, row := range res.Rows {
		if row.ID == "c1" {
			t.Error("a session skipped for time is listed")
		}
	}

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	cands, err := DirectWalk(r).Candidates(ctx, work)
	if !errors.Is(err, context.DeadlineExceeded) || len(cands) != 0 {
		t.Errorf("discovery past its deadline: %v, %v", cands, err)
	}
	res, err = ListWith(ctx, DirectWalk(r), work, ParseSummary)
	if err != nil || !res.Partial || res.Of != 0 || len(res.Rows) != 0 {
		t.Errorf("list past its deadline: %+v, %v", res, err)
	}
}

// withMaxLine lowers the line cap for one test.
func withMaxLine(t *testing.T, n int) {
	old := MaxLine
	MaxLine = n
	t.Cleanup(func() { MaxLine = old })
}

// A record over the cap is read past and counted where it came: before the
// first turn on the session, inside a turn on that turn. One the parser never
// reads, such as a file snapshot, is not counted; the rest parses as before.
func TestClaudeLineCap(t *testing.T) {
	withMaxLine(t, 100<<10)
	big := strings.Repeat("x", 200<<10)
	lines := claudeFixture("/w")
	c := `"cwd":"/w","sessionId":"s1"`
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	writeLines(t, path, lines...)
	want, err := ParseClaude(path)
	if err != nil {
		t.Fatal(err)
	}
	var capped []string
	capped = append(capped, `{"type":"user","isMeta":true,`+c+`,"timestamp":"2026-09-15T09:00:00Z","message":{"role":"user","content":"`+big+`"}}`)
	capped = append(capped, `{"type":"file-history-snapshot","snapshot":{"x":"`+big+`"}}`)
	capped = append(capped, lines[:8]...)
	capped = append(capped, `{"type":"user",`+c+`,"timestamp":"2026-09-15T10:01:07Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"`+big+`"}]}}`)
	capped = append(capped, lines[8:]...)
	writeLines(t, path, capped...)
	got, err := ParseClaude(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Skipped != 1 || len(got.Turns) != len(want.Turns) || got.Turns[0].Skipped != 1 || got.Turns[1].Skipped != 0 {
		t.Fatalf("skipped %d before the turns; turns %+v", got.Skipped, got.Turns)
	}
	got.Skipped, got.Turns[0].Skipped = 0, 0
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the rest of the parse changed:\n got %+v\nwant %+v", got, want)
	}
}

func TestCodexLineCap(t *testing.T) {
	withMaxLine(t, 100<<10)
	big := strings.Repeat("x", 200<<10)
	lines := codexFixture("/w", "c1")
	path := filepath.Join(t.TempDir(), "rollout-x.jsonl")
	writeLines(t, path, lines...)
	want, err := ParseCodex(path)
	if err != nil {
		t.Fatal(err)
	}
	var capped []string
	capped = append(capped, lines[:7]...)
	// Reasoning is read only for its time, so its length does not matter.
	capped = append(capped, `{"timestamp":"2026-09-15T21:13:24Z","type":"response_item","payload":{"type":"reasoning","content":"`+big+`"}}`)
	capped = append(capped, `{"timestamp":"2026-09-15T21:13:24Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"`+big+`"}]}}`)
	capped = append(capped, lines[7:]...)
	writeLines(t, path, capped...)
	got, err := ParseCodex(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Skipped != 0 || len(got.Turns) != 2 || got.Turns[0].Skipped != 1 || got.Turns[1].Skipped != 0 {
		t.Fatalf("skipped %d before the turns; turns %+v", got.Skipped, got.Turns)
	}
	got.Turns[0].Skipped = 0
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the rest of the parse changed:\n got %+v\nwant %+v", got, want)
	}
}

func TestAgyLineCap(t *testing.T) {
	withMaxLine(t, 100<<10)
	big := strings.Repeat("x", 200<<10)
	path := AgyTranscript(t.TempDir(), "a1")
	writeLines(t, path,
		`{"type":"GENERIC","created_at":"2026-09-04T17:57:00Z","content":"`+big+`"}`,
		`{"type":"USER_INPUT","created_at":"2026-09-04T17:57:50Z","content":"<USER_REQUEST>\nfix it\n</USER_REQUEST>"}`,
		`{"type":"PLANNER_RESPONSE","created_at":"2026-09-04T17:58:00Z","content":"`+big+`"}`,
		`{"type":"PLANNER_RESPONSE","created_at":"2026-09-04T17:58:10Z","content":"Fixed."}`,
	)
	s, err := ParseAgy(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Skipped != 1 || len(s.Turns) != 1 || s.Turns[0].Skipped != 1 || s.Turns[0].Reply != "Fixed." {
		t.Fatalf("skipped %d before the turns; turns %+v", s.Skipped, s.Turns)
	}
}
