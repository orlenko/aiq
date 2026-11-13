package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/transcript"
)

const fullIDFixtureID = "a1000000-1111-4222-8333-000000000001"

func fullIDWrite(t *testing.T, roots transcript.Roots, provider, id, dir string) string {
	t.Helper()
	var path string
	switch provider {
	case "claude":
		path = filepath.Join(roots.ClaudeProjects[0], claudeNonAlnum.ReplaceAllString(dir, "-"), id+".jsonl")
		c := &goldenClaude{cwd: dir, id: id, at: goldenAt(12, 9, 0, 0)}
		c.typed("fixture prompt")
		c.answer("fixture-message", "fixture answer")
		goldenWrite(t, path, c.lines...)
	case "codex":
		path = filepath.Join(roots.CodexSessions[0], "rollout-fixture-"+id+".jsonl")
		goldenWrite(t, path, goldenCodex(id, dir, "cli", goldenAt(12, 9, 0, 0), [2]string{"fixture prompt", "fixture answer"})...)
	case "agy":
		path = transcript.AgyTranscript(roots.AgyAppData[0], id)
		goldenWrite(t, path, `{"type":"USER_INPUT","created_at":"2026-11-12T09:00:00Z","content":"<USER_REQUEST>fixture prompt</USER_REQUEST>"}`, `{"type":"PLANNER_RESPONSE","created_at":"2026-11-12T09:01:00Z","content":"fixture answer"}`)
		goldenWrite(t, filepath.Join(roots.AgyAppData[0], "history.jsonl"), fmt.Sprintf(`{"workspace":%q,"conversationId":%q}`, dir, id))
	}
	return path
}

func fullIDRoots(t *testing.T) transcript.Roots {
	t.Helper()
	root := t.TempDir()
	return transcript.Roots{ClaudeProjects: []string{filepath.Join(root, "claude", "projects")}, CodexSessions: []string{filepath.Join(root, "codex", "sessions")}, AgyAppData: []string{filepath.Join(root, "agy")}}
}

type fullIDSource struct {
	transcript.Discovery
	byID  func(context.Context, string, string) ([]transcript.Candidate, error)
	calls []string
}

func (s *fullIDSource) ByID(ctx context.Context, provider, id string) ([]transcript.Candidate, error) {
	s.calls = append(s.calls, provider)
	if s.byID != nil {
		return s.byID(ctx, provider, id)
	}
	return s.Discovery.ByID(ctx, provider, id)
}

func fullIDResolver(roots transcript.Roots, src transcript.Discovery, warn io.Writer) *convoResolver {
	return &convoResolver{roots: roots, src: src, warn: warn, claude: func() []claudeLive { return nil }, now: func() time.Time { return goldenAt(13, 12, 0, 0) }}
}

func fullIDParse(path string) (*transcript.Session, error) {
	if filepath.Base(path) == "transcript.jsonl" {
		return transcript.ParseAgy(path)
	}
	if strings.HasPrefix(filepath.Base(path), "rollout-") {
		return transcript.ParseCodex(path)
	}
	return transcript.ParseClaude(path)
}

func TestConvoFullIDGrammarAndControls(t *testing.T) {
	for _, provider := range []string{"claude", "codex", "agy", "unknown"} {
		for _, id := range []string{fullIDFixtureID, strings.ToUpper(fullIDFixtureID), "short", fullIDFixtureID[:35], fullIDFixtureID + "x", "../" + fullIDFixtureID, fullIDFixtureID + "*"} {
			want := provider != "unknown" && (id == fullIDFixtureID || id == strings.ToUpper(fullIDFixtureID))
			if transcript.IsFullID(provider, id) != want {
				t.Fatalf("full-ID grammar mismatch for %q/%q", provider, id)
			}
		}
	}
	for _, b := range append([]byte{127}, []byte("\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f")...) {
		id := "fixture" + string(b)
		src := &fullIDSource{Discovery: transcript.DirectWalk(transcript.Roots{})}
		r := fullIDResolver(transcript.Roots{}, src, io.Discard)
		want := fmt.Sprintf("invalid session id %q: control characters are not allowed", id)
		if _, err := parseConvoArgs([]string{id}); err == nil || err.Error() != want {
			t.Fatalf("unquoted control selector %d", b)
		}
		if _, err := r.resolveID("/invented", id); err == nil || err.Error() != want || errors.Is(err, errSessionNoMatch) || len(src.calls) != 0 {
			t.Fatal("resolver did not reject the control selector before lookup")
		}
	}
}

func TestConvoFullIDLocalPriority(t *testing.T) {
	for _, mode := range []string{"exact", "prefix", "ambiguous", "short-miss"} {
		t.Run(mode, func(t *testing.T) {
			roots := fullIDRoots(t)
			dir := canonicalPath(t.TempDir())
			fullIDWrite(t, roots, "codex", fullIDFixtureID, canonicalPath(t.TempDir()))
			id := fullIDFixtureID
			switch mode {
			case "exact":
				fullIDWrite(t, roots, "claude", id, dir)
			case "prefix":
				fullIDWrite(t, roots, "claude", id+"-local", dir)
			case "ambiguous":
				fullIDWrite(t, roots, "claude", id+"-one", dir)
				fullIDWrite(t, roots, "claude", id+"-two", dir)
			case "short-miss":
				id = "absent-prefix"
			}
			src := &fullIDSource{Discovery: transcript.DirectWalk(roots)}
			r := fullIDResolver(roots, src, io.Discard)
			before, _ := os.Getwd()
			target, err := r.resolveID(dir, id)
			after, _ := os.Getwd()
			if before != after || len(src.calls) != 0 {
				t.Fatal("local outcome changed cwd or probed global roots")
			}
			switch mode {
			case "exact", "prefix":
				if err != nil || target.global || target.provider != "claude" || !strings.HasPrefix(target.id, fullIDFixtureID) {
					t.Fatal("readable local priority lost")
				}
			case "ambiguous":
				if err == nil || errors.Is(err, errSessionNoMatch) || err.Error() != fmt.Sprintf("%q matches 2 sessions; give more of the id", id) {
					t.Fatalf("local ambiguity: %v", err)
				}
			case "short-miss":
				if !errors.Is(err, errSessionNoMatch) || err.Error() != `no session "absent-prefix" in this directory (aiq resume --print --all lists them)` {
					t.Fatalf("local prefix error changed: %v", err)
				}
			}
		})
	}
}

func TestConvoFullIDGlobalProvidersAndProbeFailures(t *testing.T) {
	for _, provider := range []string{"claude", "codex", "agy"} {
		t.Run(provider, func(t *testing.T) {
			roots := fullIDRoots(t)
			path := fullIDWrite(t, roots, provider, fullIDFixtureID, canonicalPath(t.TempDir()))
			src := &fullIDSource{Discovery: transcript.DirectWalk(roots)}
			target, err := fullIDResolver(roots, src, io.Discard).resolveID(canonicalPath(t.TempDir()), fullIDFixtureID)
			if err != nil || !target.global || target.provider != provider || target.path != path || target.id != fullIDFixtureID || target.account != "" {
				t.Fatalf("global target: %+v %v", target, err)
			}
			if !reflect.DeepEqual(src.calls, []string{"claude", "codex", "agy"}) {
				t.Fatal("did not check every provider")
			}
			s, err := loadConvo(target)
			if err != nil || len(s.Turns) != 1 || s.Turns[0].Prompt != "fixture prompt" {
				t.Fatal("global target did not load")
			}
		})
	}
	roots := fullIDRoots(t)
	path := fullIDWrite(t, roots, "claude", fullIDFixtureID, "/invented/work")
	for _, mode := range []string{"collision", "before", "between", "after", "budget", "ordinary", "miss", "prefix"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cause := errors.New("fixture nested diagnostic\nnot for display")
			if mode == "before" {
				cancel()
			}
			src := &fullIDSource{byID: func(_ context.Context, p, _ string) ([]transcript.Candidate, error) {
				if mode == "between" && p == "codex" || mode == "after" && p == "agy" {
					cancel()
					return nil, transcript.ErrNoSuchSession
				}
				if p == "codex" && mode == "budget" {
					return nil, transcript.ErrBudget
				}
				if p == "codex" && mode == "ordinary" {
					return nil, cause
				}
				if mode != "miss" && (p == "claude" || mode == "collision" && p == "codex") {
					return []transcript.Candidate{{Provider: p, Path: path}}, nil
				}
				return nil, nil
			}}
			id := fullIDFixtureID
			if mode == "prefix" {
				id = "short"
			}
			_, err := resolveGlobalConvo(ctx, src, id, transcript.ParseSummary)
			want := transcript.ErrNoSuchSession
			switch mode {
			case "collision":
				want = errConvoAmbiguousID
			case "before", "between", "after":
				want = context.Canceled
			case "budget":
				want = transcript.ErrBudget
			case "ordinary":
				want = cause
			case "prefix":
				want = transcript.ErrBadID
			}
			if !errors.Is(err, want) || strings.Contains(err.Error(), "nested diagnostic") {
				t.Fatalf("probe %s: %v", mode, err)
			}
			if (mode == "before" || mode == "prefix") && len(src.calls) != 0 {
				t.Fatal("invalid/cancelled lookup reached discovery")
			}
		})
	}
}

func TestConvoFullIDUsesSharedCopyChooser(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	goldenWrite(t, a, "invented")
	goldenWrite(t, b, "invented")
	at := goldenAt(12, 9, 0, 0)
	src := &fullIDSource{byID: func(_ context.Context, p, _ string) ([]transcript.Candidate, error) {
		if p != "claude" {
			return nil, nil
		}
		return []transcript.Candidate{{Path: a, Root: "first"}, {Path: b, Root: "second", Account: "invented"}}, nil
	}}
	for _, mode := range []string{"newest", "updated-tie", "panic-tie", "ordinary-tie"} {
		if err := os.Chtimes(a, at, at); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(b, at, at); err != nil {
			t.Fatal(err)
		}
		if mode == "newest" {
			if err := os.Chtimes(b, at.Add(time.Minute), at.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
		}
		parse := func(_ context.Context, path string) (transcript.Summary, error) {
			if mode == "newest" {
				t.Error("unequal mtime unnecessarily parsed a tie")
			}
			if mode == "panic-tie" {
				panic("fixture tie payload")
			}
			if mode == "ordinary-tie" {
				return transcript.Summary{}, errors.New("fixture unreadable")
			}
			stamp := at
			if path == b {
				stamp = at.Add(time.Hour)
			}
			return transcript.Summary{Updated: stamp}, nil
		}
		target, err := resolveGlobalConvo(context.Background(), src, fullIDFixtureID, parse)
		want := b
		if mode == "panic-tie" || mode == "ordinary-tie" {
			want = a
		}
		if err != nil || target.path != want {
			t.Fatalf("shared copy selection %s: %v", mode, err)
		}
	}
}

func fullIDWarningFixture(selected string) ([]string, string, string) {
	paths := []string{selected}
	for i := 0; i < 12; i++ {
		paths = append(paths, fmt.Sprintf("/warnings/%02d.jsonl", i))
	}
	paths = append(paths, selected)
	full := fmt.Sprintf("aiq: could not read transcript %q\n", selected)
	filtered := ""
	for i := 0; i < 10; i++ {
		line := fmt.Sprintf("aiq: could not read transcript %q\n", fmt.Sprintf("/warnings/%02d.jsonl", i))
		filtered += line
		if i < 9 {
			full += line
		}
	}
	return paths, full + "aiq: …and 4 more could not be read\n", filtered + "aiq: …and 2 more could not be read\n"
}

func fullIDGood(path string) *transcript.Session {
	return &transcript.Session{Provider: "claude", ID: fullIDFixtureID, Path: path, Turns: []transcript.Turn{{Prompt: "valid prompt", Reply: "valid answer"}}}
}

func fullIDFailure(mode, path string) (*transcript.Session, error, error) {
	s := fullIDGood(path)
	s.Turns[0].Reply = "FAILED GRAPH"
	switch mode {
	case "panic":
		panic("fixture panic payload")
	case "nil":
		return nil, nil, errConvoIdentity
	case "provider":
		s.Provider = "codex"
	case "id":
		s.ID = "a1000000-1111-4222-8333-000000000002"
	case "ordinary":
		return s, &os.PathError{Op: "fixture", Path: "nested\nfixture payload", Err: os.ErrPermission}, os.ErrPermission
	}
	return s, nil, errConvoIdentity
}

func TestConvoFullIDLoadAndWarnings(t *testing.T) {
	for _, mode := range []string{"success", "panic", "nil", "provider", "id", "ordinary"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "quoted\n\x1b-fixture.jsonl")
			paths, _, filtered := fullIDWarningFixture(path)
			before := append([]string(nil), paths...)
			var warnings bytes.Buffer
			target := convoTarget{provider: "claude", path: path, id: fullIDFixtureID, global: true, warnings: &convoWarnings{paths: paths, selected: path, warn: &warnings}}
			restore := transcript.SetParserForTest(func(string) (*transcript.Session, error) {
				if mode == "success" {
					return fullIDGood(path), nil
				}
				s, e, _ := fullIDFailure(mode, path)
				return s, e
			})
			defer restore()
			loaded, err := loadConvo(target)
			if mode == "success" {
				if err != nil || loaded == nil {
					t.Fatal("successful reread failed")
				}
			} else {
				cause := errConvoIdentity
				if mode == "panic" {
					cause = transcript.ErrUnreadable
				}
				if mode == "ordinary" {
					cause = os.ErrPermission
				}
				var typed *convoLoadError
				var exit *exitError
				if loaded != nil || !errors.Is(err, cause) || !errors.As(err, &typed) || errors.As(err, &exit) || err.Error() != fmt.Sprintf("could not read transcript %q", path) {
					t.Fatalf("unsafe/unclassified load error: %v", err)
				}
			}
			if warnings.String() != filtered || !reflect.DeepEqual(paths, before) {
				t.Fatal("selected filtering changed order, cap or input")
			}
			_, _ = loadConvo(target)
			copy := target
			copy.flushWarnings(false)
			if warnings.String() != filtered || target.warnings.paths != nil || target.warnings.warn != nil || target.warnings.selected != "" {
				t.Fatal("copied target replayed or retained warning record")
			}
		})
	}
	path := "/invented/settled.jsonl"
	source := fullIDGood(path)
	source.Turns[0] = transcript.Turn{Open: true, Earlier: []string{"old", "latest"}}
	restore := transcript.SetParserForTest(func(string) (*transcript.Session, error) { return source, nil })
	defer restore()
	s, err := loadConvo(convoTarget{global: true, provider: "claude", id: fullIDFixtureID, path: path, stale: true})
	if err != nil || s.Turns[0].Open || s.Turns[0].Reply != "latest" || !reflect.DeepEqual(s.Turns[0].Earlier, []string{"old"}) {
		t.Fatal("global load lost existing stale settlement")
	}
}

func TestConvoFullIDWarningsNoLoadAndOnceRace(t *testing.T) {
	for _, screen := range []bool{false, true} {
		paths, full, _ := fullIDWarningFixture("/invented/selected.jsonl")
		var warnings, output bytes.Buffer
		target := convoTarget{provider: "claude", id: fullIDFixtureID, path: paths[0], global: true, warnings: &convoWarnings{paths: paths, selected: paths[0], warn: &warnings}}
		var calls atomic.Int32
		restore := transcript.SetParserForTest(func(string) (*transcript.Session, error) { calls.Add(1); return nil, nil })
		stop := make(chan struct{})
		close(stop)
		f := &convoFollow{tgt: target, w: &output}
		var err error
		if screen {
			err = f.screen(nil, stop)
		} else {
			err = f.loop(stop)
		}
		restore()
		if err != nil || calls.Load() != 0 || warnings.Len() != 0 || output.Len() != 0 {
			t.Fatal("preclosed stop performed a global load/output")
		}
		target.flushWarnings(false)
		target.flushWarnings(true)
		if warnings.String() != full {
			t.Fatal("no-load exit lost full warning report")
		}
	}
	paths, full, filtered := fullIDWarningFixture("/invented/selected.jsonl")
	before := append([]string(nil), paths...)
	var warnings bytes.Buffer
	target := convoTarget{warnings: &convoWarnings{paths: paths, selected: paths[0], warn: &warnings}}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; copy := target; copy.flushWarnings(i%2 == 0) }()
	}
	close(start)
	wg.Wait()
	if warnings.String() != full && warnings.String() != filtered || !reflect.DeepEqual(paths, before) {
		t.Fatal("concurrent warning ownership was not once-only/nonmutating")
	}
}

type fullIDWriter struct {
	bytes.Buffer
	after func()
}

func (w *fullIDWriter) Write(p []byte) (int, error) {
	n, _ := w.Buffer.Write(p)
	if w.after != nil {
		fn := w.after
		w.after = nil
		fn()
	}
	return n, nil
}

func TestConvoFullIDFatalStreamReload(t *testing.T) {
	for _, mode := range []string{"panic", "nil", "provider", "id", "ordinary"} {
		for _, tty := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s-tty%v", mode, tty), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "fixture.jsonl")
				goldenWrite(t, path, "first")
				var calls atomic.Int32
				restore := transcript.SetParserForTest(func(string) (*transcript.Session, error) {
					if calls.Add(1) == 1 {
						return fullIDGood(path), nil
					}
					s, e, _ := fullIDFailure(mode, path)
					return s, e
				})
				defer restore()
				w := &fullIDWriter{}
				f := &convoFollow{tgt: convoTarget{global: true, provider: "claude", id: fullIDFixtureID, path: path}, w: w, tty: tty, statEvery: time.Millisecond, parseGap: time.Millisecond, quiet: time.Millisecond}
				w.after = func() {
					if tty {
						f.setStatus("fixture running")
					}
					if err := os.WriteFile(path, []byte("changed larger fixture"), 0o600); err != nil {
						t.Error(err)
					}
				}
				stop := make(chan struct{})
				defer close(stop)
				done := make(chan error, 1)
				go func() { done <- f.loop(stop) }()
				var err error
				select {
				case err = <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("fatal reload did not terminate")
				}
				cause := errConvoIdentity
				if mode == "panic" {
					cause = transcript.ErrUnreadable
				}
				if mode == "ordinary" {
					cause = os.ErrPermission
				}
				if !errors.Is(err, cause) || calls.Load() != 2 || !strings.Contains(w.String(), "valid prompt") || strings.Contains(w.String(), "FAILED GRAPH") {
					t.Fatal("failed reload published/retried data or lost classification")
				}
				if tty {
					if f.status != "" || !strings.HasSuffix(w.String(), "\r\x1b[K") {
						t.Fatal("TTY status was not erased before returning error")
					}
				} else if strings.Contains(w.String(), "\x1b") {
					t.Fatal("non-TTY acquired terminal escapes")
				}
				fmt.Fprintf(w, "aiq: %s\n", errorText(err))
				if strings.Count(w.String(), "could not read transcript") != 1 {
					t.Fatal("selected error was not one final diagnostic")
				}
			})
		}
	}
}

type fullIDCaptureTerm struct {
	*fakeConvoTerm
	captureMu sync.Mutex
	output    strings.Builder
	painted   chan struct{}
	once      sync.Once
}

func (t *fullIDCaptureTerm) write(s string) error {
	err := t.fakeConvoTerm.write(s)
	t.captureMu.Lock()
	defer t.captureMu.Unlock()
	t.output.WriteString(s)
	if strings.Contains(t.output.String(), "valid prompt") {
		t.once.Do(func() { close(t.painted) })
	}
	return err
}
func (t *fullIDCaptureTerm) captured() string {
	t.captureMu.Lock()
	defer t.captureMu.Unlock()
	return t.output.String()
}

func TestConvoFullIDFatalScreenHandoff(t *testing.T) {
	noExit(t)
	for _, mode := range []string{"panic", "nil", "provider", "id", "ordinary"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fixture.jsonl")
			goldenWrite(t, path, "first")
			var calls atomic.Int32
			restore := transcript.SetParserForTest(func(string) (*transcript.Session, error) {
				if calls.Add(1) == 1 {
					s := fullIDGood(path)
					s.Turns[0].Open = true
					return s, nil
				}
				s, e, _ := fullIDFailure(mode, path)
				return s, e
			})
			defer restore()
			updates := make(chan convoUpdate)
			failed := make(chan error, 1)
			stop := make(chan struct{})
			defer close(stop)
			f := &convoFollow{tgt: convoTarget{global: true, provider: "claude", id: fullIDFixtureID, path: path}, tty: true, statEvery: time.Millisecond, parseGap: time.Millisecond, quiet: time.Millisecond, status: "fixture screen status"}
			var emptyStatus atomic.Int32
			f.send = func(u convoUpdate) {
				if u.s == nil && u.status == "" {
					emptyStatus.Add(1)
				}
				select {
				case updates <- u:
				case <-stop:
				}
			}
			go func() { failed <- f.loop(stop) }()
			var initial convoUpdate
			select {
			case initial = <-updates:
			case <-time.After(3 * time.Second):
				t.Fatal("no initial screen handoff")
			}
			screen := newConvoScreen(true)
			screen.apply(initial)
			term := &fullIDCaptureTerm{fakeConvoTerm: newFakeConvoTerm(), painted: make(chan struct{})}
			done := make(chan error, 1)
			go func() { done <- runConvoScreen(screen, term, updates, failed, stop) }()
			select {
			case <-term.painted:
			case <-time.After(3 * time.Second):
				t.Fatal("initial valid screen was not painted")
			}
			if err := os.WriteFile(path, []byte("changed larger fixture"), 0o600); err != nil {
				t.Fatal(err)
			}
			err := waitRun(t, done, 3*time.Second)
			cause := errConvoIdentity
			if mode == "panic" {
				cause = transcript.ErrUnreadable
			}
			if mode == "ordinary" {
				cause = os.ErrPermission
			}
			if !errors.Is(err, cause) || calls.Load() != 2 || emptyStatus.Load() != 0 || strings.Contains(term.captured(), "FAILED GRAPH") {
				t.Fatal("screen failure handoff published/retried/blocked")
			}
			if !reflect.DeepEqual(tail(term.log(), 4), afterRun) {
				t.Fatal("screen did not restore input and terminal before error return")
			}
		})
	}
}

func TestConvoFullIDLegacyReloadContinues(t *testing.T) {
	roots := fullIDRoots(t)
	path := fullIDWrite(t, roots, "claude", fullIDFixtureID, "/invented/work")
	stop := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(stop) }) }
	defer finish()
	events := make(chan convoUpdate, 8)
	done := make(chan error, 1)
	f := &convoFollow{tgt: convoTarget{provider: "claude", path: path}, statEvery: time.Millisecond, parseGap: time.Millisecond, quiet: time.Millisecond, status: "fixture existing status"}
	f.send = func(u convoUpdate) {
		if u.begin {
			os.Remove(path)
		}
		events <- u
	}
	go func() { done <- f.loop(stop) }()
	select {
	case <-events:
	case <-time.After(3 * time.Second):
		t.Fatal("no initial legacy data")
	}
	select {
	case err := <-done:
		t.Fatalf("legacy reload unexpectedly fatal: %v", err)
	case u := <-events:
		if u.s != nil || u.status != "" {
			t.Fatal("legacy failed reload published a graph")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("legacy loop did not pass failed reload")
	}
	finish()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("legacy loop did not stop")
	}
}

func TestConvoFullIDSuccessFollowStaysOnSelectedPath(t *testing.T) {
	roots := fullIDRoots(t)
	work := canonicalPath(t.TempDir())
	path := fullIDWrite(t, roots, "claude", fullIDFixtureID, work)
	otherRoots := fullIDRoots(t)
	other := fullIDWrite(t, otherRoots, "claude", fullIDFixtureID, work)
	roots.ClaudeProjects = append(roots.ClaudeProjects, otherRoots.ClaudeProjects[0])
	at := goldenAt(12, 12, 0, 0)
	for p, stamp := range map[string]time.Time{path: at, other: at.Add(-time.Hour)} {
		if err := os.Chtimes(p, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	src := &fullIDSource{Discovery: transcript.DirectWalk(roots)}
	r := fullIDResolver(roots, src, io.Discard)
	target, err := r.resolveID(canonicalPath(t.TempDir()), fullIDFixtureID)
	if err != nil || !target.global || target.path != path {
		t.Fatal("initial global selection failed")
	}
	stop := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(stop) }) }
	defer finish()
	updates, done := make(chan convoUpdate, 8), make(chan error, 1)
	f := &convoFollow{r: r, tgt: target, statEvery: time.Millisecond, parseGap: time.Millisecond, resolveEvery: time.Nanosecond, quiet: time.Millisecond}
	f.send = func(u convoUpdate) {
		select {
		case updates <- u:
		case <-stop:
		}
	}
	go func() { done <- f.loop(stop) }()
	waitPrompt := func(want string) {
		t.Helper()
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		for {
			select {
			case u := <-updates:
				if u.s != nil && len(u.s.Turns) > 0 && u.s.Turns[len(u.s.Turns)-1].Prompt == want {
					if u.t.path != path || !u.t.global {
						t.Fatal("follow retargeted roots")
					}
					return
				}
			case err := <-done:
				t.Fatalf("follow ended early: %v", err)
			case <-timer.C:
				t.Fatalf("missing follow update %q", want)
			}
		}
	}
	waitPrompt("fixture prompt")
	if err := os.Chtimes(other, at.Add(time.Hour), at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	growth := &goldenClaude{cwd: work, id: fullIDFixtureID, at: goldenAt(12, 13, 0, 0)}
	growth.typed("grown prompt")
	growth.answer("growth", "grown answer")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.WriteString(strings.Join(growth.lines, "\n") + "\n")
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatal("could not append follow fixture")
	}
	waitPrompt("grown prompt")
	replacement := &goldenClaude{cwd: work, id: fullIDFixtureID, at: goldenAt(12, 14, 0, 0)}
	replacement.typed("replacement prompt")
	replacement.answer("replacement", "replacement answer")
	goldenWrite(t, path+".replacement", replacement.lines...)
	if err := os.Rename(path+".replacement", path); err != nil {
		t.Fatal(err)
	}
	waitPrompt("replacement prompt")
	finish()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("follow did not stop")
	}
	if !reflect.DeepEqual(src.calls, []string{"claude", "codex", "agy"}) {
		t.Fatal("explicit-ID follow repeated global lookup")
	}
}

// Actual main error printing is tested in an isolated child, not reimplemented
// as the command oracle. Every root and selector comes from invented fixtures.
func TestConvoFullIDCommandChild(t *testing.T) {
	mode := os.Getenv("AIQ_TEST_CONVO_FULL_CHILD")
	if mode == "" {
		return
	}
	path := os.Getenv("AIQ_TEST_CONVO_FULL_PATH")
	var calls int
	if mode != "success" {
		transcript.SetParserForTest(func(p string) (*transcript.Session, error) {
			if p == path {
				calls++
				if mode == "agy-follow" && calls > 1 {
					os.WriteFile(os.Getenv("AIQ_TEST_CONVO_FULL_MARKER"), []byte("attempted"), 0o600)
				}
				panic("fixture panic payload")
			}
			return fullIDParse(p)
		})
	}
	before, _ := os.Getwd()
	os.Args = []string{"aiq", "convo", fullIDFixtureID}
	if mode == "agy-follow" {
		os.Args = append(os.Args, "--follow")
	}
	main()
	after, _ := os.Getwd()
	if before != after {
		t.Fatal("read command changed cwd")
	}
}

func TestConvoFullIDActualCommand(t *testing.T) {
	for _, provider := range []string{"claude", "codex", "agy"} {
		for _, mode := range []string{"success", "panic", "agy-follow"} {
			if mode == "agy-follow" && provider != "agy" {
				continue
			}
			t.Run(provider+"/"+mode, func(t *testing.T) {
				work, _ := listHome(t)
				path := fullIDWrite(t, defaultRoots(), provider, fullIDFixtureID, work)
				self, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, self, "-test.run=^TestConvoFullIDCommandChild$", "-test.count=1")
				cmd.WaitDelay = 2 * time.Second
				cmd.Dir = t.TempDir()
				if mode != "success" {
					cmd.Dir = work
				}
				marker := filepath.Join(t.TempDir(), "unexpected-load")
				cmd.Env = append(os.Environ(), "AIQ_TEST_CONVO_FULL_CHILD="+mode, "AIQ_TEST_CONVO_FULL_PATH="+path, "AIQ_TEST_CONVO_FULL_MARKER="+marker)
				out, err := cmd.CombinedOutput()
				if mode == "success" {
					if err != nil || !bytes.Contains(out, []byte("fixture prompt")) || !bytes.Contains(out, []byte("fixture answer")) {
						t.Fatalf("command snapshot failed: %v\n%s", err, out)
					}
				} else if mode == "panic" {
					want := fmt.Sprintf("aiq: could not read transcript %q\n", path)
					if err == nil || string(out) != want {
						t.Fatalf("actual main selected diagnostic: %v\n%q want %q", err, out, want)
					}
				} else {
					if err == nil || strings.Count(string(out), "could not read transcript") != 1 || !bytes.Contains(out, []byte("cannot follow Antigravity")) || bytes.Contains(out, []byte("fixture prompt")) {
						t.Fatalf("AGY no-load refusal: %v\n%s", err, out)
					}
					if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("AGY refusal performed selected-file load")
					}
				}
			})
		}
	}
}
