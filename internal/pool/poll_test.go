package pool

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/config"
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
	if err := p.pollOne(acc, 2*time.Second); err != nil {
		t.Fatal(err)
	}
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
