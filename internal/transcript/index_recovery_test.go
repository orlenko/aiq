package transcript

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// These regressions use only invented files; TestMain isolates AIQ_DATA_DIR.
// Compare discovery membership rather than ByID's account annotations.
func recoveryParity(t *testing.T, x *Index, r Roots, provider, dir string, count int) {
	t.Helper()
	ctx := context.Background()
	paths := func(src Discovery) []string {
		t.Helper()
		cands, err := src.Candidates(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, c := range cands {
			out = append(out, c.Provider+"/"+c.Path)
		}
		return out
	}
	got, want := paths(x), paths(DirectWalk(r))
	if len(got) != count || !reflect.DeepEqual(got, want) {
		t.Fatalf("Candidates: %v, want %v (%d)", got, want, count)
	}
	for _, since := range []time.Time{{}, t0, t0.Add(time.Hour)} {
		got := x.Written(ctx, provider, dir, since)
		want := DirectWalk(r).Written(ctx, provider, dir, since)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("Written since %v: %v, want %v", since, got, want)
		}
	}
}

func recoveryRefresh(t *testing.T, x *Index, clk *clock, dir string) {
	t.Helper()
	codex, agy := x.generation("codex"), x.generation("agy")
	clk.Add(DiscoveryTTL)
	if _, err := x.Candidates(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	waitWalk(t, x, int64(codex+1))
	deadline := time.Now().Add(3 * time.Second)
	for x.generation("agy") <= agy {
		if time.Now().After(deadline) {
			t.Fatal("Antigravity refresh did not complete")
		}
		time.Sleep(time.Millisecond)
	}
}

func recoverySameInode(t *testing.T, before os.FileInfo, path string) {
	t.Helper()
	after, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	dev, ino := fileID(before)
	afterDev, afterIno := fileID(after)
	if ino == 0 || afterIno == 0 {
		t.Skip("file identities unavailable")
	}
	if !os.SameFile(before, after) || dev != afterDev || ino != afterIno {
		t.Fatal("fixture changed device/inode instead of completing in place")
	}
}

func TestRecoveryPartialCodexHeader(t *testing.T) {
	for _, initial := range []string{"", `{"type":"session_meta","payload":`, `not json` + "\n"} {
		t.Run(fmt.Sprintf("initial-%d", len(initial)), func(t *testing.T) {
			root := t.TempDir()
			work := filepath.Join(root, "work")
			if err := os.MkdirAll(work, 0o700); err != nil {
				t.Fatal(err)
			}
			work = canonical(work)
			path := filepath.Join(root, "codex", "rollout-"+idA+".jsonl")
			writeLines(t, path, "")
			if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			r := Roots{CodexSessions: []string{filepath.Dir(path)}}
			x, clk := testIndex(r)
			recoveryParity(t, x, r, "codex", work, 0)
			recoveryRefresh(t, x, clk, work)
			recoveryParity(t, x, r, "codex", work, 0)

			// WriteFile truncates the existing file rather than replacing it.
			writeLines(t, path, codexFixture(work, idA)...)
			recoverySameInode(t, before, path)
			setMtime(t, path, t0)
			for i := 0; i < 3; i++ {
				recoveryRefresh(t, x, clk, work)
				recoveryParity(t, x, r, "codex", work, 1)
			}
		})
	}
}

// A failed open must also recover even if the directory entry's inode
// remains unchanged. The symlink and its temporarily absent target are fixtures.
func TestRecoveryCodexTransientOpenFailure(t *testing.T) {
	root := t.TempDir()
	work := canonical(root)
	target, parked := filepath.Join(root, "target"), filepath.Join(root, "parked")
	writeLines(t, target, codexFixture(work, idA)...)
	targetBefore, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "rollout-"+idA+".jsonl")
	if err := os.Symlink(target, path); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(target, parked); err != nil {
		t.Fatal(err)
	}
	r := Roots{CodexSessions: []string{root}}
	x, clk := testIndex(r)
	recoveryParity(t, x, r, "codex", work, 0)
	recoveryRefresh(t, x, clk, work)
	if err := os.Rename(parked, target); err != nil {
		t.Fatal(err)
	}
	recoverySameInode(t, before, path)
	recoverySameInode(t, targetBefore, target)
	setMtime(t, path, t0)
	for i := 0; i < 2; i++ {
		recoveryRefresh(t, x, clk, work)
		recoveryParity(t, x, r, "codex", work, 1)
	}
}

func TestRecoveryCodexSuccessfulHeaderReused(t *testing.T) {
	for _, source := range []string{"cli", "exec"} {
		t.Run(source, func(t *testing.T) {
			root := t.TempDir()
			work := canonical(root)
			path := filepath.Join(root, "rollout-"+idA+".jsonl")
			header := strings.ReplaceAll(codexFixture(work, idA)[0], `"source":"cli"`, `"source":"`+source+`"`)
			writeLines(t, path, header)
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			x, clk := testIndex(Roots{CodexSessions: []string{root}})
			if got, err := x.Candidates(context.Background(), work); err != nil || len(got) != 1 {
				t.Fatalf("initial discovery: %v %v", got, err)
			}
			// A successful classification stays immutable on this inode,
			// including a worker's non-interactive classification.
			writeLines(t, path, "invalid rewritten header")
			recoverySameInode(t, before, path)
			for i := 0; i < 2; i++ {
				recoveryRefresh(t, x, clk, work)
				if got, err := x.Candidates(context.Background(), work); err != nil || len(got) != 1 {
					t.Fatalf("successful head reread: %v %v", got, err)
				}
				want := 0
				if source == "cli" {
					want = 1
				}
				if got := x.Written(context.Background(), "codex", work, time.Time{}); len(got) != want {
					t.Fatalf("interactive classification changed: %v, want %d", got, want)
				}
			}
		})
	}
}

func TestRecoveryAgySharedWorkspace(t *testing.T) {
	for _, mapping := range []string{"history", "latest", "history-and-latest"} {
		t.Run(mapping, func(t *testing.T) {
			root := t.TempDir()
			a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
			for _, dir := range []string{a, b} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			app := filepath.Join(root, "app")
			path := AgyTranscript(app, idA)
			writeLines(t, path, agyFixture()...)
			setMtime(t, path, t0)
			history := []string{fmt.Sprintf(`{"workspace":%q,"conversationId":%q}`, a, idA)}
			latest := fmt.Sprintf(`{%q:%q,%q:%q}`, a, idA, b, idA)
			if mapping == "history" {
				history = append(history, fmt.Sprintf(`{"workspace":%q,"conversationId":%q}`, b, idA))
			}
			if mapping != "latest" {
				// Duplicate records and roots must still give one path per workspace.
				writeLines(t, filepath.Join(app, "history.jsonl"), append(history, history[0])...)
			}
			if mapping != "history" {
				writeLines(t, filepath.Join(app, "cache", "last_conversations.json"), latest)
			}
			r := Roots{AgyAppData: []string{app, app}}
			x, clk := testIndex(r)
			for i := 0; i < 5; i++ {
				for _, dir := range []string{canonical(a), canonical(b)} {
					recoveryParity(t, x, r, "agy", dir, 1)
				}
				if i < 4 {
					recoveryRefresh(t, x, clk, canonical(a))
				}
			}
		})
	}
}

func recoveryTiedCopies(t *testing.T) (Roots, string, string) {
	t.Helper()
	r, _ := findFixture(t)
	real := filepath.Join(r.ClaudeProjects[0], "-w", idA+".jsonl")
	acct := filepath.Join(r.ClaudeProjects[1], "-w", idA+".jsonl")
	writeLines(t, real, "old")
	writeLines(t, acct, "new")
	setMtime(t, real, t0)
	setMtime(t, acct, t0)
	return r, real, acct
}

func recoveryHealthyRetry(t *testing.T, x *Index, r Roots, acct string) {
	t.Helper()
	parse := func(_ context.Context, path string) (Summary, error) {
		at := t0
		if path == acct {
			at = at.Add(time.Hour)
		}
		return Summary{Updated: at}, nil
	}
	want := Hit{Path: acct, Root: r.ClaudeProjects[1], Account: r.Labels[r.ClaudeProjects[1]]}
	if hit, err := x.Find(context.Background(), "claude", idA, parse); err != nil || hit != want {
		t.Fatalf("healthy retry reused incomplete selection: %+v %v, want %+v", hit, err, want)
	}
}

func TestRecoveryCanceledTieNotMemoized(t *testing.T) {
	for _, kind := range []string{"budget", "canceled-error", "deadline-error", "cancel-budget", "cancel-success", "cancel-unreadable", "deadline"} {
		for _, failAt := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s/parse-%d", kind, failAt), func(t *testing.T) {
				r, _, acct := recoveryTiedCopies(t)
				x, _ := testIndex(r)
				ctx, cancel := context.WithCancel(context.Background())
				if kind == "deadline" {
					cancel()
					ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
				}
				defer cancel()
				want := ErrBudget
				if strings.HasPrefix(kind, "cancel") {
					want = context.Canceled
				} else if strings.HasPrefix(kind, "deadline") {
					want = context.DeadlineExceeded
				}
				calls := 0
				bad := func(context.Context, string) (Summary, error) {
					calls++
					if calls != failAt {
						return Summary{Updated: t0}, nil
					}
					switch kind {
					case "cancel-budget":
						cancel()
						return Summary{}, ErrBudget
					case "cancel-success":
						cancel()
						return Summary{Updated: t0}, nil
					case "cancel-unreadable":
						cancel()
						return Summary{}, ErrUnreadable
					case "deadline":
						<-ctx.Done()
						return Summary{}, ErrBudget
					default:
						return Summary{}, fmt.Errorf("incomplete comparison: %w", want)
					}
				}
				if hit, err := x.Find(ctx, "claude", idA, bad); !errors.Is(err, want) || hit != (Hit{}) {
					t.Errorf("incomplete comparison reported success: %+v %v, want %v", hit, err, want)
				}
				recoveryHealthyRetry(t, x, r, acct)
			})
		}
	}
}

func TestRecoveryUnreadableTieFallback(t *testing.T) {
	for _, failure := range []string{"both", "real", "account", "panic", "ordinary-error"} {
		t.Run(failure, func(t *testing.T) {
			r, real, acct := recoveryTiedCopies(t)
			x, _ := testIndex(r)
			parse := func(_ context.Context, path string) (Summary, error) {
				if failure == "panic" {
					panic("invented parser failure")
				}
				if failure == "ordinary-error" {
					return Summary{}, errors.New("invented read failure")
				}
				if failure == "both" || failure == "real" && path == real || failure == "account" && path == acct {
					return Summary{}, fmt.Errorf("invented parse: %w", ErrUnreadable)
				}
				return Summary{Updated: t0}, nil
			}
			want := Hit{Path: real, Root: r.ClaudeProjects[0]}
			if failure == "real" {
				want = Hit{Path: acct, Root: r.ClaudeProjects[1], Account: r.Labels[r.ClaudeProjects[1]]}
			}
			for i := 0; i < 2; i++ {
				if hit, err := x.Find(context.Background(), "claude", idA, parse); err != nil || hit != want {
					t.Fatalf("unreadable fallback: %+v %v, want %+v", hit, err, want)
				}
			}
		})
	}
}

func TestRecoveryCanceledMemoHit(t *testing.T) {
	r, _, acct := recoveryTiedCopies(t)
	x, _ := testIndex(r)
	recoveryHealthyRetry(t, x, r, acct)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if hit, err := x.Find(ctx, "claude", idA, ParseSummary); !errors.Is(err, context.Canceled) || hit != (Hit{}) {
		t.Fatalf("canceled memo lookup: %+v %v", hit, err)
	}
	recoveryHealthyRetry(t, x, r, acct)
}

func TestRecoveryTieBehindCacheSlot(t *testing.T) {
	r, _, acct := recoveryTiedCopies(t)
	x, _ := testIndex(r)
	cfg := defaultCacheConfig
	cfg.width = 1
	c := newCache(cfg)
	busy := filepath.Join(t.TempDir(), "busy.jsonl")
	writeLines(t, busy, "invented busy file")
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	c.parse = func(path string) (*Session, error) {
		if path == busy {
			close(started)
			<-release
		}
		at := t0
		if path == acct {
			at = at.Add(time.Hour)
		}
		return &Session{Provider: "claude", ID: idA, Path: path, Updated: at}, nil
	}
	go func() {
		_, _, err := c.Session(context.Background(), busy)
		done <- err
	}()
	defer func() {
		close(release)
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("slot holder: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("slot holder did not finish")
		}
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("slot holder did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if hit, err := x.Find(ctx, "claude", idA, c.Summary); hit != (Hit{}) || (!errors.Is(err, ErrBudget) && !errors.Is(err, context.DeadlineExceeded)) {
		t.Errorf("saturated-slot comparison: %+v %v", hit, err)
	}
	if c.Stats().Parses != 1 {
		t.Error("timed-out comparison started a parse behind the occupied slot")
	}
	// The same cache and Index must recover after actual contention clears.
	release <- struct{}{}
	if hit, err := x.Find(context.Background(), "claude", idA, c.Summary); err != nil || hit.Path != acct || hit.Account != r.Labels[r.ClaudeProjects[1]] {
		t.Fatalf("retry after slot release: %+v %v", hit, err)
	}
}
