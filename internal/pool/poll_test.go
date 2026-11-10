package pool

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/proc"
	"github.com/orlenko/aiq/internal/provider/claude"
	"github.com/orlenko/aiq/internal/state"
)

func TestAgyPollReplacesStaleQuotaAndPreservesMetadata(t *testing.T) {
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	acc := state.Account{ID: "agy/main", Provider: "agy", Name: "main", Home: t.TempDir(), Enabled: true, Identity: "dev@example.com"}
	if err := st.AddAccount(acc); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	st.SetUsageMeta(acc.ID, "Google AI Pro", 0, "agy finished without reporting quota", now.Add(-time.Hour))
	st.UpsertWindow(state.Window{AccountID: acc.ID, Key: "gemini:weekly", Kind: state.KindWeekly,
		UsedPct: 100, ResetsAt: now.Add(-time.Hour).Unix(), Source: SourceAIQ, ObservedAt: now.Add(-2 * time.Hour).Unix()})
	output := `{"status":"SUCCESS","num_turns":0,"command":{"name":"usage","data":{"groups":[{"buckets":[{"id":"gemini-weekly","remaining_fraction":0.76,"reset_time":"` + now.Add(7*24*time.Hour).UTC().Format(time.RFC3339) + `"}]}]}}}`
	p := &Pool{Cfg: config.Default(), St: st, Command: func(provider string, args, env []string) *exec.Cmd {
		if provider != "agy" || len(args) < 2 || args[1] != "/usage" {
			t.Errorf("unexpected poll: %s %q", provider, args)
		}
		cmd := exec.Command("sh", "-c", `printf '%s' "$AIQ_TEST_OUTPUT"`)
		cmd.Env = append(env, "AIQ_TEST_OUTPUT="+output)
		return cmd
	}}
	// The agy poll keeps the status line's windows, rolled or not.
	st.UpsertWindow(state.Window{AccountID: acc.ID, Key: "line", Kind: state.KindShort,
		ResetsAt: now.Add(-time.Hour).Unix(), Source: "statusline", ObservedAt: now.Add(-2 * time.Hour).Unix()})
	if err := p.pollOne(acc, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if ws, _ := st.ListWindows(acc.ID); len(ws) != 2 {
		t.Fatalf("agy poll windows: %+v", ws)
	}
	st.DeleteRolledWindows(acc.ID, "statusline", now.Unix())
	u, _, _ := st.GetUsage(acc.ID)
	if u.PollError != "" || u.Plan != "Google AI Pro" || u.ObservedAt < now.Unix() {
		t.Fatalf("metadata: %+v", u)
	}
	got, _ := st.GetAccount(acc.ID)
	if got.Identity != acc.Identity {
		t.Fatalf("identity changed: %+v", got)
	}
	ws, _ := st.ListWindows(acc.ID)
	if len(ws) != 1 || ws[0].UsedPct < 23.99 || ws[0].UsedPct > 24.01 || ws[0].ResetsAt <= now.Unix() {
		t.Fatalf("quota not refreshed: %+v", ws)
	}

	// A malformed next refresh must keep those last good windows.
	output = `{"status":"SUCCESS","command":{"name":"usage","data":{"groups":[]}}}`
	if err := p.pollOne(acc, 2*time.Second); err == nil {
		t.Fatal("missing quota should fail the poll")
	}
	kept, _ := st.ListWindows(acc.ID)
	u, _, _ = st.GetUsage(acc.ID)
	if len(kept) != 1 || kept[0] != ws[0] || !strings.Contains(u.PollError, "no quota buckets") {
		t.Fatalf("failed poll lost last quota: %+v, error %q", kept, u.PollError)
	}
}

// An agy poll the CLI outlasts returns once the CLI and what it started are
// gone, with the agy poll's own result, so a command that exits right after
// Poll does not leave them running.
func TestPollWaitsForAgyToStop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh and process groups")
	}
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	acc := state.Account{ID: "agy/main", Provider: "agy", Name: "main", Home: t.TempDir(), Enabled: true}
	if err := st.AddAccount(acc); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(t.TempDir(), "pid")
	p := &Pool{Cfg: config.Default(), St: st, Command: func(provider string, args, env []string) *exec.Cmd {
		if i := slices.Index(args, "--print-timeout"); i < 0 || i+1 == len(args) || args[i+1] != "1s" {
			t.Errorf("agy args %q, want --print-timeout 1s", args)
		}
		// The CLI and a process it started both ignore TERM, so only the KILL
		// that follows TERM by three seconds ends them.
		cmd := exec.Command("sh", "-c", `trap '' TERM; sleep 60 & echo $! > "$1"; wait`, "sh", pidFile)
		cmd.Env = env
		return cmd
	}}
	start := time.Now()
	res := p.Poll(2*time.Second, acc.ID)
	took := time.Since(start)
	// The CLI's deadline is 1 s; TERM, three seconds, KILL.
	if len(res) != 1 || res[0].Err == nil || !strings.Contains(res[0].Err.Error(), "agy /usage timed out after 1s") {
		t.Fatalf("results = %+v, want the agy poll's own timeout", res)
	}
	if took < 4*time.Second || took > 5500*time.Millisecond {
		t.Errorf("Poll took %s, want about 4.1 s", took)
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	// The child is gone at once, short of its new parent reaping it.
	for deadline := time.Now().Add(time.Second); proc.Alive(pid); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			if p, err := os.FindProcess(pid); err == nil {
				p.Kill()
			}
			t.Fatalf("pid %d still running %s after Poll returned at %s", pid, time.Since(start)-took, took)
		}
	}
}

// Only an agy poll is waited for past the timeout: it stops itself. A Codex
// poll has no deadline of its own, and Poll returns without it.
func TestPollDoesNotWaitForAHungCodexPoll(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sleep")
	}
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	acc := state.Account{ID: "codex/main", Provider: "codex", Name: "main", Home: t.TempDir(), Enabled: true}
	if err := st.AddAccount(acc); err != nil {
		t.Fatal(err)
	}
	p := &Pool{Cfg: config.Default(), St: st, Command: func(provider string, args, env []string) *exec.Cmd {
		return exec.Command("sleep", "2")
	}}
	start := time.Now()
	res := p.Poll(time.Second, acc.ID)
	if took := time.Since(start); took > 2*time.Second || len(res) != 1 || res[0].Err == nil {
		t.Fatalf("Poll took %s, results %+v", took, res)
	}
}

// A one-second poll timeout still gives the agy CLI a second.
func TestAgyPollGetsAtLeastASecond(t *testing.T) {
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	acc := state.Account{ID: "agy/main", Provider: "agy", Name: "main", Home: t.TempDir(), Enabled: true}
	var got []string
	p := &Pool{Cfg: config.Default(), St: st, Command: func(provider string, args, env []string) *exec.Cmd {
		got = args
		return exec.Command("true")
	}}
	p.pollOne(acc, time.Second)
	if i := slices.Index(got, "--print-timeout"); i < 0 || i+1 == len(got) || got[i+1] != "1s" {
		t.Errorf("agy args %q, want --print-timeout 1s", got)
	}
}

// The usage API leaves the five-hour window out while none is running, so the
// status line's last session window goes once its reset time has passed. One
// still running stays, even when the poll reports nothing.
func TestClaudePollDropsRolledStatusLineWindows(t *testing.T) {
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	acc := state.Account{ID: "claude/acct-a", Provider: "claude", Name: "acct-a", Home: t.TempDir(), Enabled: true}
	if err := st.AddAccount(acc); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	st.UpsertWindow(state.Window{AccountID: acc.ID, Key: "session", Kind: state.KindShort, UsedPct: 10,
		ResetsAt: now.Add(-239 * time.Hour).Unix(), Source: "statusline", ObservedAt: now.Add(-240 * time.Hour).Unix()})
	p := &Pool{Cfg: config.Default(), St: st}
	u := &claude.Usage{Windows: []state.Window{{Key: "weekly", Kind: state.KindWeekly, UsedPct: 2, ObservedAt: now.Unix()}}}
	if err := p.saveClaudeUsage(acc, u, now); err != nil {
		t.Fatal(err)
	}
	ws, _ := st.ListWindows(acc.ID)
	if len(ws) != 1 || ws[0].Key != "weekly" {
		t.Fatalf("after the poll: %+v", ws)
	}
	st.UpsertWindow(state.Window{AccountID: acc.ID, Key: "session", Kind: state.KindShort, UsedPct: 97,
		ResetsAt: now.Add(2 * time.Hour).Unix(), Source: "statusline", ObservedAt: now.Add(-30 * time.Second).Unix()})
	if err := p.saveClaudeUsage(acc, u, now); err != nil {
		t.Fatal(err)
	}
	if ws, _ := st.ListWindows(acc.ID); len(ws) != 2 {
		t.Fatalf("a running status-line window went: %+v", ws)
	}
	// A reply with no windows in it is a failed poll, and the windows stay.
	if err := p.saveClaudeUsage(acc, &claude.Usage{}, now); err == nil {
		t.Fatal("an empty reply passed for a poll")
	}
	if ws, _ := st.ListWindows(acc.ID); len(ws) != 2 {
		t.Fatalf("an empty reply cleared the windows: %+v", ws)
	}
	if got, _, _ := st.GetUsage(acc.ID); !strings.Contains(got.PollError, "no quota windows") {
		t.Fatalf("poll error %q", got.PollError)
	}
}
