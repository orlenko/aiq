package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/asks"
	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/state"
)

func asksTestStores(t *testing.T) (*state.Store, *state.Store) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("AIQ_DATA_DIR", root)
	t.Setenv("AIQ_CONFIG", filepath.Join(root, "config.toml"))
	reg, err := state.Open(filepath.Join(root, "asks.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	if err := reg.EnsureAsks(); err != nil {
		t.Fatal(err)
	}
	leasePath := filepath.Join(root, "leases.db")
	writable, err := state.Open(leasePath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = writable.AddLease(state.Lease{AccountID: "claude/example", Provider: "claude", Mode: state.ModeLong,
		Workspace: "/example/project", Cwd: "/example/project", PID: os.Getpid(), Hostname: pool.Hostname(), Pane: "%123", StartedAt: time.Now().Unix()})
	writable.Close()
	if err != nil {
		t.Fatal(err)
	}
	leases, err := state.OpenReadOnly(leasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { leases.Close() })
	return reg, leases
}

func asksSeed(t *testing.T, reg *state.Store, items ...state.Ask) {
	t.Helper()
	s, _, err := reg.UpsertAskSession(state.AskSession{Provider: "claude", SessionID: "example-session", Chain: "example"})
	if err != nil {
		t.Fatal(err)
	}
	to := s.Position
	to.ItemsDone++
	if err := reg.ApplyAskPlan(s.Provider, s.SessionID, s.Position, to, 0, state.AskPlan{Inserts: items}); err != nil {
		t.Fatal(err)
	}
}

func TestAsksInDir(t *testing.T) {
	for _, tc := range []struct {
		ws, dir string
		want    bool
	}{
		{"/w/alpha", "/w/alpha", true},
		{"/w/alpha", "/w/alpha/sub", true},
		{"/w/alpha", "/w", true},
		{"/w/alpha", "/w/alpha2", false},
		{"/w/alpha2", "/w/alpha", false},
		{"", "/w", false},
		{"/w", "", false},
		{"/w/alpha", "/", true},
		{"/", "/w/alpha", true},
	} {
		if got := asksInDir(tc.ws, tc.dir, asksRealPath); got != tc.want {
			t.Errorf("asksInDir(%q, %q) = %v", tc.ws, tc.dir, got)
		}
	}
}

func TestAsksGroupSections(t *testing.T) {
	now := time.Now().Unix()
	for _, tc := range []struct {
		a    state.Ask
		want string
	}{
		{state.Ask{RaisedAt: now - 48*3600, AsksReader: .6}, "Asks"},
		{state.Ask{RaisedAt: now - 48*3600 - 1, AsksReader: .6}, "Other open"},
		{state.Ask{RaisedAt: now, AsksReader: .599, Risk: .8}, "Warnings"},
		{state.Ask{RaisedAt: now, AsksReader: .6, Risk: .8}, "Asks"},
		{state.Ask{RaisedAt: now, AsksReader: .2}, "Other open"},
	} {
		if got := asksGroup(tc.a, now); got != tc.want {
			t.Errorf("%+v grouped as %s, want %s", tc.a, got, tc.want)
		}
	}
}

func TestAsksListRender(t *testing.T) {
	now := time.Now()
	st := state.AsksStatus{PassAt: now.Unix(), Watched: 2}
	items := []state.Ask{
		{ID: 1, State: state.AskOpen, Workspace: "/w/alpha", Provider: "claude", RaisedAt: now.Unix(), AsksReader: .9, FirstQuote: "Merge the example?\x1b[31m", Repeats: 1},
		{ID: 2, State: state.AskOpen, Workspace: "/w/beta", Provider: "codex", RaisedAt: now.Unix(), Risk: .9, FirstQuote: "The example key is exposed."},
		{ID: 3, State: state.AskProbablyDone, Workspace: "/w/alpha", Provider: "claude", RaisedAt: now.Unix(), FirstQuote: "Run the example check.", StateReason: "reply"},
		{ID: 4, State: state.AskDismissed, Workspace: "/w/alpha", Provider: "claude", RaisedAt: now.Unix(), FirstQuote: "Old example."},
	}
	out := renderAsksList(items, nil, st, true, false, now, asksCmd)
	if strings.Contains(out, "\x1b") {
		t.Fatalf("escape reached the list: %q", out)
	}
	asksAt, warnAt := strings.Index(out, "\nAsks\n"), strings.Index(out, "\nWarnings\n")
	if asksAt < 0 || warnAt < asksAt || !strings.Contains(out, "asked 2x") {
		t.Fatalf("sections or repeat count missing:\n%s", out)
	}
	if strings.Contains(out, "Run the example check") || !strings.Contains(out, "1 probably done, 1 closed or dismissed") {
		t.Fatalf("settled items should only be counted without --all:\n%s", out)
	}
	all := renderAsksList(items, nil, st, true, true, now, asksCmd)
	if !strings.Contains(all, "\nProbably done\n") || !strings.Contains(all, "(reply)") || !strings.Contains(all, "Old example.") {
		t.Fatalf("--all should show settled items:\n%s", all)
	}
}

func TestAsksStatusLineSaysWhetherToTrustTheList(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		st    state.AsksStatus
		keyOK bool
		want  string
	}{
		{state.AsksStatus{PassAt: now.Unix()}, false, "Off: no key"},
		{state.AsksStatus{}, true, "Not watching."},
		{state.AsksStatus{PassAt: now.Add(-asksStaleAfter - time.Minute).Unix()}, true, "Not watching since"},
		{state.AsksStatus{PassAt: now.Unix(), LastError: "example", ErrorAt: now.Unix()}, true, "Failing since"},
		// In its back-off a failing watcher is still failing, not gone.
		{state.AsksStatus{PassAt: now.Add(-9 * time.Minute).Unix(), LastError: "example", ErrorAt: now.Add(-9 * time.Minute).Unix(),
			NextTryAt: now.Add(6 * time.Minute).Unix()}, true, "Failing since"},
		{state.AsksStatus{PassAt: now.Unix(), Waiting: 3}, true, "Reading: 3 texts waiting"},
		{state.AsksStatus{PassAt: now.Unix(), Watched: 1}, true, "Nothing open. Watching 1"},
	} {
		if got := asksStatusLine(tc.st, tc.keyOK, 0, now, asksCmd); !strings.HasPrefix(got, tc.want) {
			t.Errorf("%+v: %q, want prefix %q", tc.st, got, tc.want)
		}
	}
}

func TestAsksResetKeepsOtherDirectories(t *testing.T) {
	reg, _ := asksTestStores(t)
	now := time.Now()
	asksSeed(t, reg,
		state.Ask{Chain: "example", Workspace: "/w/alpha", Provider: "claude", SessionID: "example-session", RaisedAt: now.Unix(), AsksReader: .9, FirstQuote: "Alpha example?"},
		state.Ask{Chain: "example", Workspace: "/w/beta", Provider: "claude", SessionID: "example-session", RaisedAt: now.Unix(), AsksReader: .9, FirstQuote: "Beta example?"})
	n, err := asksReset(reg, "/w/alpha", now)
	if err != nil || n != 1 {
		t.Fatalf("reset %d, %v", n, err)
	}
	left, err := reg.ListAsks("")
	if err != nil || len(left) != 1 || left[0].Workspace != "/w/beta" {
		t.Fatalf("left %+v, %v", left, err)
	}
	if n, err := asksReset(reg, "", now); err != nil || n != 1 {
		t.Fatalf("reset all %d, %v", n, err)
	}
}

type asksStubModel struct{ calls int }

func (m *asksStubModel) Ask(context.Context, string, map[string]asks.Question) (map[string]float64, error) {
	m.calls++
	return map[string]float64{}, nil
}

// The key counts only as a regular file no one else can read, and a request goes out only
// while it is still the key the watcher started with.
func TestAsksKeyFileAndGuard(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AIQ_CONFIG", filepath.Join(root, "config.toml"))
	path := asksKeyPath()
	if err := os.WriteFile(path, []byte("example-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for mode, want := range map[os.FileMode]string{0o600: "example-key", 0o400: "example-key", 0o640: "", 0o604: "", 0o200: ""} {
		os.Chmod(path, mode)
		if got := asksKey(); got != want {
			t.Errorf("mode %o: key %q, want %q", mode, got, want)
		}
	}
	os.Chmod(path, 0o600)
	inner := &asksStubModel{}
	g := &asksKeyGuard{inner: inner, key: "example-key"}
	if _, err := g.Ask(context.Background(), "x", nil); err != nil || inner.calls != 1 {
		t.Fatalf("%v, %d calls", err, inner.calls)
	}
	os.Chmod(path, 0o644)
	if _, err := g.Ask(context.Background(), "x", nil); err == nil || inner.calls != 1 {
		t.Fatalf("a request went out with the key file open: %v, %d calls", err, inner.calls)
	}
	os.Chmod(path, 0o600)
	os.WriteFile(path, []byte("other-key\n"), 0o600)
	if _, err := g.Ask(context.Background(), "x", nil); err == nil || inner.calls != 1 {
		t.Fatalf("a request went out after the key changed: %v", err)
	}
}

// With the current directory gone, a directory-scoped command fails instead of covering
// every directory.
func TestAsksHereDirFailsClosed(t *testing.T) {
	back, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(back) })
	gone := filepath.Join(t.TempDir(), "gone")
	os.Mkdir(gone, 0o700)
	if err := os.Chdir(gone); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PWD", gone)
	os.Remove(gone)
	if d, err := asksHereDir(); err == nil {
		t.Fatalf("got %q with the directory gone", d)
	}
}

// off and on cover every name a lease could give the workspace.
func TestAsksWorkspaceKeysResolveLinks(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(root, "real")
	os.Mkdir(real, 0o700)
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	names, err := asksWorkspaceKeys(link)
	if err != nil || !containsString(names, link) || !containsString(names, real) {
		t.Fatalf("%q, %v", names, err)
	}
	if _, err := asksWorkspaceKeys(filepath.Join(root, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a missing path: %v", err)
	}
}

// A directory named through a link matches a workspace recorded by its real path.
func TestAsksInDirThroughALink(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(root, "real")
	os.MkdirAll(filepath.Join(real, "sub"), 0o700)
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if !asksInDir(real, link, asksRealPath) || !asksInDir(real, filepath.Join(link, "sub"), asksRealPath) || !asksInDir(link, real, asksRealPath) {
		t.Fatal("a link and its target must match")
	}
	if asksInDir(filepath.Join(root, "other"), link, asksRealPath) {
		t.Fatal("an unrelated directory matched")
	}
}

// A watcher backing off after a plain failure reads as failing, with its next try, rather
// than as no watcher once the last pass is old.
func TestAsksBackoffReadsAsFailing(t *testing.T) {
	reg, _ := asksTestStores(t)
	now := time.Now()
	if err := reg.SetAsksStatus(state.AsksStatus{PassAt: now.Add(-6 * time.Minute).Unix(), ErrorAt: now.Add(-20 * time.Minute).Unix(), LastError: "service unreachable"}); err != nil {
		t.Fatal(err)
	}
	asksNoteNextTry(reg, now.Add(10*time.Minute))
	st, err := reg.GetAsksStatus(now)
	if err != nil {
		t.Fatal(err)
	}
	if line := asksStatusLine(st, true, 0, now, asksCmd); !strings.HasPrefix(line, "Failing since") || !strings.Contains(line, "Next try") {
		t.Fatal(line)
	}
	if err := reg.SetAsksStatus(state.AsksStatus{PassAt: now.Unix()}); err != nil {
		t.Fatal(err)
	}
	asksNoteNextTry(reg, now.Add(10*time.Minute))
	if st, _ := reg.GetAsksStatus(now); st.NextTryAt != 0 {
		t.Fatal("a healthy status got a next try")
	}
}

// A reset holds sessions the watcher has not seen yet in that directory to the reset too.
func TestAsksResetFencesUnseenSessions(t *testing.T) {
	reg, _ := asksTestStores(t)
	now := time.Now()
	if _, err := asksReset(reg, "/example/project", now); err != nil {
		t.Fatal(err)
	}
	row, _, err := reg.UpsertAskSession(state.AskSession{Provider: "claude", SessionID: "later", Chain: "/example/project/sub", Workspace: "/example/project/sub", Transcript: "/t", NotBefore: 1})
	if err != nil || row.NotBefore != now.UnixMilli() {
		t.Fatalf("%d %v", row.NotBefore, err)
	}
	row, _, _ = reg.UpsertAskSession(state.AskSession{Provider: "claude", SessionID: "elsewhere", Chain: "/example/other", Workspace: "/example/other", Transcript: "/t", NotBefore: 1})
	if row.NotBefore != 1 {
		t.Fatalf("another workspace was fenced: %d", row.NotBefore)
	}
	// A reset from inside a checkout covers the checkout root holding it, not its neighbours.
	if _, err := asksReset(reg, "/example/repo/sub", now); err != nil {
		t.Fatal(err)
	}
	row, _, _ = reg.UpsertAskSession(state.AskSession{Provider: "claude", SessionID: "root", Chain: "/example/repo", Workspace: "/example/repo", Transcript: "/t", NotBefore: 1})
	if row.NotBefore != now.UnixMilli() {
		t.Fatalf("the checkout root was not fenced: %d", row.NotBefore)
	}
	row, _, _ = reg.UpsertAskSession(state.AskSession{Provider: "claude", SessionID: "next", Chain: "/example/repo/other", Workspace: "/example/repo/other", Transcript: "/t", NotBefore: 1})
	if row.NotBefore != 1 {
		t.Fatalf("a neighbour was fenced: %d", row.NotBefore)
	}
	// A workspace already known that holds the directory is fenced alone, not the rest of it.
	reg.UpsertAskSession(state.AskSession{Provider: "claude", SessionID: "known", Chain: "/example/known", Workspace: "/example/known", Transcript: "/t", NotBefore: 1})
	if _, err := asksReset(reg, "/example/known/sub", now); err != nil {
		t.Fatal(err)
	}
	row, _, _ = reg.UpsertAskSession(state.AskSession{Provider: "claude", SessionID: "sibling", Chain: "/example/known/other", Workspace: "/example/known/other", Transcript: "/t", NotBefore: 1})
	if row.NotBefore != 1 {
		t.Fatalf("a sibling under a known workspace was fenced: %d", row.NotBefore)
	}
}

// A reset by a directory's own name holds a session first seen under a link to it.
func TestAsksResetFencesALinkSeenLater(t *testing.T) {
	reg, _ := asksTestStores(t)
	root := t.TempDir()
	real := filepath.Join(root, "real")
	link := filepath.Join(root, "link")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	real, _ = filepath.EvalSymlinks(real)
	now := time.Now()
	if _, err := asksReset(reg, real, now); err != nil {
		t.Fatal(err)
	}
	row, _, err := reg.UpsertAskSession(state.AskSession{Provider: "claude", SessionID: "via-link", Chain: link, Workspace: link, Transcript: "/t", NotBefore: 1})
	if err != nil || row.NotBefore != now.UnixMilli() {
		t.Fatalf("%d %v", row.NotBefore, err)
	}
}
