//go:build darwin || linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/herdr"
	"github.com/orlenko/aiq/internal/longrun"
	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/testenv"
	"github.com/orlenko/aiq/internal/transcript"
)

const fakeHerdrProviderScript = `#!/bin/sh
case "$AIQ_PROVIDER" in claude) payload=$GO_HERDR_CLAUDE_PAYLOAD;; codex) payload=$GO_HERDR_CODEX_PAYLOAD;; *) exit 3;; esac
hook() { printf '%s' "$payload" | "$GO_HERDR_AIQ" "$AIQ_PROVIDER-hook" "$1"; }
hook sessionstart
printf 'FAKE READY %s/%s\n' "$AIQ_PROVIDER" "$AIQ_ACCOUNT"
printf 'FAKE ARGS'; printf ' <%s>' "$@"; printf '\n'
while IFS= read -r line; do
 case "$line" in
  quit) exit 0;;
  busy) hook userpromptsubmit; printf 'FAKE BUSY\n';;
  idle) hook stop; printf 'FAKE IDLE\n';;
  *) hook userpromptsubmit; printf 'FAKE ANSWER\n'; hook stop;;
 esac
done
`

// This exercises the real CLI/daemon takeover boundary and hooks, but every
// account, credential marker, provider program and transcript is invented.
func TestHerdrAccountAndProviderRollover(t *testing.T) {
	if _, err := exec.LookPath("hc"); err != nil {
		t.Skip("hc unavailable; native end-to-end evidence requires installed core")
	}
	root, err := os.MkdirTemp("/tmp", "aiqh-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	t.Setenv("AIQ_DATA_DIR", filepath.Join(root, "aiq"))
	t.Setenv("AIQ_CONFIG", filepath.Join(root, "config.toml"))
	t.Setenv("HC_SOCKET", filepath.Join(root, "core.sock"))
	t.Setenv("HC_SESSION", "aiq-e2e")
	bin := filepath.Join(root, "aiq-bin")
	if out, err := testenv.GoCommand("build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build fixture aiq: %v %s", err, out)
	}
	t.Setenv("GO_HERDR_AIQ", bin)
	for provider, id := range map[string]string{"claude": "11111111-1111-4111-8111-111111111111", "codex": "22222222-2222-4222-8222-222222222222"} {
		name := id + ".jsonl"
		if provider == "codex" {
			name = "rollout-2026-01-01-" + id + ".jsonl"
		}
		file := filepath.Join(root, name)
		text := fmt.Sprintf(`{"type":"user","sessionId":%q,"timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"invented task"}}`+"\n", id)
		if provider == "codex" {
			text = fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"cwd":%q}}`+"\n"+`{"type":"event_msg","payload":{"type":"user_message","message":"invented task"}}`+"\n", id, root)
		}
		if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		payload, _ := json.Marshal(map[string]string{"session_id": id, "transcript_path": file})
		t.Setenv("GO_HERDR_"+strings.ToUpper(provider)+"_PAYLOAD", string(payload))
	}
	fake := filepath.Join(root, "fiction-provider")

	if err := os.WriteFile(fake, []byte(fakeHerdrProviderScript), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Providers.Claude.Binary = fake
	cfg.Providers.Codex.Binary = fake
	cfg.Long.Fallback = []string{"claude", "codex"}
	cfg.Daemon.Listen = "127.0.0.1:1"
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if err := paths.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(paths.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, id := range []string{"claude/fiction-a", "claude/fiction-b", "codex/fiction-c", "codex/fiction-d"} {
		provider, name, _ := state.ParseAccountID(id)
		home := filepath.Join(root, name)
		os.Mkdir(home, 0o700)
		os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"fiction":true}`), 0o600)
		if err := st.AddAccount(state.Account{ID: id, Provider: provider, Name: name, Enabled: true, Native: true, Home: home}); err != nil {
			t.Fatal(err)
		}
		setFakeUsage(t, st, id, 10)
	}
	defer stopFixtureCore(t)
	startCtx, cancelStart := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelStart()
	cmd := fixtureHC(startCtx, "run", "--bg", "--json", "--no-parent", "--cwd", root, "--", bin, "long", "--backend", "herdr", "claude", "--account", "fiction-a")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("start native CLI: %v %s", err, out)
	}
	var started struct {
		Address string `json:"address"`
	}
	if err := json.Unmarshal(out, &started); err != nil {
		t.Fatal(err)
	}
	waitFakeText(t, started.Address, "FAKE READY claude/fiction-a")
	first := nativeLease(t, st)
	if first.SessionID != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("SessionStart hook did not register: %+v", first)
	}
	if out, err := exec.Command(bin, "convo", "--pane", first.Pane).CombinedOutput(); err != nil || !bytes.Contains(out, []byte("invented task")) {
		t.Fatalf("native conversation lookup: %v %s", err, out)
	}
	t.Run("resume-attaches-to-existing-native-session", func(t *testing.T) {
		stubDir := t.TempDir()
		marker := filepath.Join(stubDir, "attach")
		os.WriteFile(filepath.Join(stubDir, "hc"), []byte("#!/bin/sh\nprintf '%s %s' \"$1\" \"$2\" > \"$GO_HERDR_ATTACH_MARKER\"\n"), 0o700)
		t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		t.Setenv("GO_HERDR_ATTACH_MARKER", marker)
		a := &app{cfg: cfg, st: st}
		if err := a.resumeSession(transcript.Session{ID: first.SessionID}, nil, liveSessions(a.st, paths.RealClaudeHome(), a.cfg.Long.TmuxPrefix)[first.SessionID], resumeOpts{}); err != nil {
			t.Fatal(err)
		}
		if err := a.attachLongWorkspace(root); err != nil {
			t.Fatal(err)
		}
		if b, err := os.ReadFile(marker); err != nil || string(b) != "attach "+started.Address {
			t.Fatalf("wrong backend attach: %q %v", b, err)
		}
	})
	followCtx, followCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer followCancel()
	follow := exec.CommandContext(followCtx, bin, "convo", "--follow", "--pane", first.Pane)
	var following bytes.Buffer
	follow.Stdout = &following
	follow.Stderr = &following
	if err := follow.Start(); err != nil {
		t.Fatal(err)
	}
	defer follow.Process.Kill()
	setFakeUsage(t, st, first.AccountID, 100)
	// A fresh process models daemon restart and avoids another test's cached
	// machine identity from a deliberately restricted PATH.
	tick := func() {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestNativeSupervisorFixture$")
		cmd.Env = append(os.Environ(), "GO_WANT_NATIVE_SUPERVISOR=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("native supervisor fixture: %v %s", err, out)
		}
	}
	tick()
	text := waitFakeText(t, started.Address, "FAKE READY claude/fiction-b")
	second := nativeLease(t, st)
	if !sameNativeFamily(second.Pane, first.Pane) || second.Pane == first.Pane || second.PID == first.PID || second.SessionID != first.SessionID || second.TakeoverOf != first.ID {
		t.Fatalf("same-provider rollover lost identity: before=%+v after=%+v", first, second)
	}
	if !strings.Contains(text, "<--resume> <"+first.SessionID+">") {
		t.Fatal("same-provider successor did not resume the transcript")
	}
	setFakeUsage(t, st, second.AccountID, 100)
	tick()
	text = waitFakeText(t, started.Address, "FAKE READY codex/fiction-c")
	third := nativeLease(t, st)
	if third.Provider != "codex" || !sameNativeFamily(third.Pane, first.Pane) || third.Pane == second.Pane || third.SessionID == first.SessionID || third.TakeoverOf != second.ID {
		t.Fatalf("provider rollover lost chain: %+v", third)
	}
	if !strings.Contains(text, "from a claude session") {
		t.Fatal("cross-provider successor did not receive the handoff prompt")
	}
	// Exercise Codex -> Codex too, using the same stable Herdr terminal.
	setFakeUsage(t, st, third.AccountID, 100)
	tick()
	text = waitFakeText(t, started.Address, "FAKE READY codex/fiction-d")
	fourth := nativeLease(t, st)
	if !sameNativeFamily(fourth.Pane, first.Pane) || fourth.Pane == third.Pane || fourth.SessionID != third.SessionID || !strings.Contains(text, "<resume> <"+third.SessionID+">") {
		t.Fatal("Codex rollover did not keep terminal and conversation")
	}
	// Following resolves the stable terminal across the transcript gap and
	// the provider change. Give its five-second resolver a complete interval.
	time.Sleep(5500 * time.Millisecond)
	follow.Process.Signal(syscall.SIGTERM)
	if err := follow.Wait(); err != nil {
		t.Fatalf("native conversation follow: %v %s", err, following.String())
	}
	if !strings.Contains(following.String(), "moved to codex/") {
		t.Fatalf("conversation follow missed provider rollover: %s", following.String())
	}
	r, err := herdr.Info(fourth.Pane)
	if err != nil {
		t.Fatal(err)
	}
	if r.Core.Terminal != started.Address || r.Generation != 4 {
		t.Fatalf("rollovers replaced terminal: %+v", r)
	}
	sendFakeLine(t, started.Address, "busy")
	waitFakeText(t, started.Address, "FAKE BUSY")
	// An intentionally missing lease is reconstructed from its immutable
	// launch and saved fallback, even after the current config changes.
	st.ForLease(fourth, 0).ReleaseLease(fourth.ID)
	cfg.Long.Fallback = []string{"agy"}
	cfg.Long.IdleGraceSeconds = 1
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	tick()
	adopted := nativeLease(t, st)
	if adopted.PID != fourth.PID || adopted.Fallback != fourth.Fallback || adopted.SessionID != fourth.SessionID || !adopted.InTurn() {
		t.Fatalf("native adoption lost session policy: %+v", adopted)
	}
	// A healthy fallback exists, but the adopted turn must prevent idle takeover.
	setFakeUsage(t, st, "claude/fiction-b", 10)
	setFakeUsage(t, st, adopted.AccountID, 97)
	tick()
	dbTurn, err := sql.Open("sqlite", paths.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dbTurn.Exec("UPDATE leases SET drain_at = ? WHERE id = ?", time.Now().Add(-time.Minute).Unix(), adopted.ID); err != nil {
		t.Fatal(err)
	}
	dbTurn.Close()
	tick()
	busy := nativeLease(t, st)
	if busy.Pane != adopted.Pane || !busy.InTurn() {
		t.Fatalf("adopted turn was cut off: %+v", busy)
	}
	sendFakeLine(t, started.Address, "idle")
	waitFakeText(t, started.Address, "FAKE IDLE")
	idle, err := st.GetLease(adopted.ID)
	if err != nil || idle.InTurn() {
		t.Fatalf("genuine stop did not clear adopted turn: %+v %v", idle, err)
	}
	if err := herdr.Stop(adopted.Pane, adopted.PID); err != nil {
		t.Fatal(err)
	}
	previousEnd := awaitNativeAttempt(t, nativeAttempt{root, os.Getenv("HC_SOCKET"), started.Address, ""})
	// Normalize only the known completed prior lease before measuring new rows.
	if err := st.ForLease(adopted, 0).ReleaseLease(adopted.ID); err != nil {
		t.Fatal(err)
	}
	remaining, err := st.ListLeases()
	if err != nil {
		t.Fatal(err)
	}
	for _, lease := range remaining {
		if lease.ID == adopted.ID {
			t.Fatal("completed fixture lease remains before baseline")
		}
	}
	// A native launch must never exec an untracked provider when lease or
	// launch registration fails. The failure injection touches this test DB.
	db, err := sql.Open("sqlite", paths.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	positiveNativeProviderEntry(t, root)
	for _, table := range []string{"leases", "launches"} {
		entry := filepath.Join(root, table+"-provider-entry")
		if err := os.WriteFile(fake, []byte(fakeProviderWithEntry(entry)), 0700); err != nil {
			t.Fatal(err)
		}
		assertNoProviderEntry(t, entry)
		beforeLeases, beforeLaunches := nativeSQLIDs(t, db, "leases"), nativeSQLIDs(t, db, "launches")
		setFakeUsage(t, st, "claude/fiction-a", 10)
		_, err := db.Exec("CREATE TRIGGER reject_native_registration BEFORE INSERT ON " + table + " BEGIN SELECT RAISE(ABORT, 'fiction registration failure'); END")
		if err != nil {
			t.Fatal(err)
		}
		dropped := false
		defer func() {
			if !dropped {
				if _, err := db.Exec("DROP TRIGGER IF EXISTS reject_native_registration"); err != nil {
					t.Errorf("fixture trigger cleanup: %v", err)
				}
			}
		}()
		launchCtx, cancelLaunch := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelLaunch()
		cmd := fixtureHC(launchCtx, "run", "--bg", "--json", "--no-parent", "--cwd", root, "--", bin, "long", "--backend", "herdr", "claude", "--account", "fiction-a")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("start registration fixture: %v %s", err, out)
		}
		var failed struct {
			Address string `json:"address"`
		}
		if err := json.Unmarshal(out, &failed); err != nil {
			t.Fatal(err)
		}
		want := "register native long lease"
		if table == "launches" {
			want = "record native long launch"
		}
		text := waitFakeText(t, failed.Address, want)
		if strings.Contains(text, "FAKE READY") {
			t.Fatal("provider ran without registration")
		}
		previousEnd = awaitNativeAttempt(t, nativeAttempt{root, os.Getenv("HC_SOCKET"), failed.Address, previousEnd.Ref})
		assertNoProviderEntry(t, entry)
		var final struct {
			Text string `json:"text"`
		}
		if err := fixtureRPC("terminal.read", map[string]any{"terminal": failed.Address, "lines": 100}, &final); err != nil {
			t.Fatal("post-completion terminal read failed")
		}
		if !strings.Contains(final.Text, want) || strings.Contains(final.Text, "FAKE READY") {
			t.Fatal("post-completion registration evidence is inconsistent")
		}
		assertNativeSQLIDs(t, db, beforeLeases, beforeLaunches)
		if _, err := db.Exec("DROP TRIGGER reject_native_registration"); err != nil {
			t.Fatal(err)
		}
		dropped = true
	}
}

func setFakeUsage(t *testing.T, st *state.Store, id string, used float64) {
	t.Helper()
	now := time.Now()
	if err := st.ReplaceWindows(id, "fixture", []state.Window{{Key: "weekly", Kind: state.KindWeekly, UsedPct: used, ResetsAt: now.Add(time.Hour).Unix(), ObservedAt: now.Unix()}}); err != nil {
		t.Fatal(err)
	}
}
func nativeLease(t *testing.T, st *state.Store) state.Lease {
	t.Helper()
	leases, err := st.ListLeases()
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range leases {
		if l.Mode == state.ModeLong && herdr.IsRef(l.Pane) {
			return l
		}
	}
	t.Fatal("no native lease")
	return state.Lease{}
}

func fixtureRPC(method string, params any, result any) error {
	c, err := net.DialTimeout("unix", os.Getenv("HC_SOCKET"), time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(time.Second))
	if err := json.NewEncoder(c).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params}); err != nil {
		return err
	}
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		return err
	}
	var out struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(line, &out); err != nil {
		return err
	}
	return json.Unmarshal(out.Result, result)
}
func waitFakeText(t *testing.T, address, want string) string {
	t.Helper()
	deadline := time.Now().Add(12 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		var out struct {
			Text string `json:"text"`
		}
		if fixtureRPC("terminal.read", map[string]any{"terminal": address, "lines": 100}, &out) == nil {
			last = out.Text
			if strings.Contains(last, want) {
				return last
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("fake provider did not print %q: %s", want, last)
	return ""
}

func TestNativeSupervisorFixture(t *testing.T) {
	if os.Getenv("GO_WANT_NATIVE_SUPERVISOR") != "1" {
		return
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	st, err := state.Open(paths.StateDB())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	sup := &longrun.Supervisor{Pool: &pool.Pool{Cfg: cfg, St: st}, AiqBin: os.Getenv("GO_HERDR_AIQ"), Logf: func(f string, args ...any) { fmt.Fprintf(os.Stderr, f+"\n", args...) }}
	sup.Tick()
	st.Close()
	os.Exit(0)
}

func sameNativeFamily(a, b string) bool {
	fa, _, ea := herdr.ParseRef(a)
	fb, _, eb := herdr.ParseRef(b)
	return ea == nil && eb == nil && fa == fb
}

func sendFakeLine(t *testing.T, address, line string) {
	t.Helper()
	var result json.RawMessage
	if err := fixtureRPC("terminal.send", map[string]any{"terminal": address, "text": line}, &result); err != nil {
		t.Fatal(err)
	}
}
