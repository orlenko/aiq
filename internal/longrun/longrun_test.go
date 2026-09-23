package longrun

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/state"
)

func TestSessionNameAndHandoffPathAreStable(t *testing.T) {
	ws := "/home/me/code/my.project"
	a := SessionName("aiq", ws)
	b := SessionName("aiq", ws)
	if a != b || !strings.HasPrefix(a, "aiq-my-project-") {
		t.Fatalf("%s %s", a, b)
	}
	if SessionName("aiq", "/home/me/code/other") == a {
		t.Fatal("different workspaces must not collide")
	}
	if HandoffPath(ws) != HandoffPath(ws) || !strings.HasSuffix(HandoffPath(ws), ".md") {
		t.Fatal("handoff path must be deterministic")
	}
}

func TestPromptsMentionTheNote(t *testing.T) {
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	ws := "/w"
	d := DrainInstruction(ws, 3, ", resets 14:00")
	if !strings.Contains(d, HandoffPath(ws)) || !strings.Contains(d, "3% left, resets 14:00") {
		t.Fatalf("%s", d)
	}
	p := TakeoverPrompt(ws, false, "claude")
	if !strings.Contains(p, "No handoff note") || !strings.Contains(p, "from a claude session") {
		t.Fatalf("%s", p)
	}
	os.MkdirAll(filepath.Dir(HandoffPath(ws)), 0o700)
	os.WriteFile(HandoffPath(ws), []byte("# note"), 0o600)
	p = TakeoverPrompt(ws, true, "claude")
	if !strings.Contains(p, "Read the handoff note") || !strings.Contains(p, "conversation above is yours") {
		t.Fatalf("%s", p)
	}
}

func TestSplitArgs(t *testing.T) {
	got := splitArgs(`["--model","x y","-p"]`)
	if len(got) != 3 || got[1] != "x y" {
		t.Fatalf("%v", got)
	}
	if got := splitArgs("a b"); len(got) != 2 {
		t.Fatalf("fallback: %v", got)
	}
}

func TestTranslateArgsCarriesThePermissionBypass(t *testing.T) {
	cases := []struct {
		from, to string
		in, want []string
	}{
		{"claude", "codex", []string{"--dangerously-skip-permissions", "--model", "opus"}, []string{"--dangerously-bypass-approvals-and-sandbox", "--model", "gpt-5.6-sol"}},
		{"claude", "codex", []string{"--permission-mode", "bypassPermissions"}, []string{"--dangerously-bypass-approvals-and-sandbox"}},
		{"claude", "codex", []string{"--permission-mode=bypassPermissions"}, []string{"--dangerously-bypass-approvals-and-sandbox"}},
		{"codex", "claude", []string{"--yolo", "-m", "gpt-5"}, []string{"--dangerously-skip-permissions"}},
		{"codex", "claude", []string{"--dangerously-bypass-approvals-and-sandbox"}, []string{"--dangerously-skip-permissions"}},
		{"claude", "codex", []string{"--permission-mode", "acceptEdits"}, nil},
		{"claude", "codex", []string{"--model", "claude-opus-5"}, nil},
		{"claude", "codex", []string{"--model", "opus"}, []string{"--model", "gpt-5.6-sol"}},
		{"claude", "codex", []string{"--model=fable", "--effort", "ultracode", "--dangerously-skip-permissions"},
			[]string{"--dangerously-bypass-approvals-and-sandbox", "--model", "gpt-6-astra", "-c", "model_reasoning_effort=ultra"}},
		{"codex", "claude", []string{"--model", "gpt-5.6-luna", "-c", "model_reasoning_effort=high", "--yolo"},
			[]string{"--dangerously-skip-permissions", "--model", "haiku", "--effort", "high"}},
		{"codex", "claude", []string{"-m", "gpt-5.6-terra", "--config=model_reasoning_effort=\"low\""}, []string{"--model", "sonnet", "--effort", "low"}},
		{"claude", "codex", []string{"--", "--model", "opus"}, nil},
		{"claude", "claude", []string{"--model", "opus"}, []string{"--model", "opus"}},
		// Antigravity has no tier-0 or tier-1 model: the model is dropped
		// (a takeover never picks it for such a session; see successorAbove).
		{"claude", "agy", []string{"--dangerously-skip-permissions", "--model", "opus", "--effort", "max"},
			[]string{"--dangerously-skip-permissions", "--effort", "high"}},
		{"claude", "agy", []string{"--model", "sonnet", "--effort", "low"}, []string{"--model", "claude-opus-4-6-thinking"}},
		{"claude", "agy", []string{"--model", "haiku", "--effort", "low"}, []string{"--model", "gemini-3.8-flash-low"}},
		{"claude", "agy", []string{"--model", "fable", "--effort", "low"}, []string{"--effort", "low"}},
		{"claude", "agy", []string{"--effort", "medium"}, []string{"--effort", "medium"}},
		{"agy", "codex", []string{"--dangerously-skip-permissions", "--model=gemini-3.8-flash-low", "--effort", "low", "--conversation", "abc"},
			[]string{"--dangerously-bypass-approvals-and-sandbox", "--model", "gpt-5.6-luna", "-c", "model_reasoning_effort=low"}},
		{"agy", "codex", []string{"--model=gemini-3.1-pro-high"}, nil},
		{"agy", "claude", []string{"-c", "--model", "claude-opus-4-6-thinking"}, []string{"--model", "sonnet"}},
		{"agy", "claude", []string{"--model", "gemini-3.8-flash-high"}, []string{"--model", "haiku"}},
		{"codex", "agy", []string{"--yolo", "-m", "gpt-5.6-terra", "-c", "model_reasoning_effort=xhigh"},
			[]string{"--dangerously-skip-permissions", "--model", "claude-opus-4-6-thinking"}},
		{"claude", "copilot", []string{"--model", "fable", "--effort", "xhigh"}, []string{"--model", "gpt-6-astra", "--effort", "xhigh"}},
		{"copilot", "claude", []string{"--model", "gpt-5.6-sol", "--effort", "max"}, []string{"--model", "opus", "--effort", "max"}},
	}
	for _, c := range cases {
		got := TranslateArgs(c.from, c.to, c.in)
		if strings.Join(got, " ") != strings.Join(c.want, " ") {
			t.Errorf("%s→%s %v: got %v, want %v", c.from, c.to, c.in, got, c.want)
		}
	}
}

func TestReplayArgsDropsSessionSelection(t *testing.T) {
	cases := []struct {
		provider string
		in, want []string
	}{
		{"codex", []string{"--yolo", "resume"}, []string{"--yolo"}},
		{"codex", []string{"resume", "--last", "--yolo"}, []string{"--yolo"}},
		{"codex", []string{"--yolo", "resume", "01a0", "do it"}, []string{"--yolo"}},
		{"codex", []string{"fork", "01a0", "-m", "gpt-5"}, []string{"-m", "gpt-5"}},
		{"codex", []string{"--yolo", "-c", "model=\"x\""}, []string{"--yolo", "-c", "model=\"x\""}},
		{"claude", []string{"--dangerously-skip-permissions", "--resume", "abc"}, []string{"--dangerously-skip-permissions"}},
		{"claude", []string{"--resume"}, nil},
		{"claude", []string{"-c", "--model", "opus"}, []string{"--model", "opus"}},
		{"claude", []string{"--session-id=abc", "-r"}, nil},
		{"claude", []string{"--model", "opus"}, []string{"--model", "opus"}},
		{"agy", []string{"--dangerously-skip-permissions", "--conversation", "abc"}, []string{"--dangerously-skip-permissions"}},
		{"agy", []string{"--conversation=abc", "-c", "--model", "gemini-3.1-pro-high"}, []string{"--model", "gemini-3.1-pro-high"}},
		{"agy", []string{"--continue", "--effort", "high"}, []string{"--effort", "high"}},
	}
	for _, c := range cases {
		got := replayArgs(c.provider, c.in)
		if strings.Join(got, " ") != strings.Join(c.want, " ") {
			t.Errorf("%s %v: got %v, want %v", c.provider, c.in, got, c.want)
		}
	}
}

func TestDrainInstructionKeepsTheAgentWorking(t *testing.T) {
	d := DrainInstruction("/w", 3, "")
	for _, want := range []string{"Keep working", "do not stop", "Keep the note current"} {
		if !strings.Contains(d, want) {
			t.Errorf("missing %q in %s", want, d)
		}
	}
	if strings.Contains(d, "end your turn") || strings.Contains(d, "do not start new work") {
		t.Errorf("drain must not tell the agent to stop: %s", d)
	}
}

func TestIdleRotateDue(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	idle := 15 * time.Minute
	ended := now.Add(-20 * time.Minute)
	quiet := endedAt(ended)
	cases := []struct {
		name      string
		lease     state.Lease
		remaining float64
		pct       float64
		lastWrite time.Time
		want      bool
	}{
		{"quiet and low", quiet, 8, 15, ended, true},
		{"plenty left", quiet, 40, 15, ended, false},
		{"turned off", quiet, 8, 0, ended, false},
		{"in a turn", func() state.Lease { x := quiet; x.TurnStartedAt = now.Unix(); return x }(), 8, 15, ended, false},
		{"idle too briefly", endedAt(now.Add(-5 * time.Minute)), 8, 15, now.Add(-5 * time.Minute), false},
		{"background work still writing", quiet, 8, 15, now.Add(-time.Minute), false},
		{"transcript unknown", quiet, 8, 15, time.Time{}, false},
		{"hooks never reported", func() state.Lease { x := quiet; x.SessionID = ""; return x }(), 8, 15, ended, false},
		{"no turn yet", func() state.Lease { x := quiet; x.TurnStartedAt, x.TurnEndedAt = 0, 0; return x }(), 8, 15, ended, false},
	}
	for _, c := range cases {
		if got := IdleRotateDue(c.lease, c.remaining, c.pct, idle, c.lastWrite, now); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSuccessorUsesLowQuotaAsLastResort(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	type target struct {
		used   float64
		resets time.Duration
	}
	cases := []struct {
		name       string
		sourceUsed float64
		targets    []target
		want       string
	}{
		{"blocked source accepts quota below drain floor", 100, []target{{96, time.Hour}}, "codex/target-0"},
		{"lower source accepts a better account below drain floor", 97, []target{{96, time.Hour}}, "codex/target-0"},
		{"equal low accounts do not ping-pong", 96, []target{{96, time.Hour}}, ""},
		{"worse low account is not a successor", 96, []target{{97, time.Hour}}, ""},
		{"healthy account beats the more perishable last resort", 97, []target{{96, time.Hour}, {50, 7 * 24 * time.Hour}}, "codex/target-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			accounts := []state.Account{{ID: "codex/source", Provider: "codex", Name: "source", Enabled: true, Native: true}}
			for i := range tc.targets {
				name := fmt.Sprintf("target-%d", i)
				accounts = append(accounts, state.Account{ID: "codex/" + name, Provider: "codex", Name: name, Enabled: true, Native: true})
			}
			for _, a := range accounts {
				if err := st.AddAccount(a); err != nil {
					t.Fatal(err)
				}
			}
			window := func(used float64, resets time.Duration) []state.Window {
				return []state.Window{{Key: "weekly", Label: "Weekly", Kind: state.KindWeekly,
					UsedPct: used, ResetsAt: now.Add(resets).Unix(), ObservedAt: now.Unix()}}
			}
			if err := st.ReplaceWindows("codex/source", "test", window(tc.sourceUsed, time.Hour)); err != nil {
				t.Fatal(err)
			}
			for i, target := range tc.targets {
				if err := st.ReplaceWindows(fmt.Sprintf("codex/target-%d", i), "test", window(target.used, target.resets)); err != nil {
					t.Fatal(err)
				}
			}

			cfg := config.Default()
			cfg.Providers.Codex.ModelScope = ""
			s := &Supervisor{Pool: &pool.Pool{Cfg: cfg, St: st}}
			got, err := s.successor(state.Lease{AccountID: "codex/source", Provider: "codex", Fallback: "codex"}, now)
			if tc.want != "" {
				if err != nil || got.ID != tc.want {
					t.Fatalf("got %q, %v; want %s", got.ID, err, tc.want)
				}
			} else if err == nil {
				t.Fatalf("got %q; want no successor", got.ID)
			}
		})
	}
}

// A session on a tier's model must not move to a provider with no model of
// that tier: the takeover would run an older model in its place.
func TestSuccessorKeepsTheTier(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	for _, tc := range []struct {
		model, want string
	}{
		{"opus", "codex/target"},        // Antigravity has no tier 1
		{"sonnet", "agy/target"},        // but it has a tier 2
		{"claude-opus-5", "agy/target"}, // not a tier model: no constraint
	} {
		t.Run(tc.model, func(t *testing.T) {
			st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			for _, provider := range []string{"claude", "agy", "codex"} {
				name := "target"
				if provider == "claude" {
					name = "source"
				}
				id := provider + "/" + name
				if err := st.AddAccount(state.Account{ID: id, Provider: provider, Name: name, Enabled: true, Native: true}); err != nil {
					t.Fatal(err)
				}
				used := 10.0
				if provider == "claude" {
					used = 100
				}
				if err := st.ReplaceWindows(id, "test", []state.Window{{Key: "weekly", Label: "Weekly", Kind: state.KindWeekly,
					UsedPct: used, ResetsAt: now.Add(time.Hour).Unix(), ObservedAt: now.Unix()}}); err != nil {
					t.Fatal(err)
				}
			}
			cfg := config.Default()
			cfg.Providers.Claude.ModelScope = ""
			cfg.Providers.Codex.ModelScope = ""
			cfg.Providers.Agy.ModelScope = ""
			s := &Supervisor{Pool: &pool.Pool{Cfg: cfg, St: st}}
			args, _ := json.Marshal([]string{"--model", tc.model})
			got, err := s.successor(state.Lease{AccountID: "claude/source", Provider: "claude", Fallback: "claude,agy,codex", Args: string(args)}, now)
			if err != nil || got.ID != tc.want {
				t.Fatalf("got %q, %v; want %s", got.ID, err, tc.want)
			}
		})
	}
}

func endedAt(ended time.Time) state.Lease {
	return state.Lease{SessionID: "s", TurnStartedAt: ended.Add(-time.Minute).Unix(), TurnEndedAt: ended.Unix()}
}

func TestLastWriteSeesSubagents(t *testing.T) {
	dir := t.TempDir()
	tr := filepath.Join(dir, "abc.jsonl")
	os.WriteFile(tr, []byte("{}\n"), 0o600)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(tr, old, old)
	if got := LastWrite(tr); !got.Equal(old) {
		t.Fatalf("transcript only: got %v, want %v", got, old)
	}
	sub := filepath.Join(dir, "abc", "subagents", "workflows", "wf1")
	os.MkdirAll(sub, 0o700)
	os.WriteFile(filepath.Join(sub, "agent.jsonl"), []byte("{}\n"), 0o600)
	if got := LastWrite(tr); time.Since(got) > time.Minute {
		t.Fatalf("a fresh subagent write must count: %v", got)
	}
	if !LastWrite("").IsZero() || !LastWrite(filepath.Join(dir, "missing.jsonl")).IsZero() {
		t.Fatal("unknown transcript must be zero")
	}
}

// A takeover releases the lease the moment it respawns the pane, expecting the
// successor to register its own. When every successor dies before it can, the
// session must stay under supervision instead of being forgotten beside a dead
// pane that nothing will ever retry.
func TestReadoptKeepsAFailedTakeoverSupervised(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	orig := state.Lease{
		AccountID: "claude/a", Mode: state.ModeLong, Hostname: "h", Provider: "claude",
		Workspace: "/w", Pane: "%3", SessionID: "sess-1", Fallback: "claude",
		Args: `["--model","opus"]`, StartedAt: time.Now().Unix(), PID: 999999,
	}
	id, err := st.AddLease(orig)
	if err != nil {
		t.Fatal(err)
	}
	orig.ID = id
	st.ReleaseLease(id) // what moveTo does before the successor is known to be up

	s := &Supervisor{Pool: &pool.Pool{Cfg: config.Default(), St: st}, Logf: func(string, ...any) {}}
	s.readopt(orig, time.Now())

	leases, err := st.ListLeases()
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 {
		t.Fatalf("got %d leases after readopt, want 1", len(leases))
	}
	got := leases[0]
	if got.Pane != "%3" || got.Workspace != "/w" || got.SessionID != "sess-1" {
		t.Fatalf("readopted lease lost its identity: %+v", got)
	}
	if got.Drain != state.DrainWaiting {
		t.Fatalf("drain = %q, want %q so the supervisor retries", got.Drain, state.DrainWaiting)
	}
	if got.PID != os.Getpid() {
		t.Fatalf("pid = %d, want the daemon's %d; a dead pid is pruned before the retry", got.PID, os.Getpid())
	}
}
