package main

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/daemon"
	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/state"
)

// quotaStore is an invented store whose hidden values are distinctive, so a
// leak shows up as a substring of the response.
func quotaStore(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	for _, a := range []state.Account{
		{ID: "claude/one", Provider: "claude", Name: "one", Enabled: true, Home: "/invented/HIDDEN-HOME", QuotaID: "HIDDEN-QUOTA", Identity: "one@invented.test", Priority: 100},
		{ID: "codex/two", Provider: "codex", Name: "two", Enabled: true, Home: "/invented/HIDDEN-HOME-2", Priority: 100},
	} {
		if err := st.AddAccount(a); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetUsageMeta("claude/one", "max", 1, "", now); err != nil {
		t.Fatal(err)
	}
	if err := st.SetExtraUsage("claude/one", &state.ExtraUsage{Enabled: true, Currency: "USD", BalanceMinor: -1, UsedMinor: 250, LimitMinor: 1000, ObservedAt: now.Unix()}); err != nil {
		t.Fatal(err)
	}
	for _, w := range []state.Window{
		{AccountID: "claude/one", Key: "five_hour", Label: "5h", Kind: state.KindShort, UsedPct: 42.5, ResetsAt: now.Add(time.Hour).Unix(), Source: "probe", ObservedAt: now.Unix()},
		{AccountID: "claude/one", Key: "seven_day", Label: "7d", Kind: state.KindWeekly, UsedPct: -1, Source: "probe", ObservedAt: now.Unix()},
	} {
		if err := st.UpsertWindow(w); err != nil {
			t.Fatal(err)
		}
	}
	for _, l := range []state.Lease{
		{AccountID: "claude/one", PID: 1, Hostname: "invented-host", Mode: state.ModeLong, Cwd: "/invented/live", Workspace: "/invented/live", Pane: "%7", Args: "HIDDEN-ARGS", Transcript: "/invented/HIDDEN-TRANSCRIPT", StartedAt: now.Unix()},
		{AccountID: "claude/one", PID: 2, Hostname: "invented-host", Mode: state.ModeWorker, Cwd: "/invented/DEAD-LEASE", StartedAt: now.Unix()},
	} {
		if _, err := st.AddLease(l); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func quotaDeps(path string) *navQuotaDeps {
	p := &pool.Pool{Cfg: config.Default()}
	return &navQuotaDeps{
		read: func(ctx context.Context, l state.QuotaSnapshotLimits) (*state.QuotaSnapshot, error) {
			ro, err := state.OpenReadOnly(path)
			if err != nil {
				return nil, err
			}
			defer ro.Close()
			return ro.ReadQuotaSnapshot(ctx, l)
		},
		host:  func(context.Context) (string, error) { return "invented-host", nil },
		scope: func(_ context.Context, provider string) string { return map[string]string{"claude": "fable"}[provider] },
		poll: func() daemon.PollStatus {
			return daemon.PollStatus{IntervalSeconds: 60, LastError: "invented poll error"}
		},
		alive:      func(pid int) bool { return pid == 1 },
		credential: func(state.Account) bool { return true },
		view:       p.ViewFrom,
	}
}

func quotaFactory(t *testing.T, deps *navQuotaDeps) *navFactory {
	t.Helper()
	in := newNavAppFixture(t, 0, "Invented answer").input(nil)
	in.quotas = deps
	f, err := newNavFactory(in, navProductionPolicy())
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestNavQuotasDocument(t *testing.T) {
	path := quotaStore(t)
	f := quotaFactory(t, quotaDeps(path))
	w := factoryGet(t, f, "/api/nav/quotas")
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Code, w.Body.String())
	}
	raw := w.Body.String()
	for _, hidden := range []string{"HIDDEN", "home", "quota_id", "args", "worker_capacity", "events", "DEAD-LEASE", "invented-host\",\"mode", "native"} {
		if strings.Contains(raw, hidden) {
			t.Fatalf("response carries %q: %s", hidden, raw)
		}
	}
	var body navQuotasBody
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Schema != 1 || body.Hostname != "invented-host" || body.Poll.LastAt != nil || body.Poll.IntervalS != 60 || body.Poll.Error != "invented poll error" {
		t.Fatalf("header: %+v", body)
	}
	if len(body.ModelScope) != len(pool.Providers) || body.ModelScope[0].Provider != pool.Providers[0] {
		t.Fatalf("model scope: %+v", body.ModelScope)
	}
	if len(body.Accounts) != 2 {
		t.Fatalf("accounts: %+v", body.Accounts)
	}
	one := body.Accounts[0]
	if one.ID != "claude/one" || one.Plan != "max" || one.ResetCredits != 1 || one.Login.CLI != "unknown" || one.Login.CheckedAt != nil || one.ObservedAt == nil {
		t.Fatalf("account: %+v", one)
	}
	if one.ExtraUsage == nil || one.ExtraUsage.BalanceMinor != nil || one.ExtraUsage.UsedMinor != 250 || one.ExtraUsage.Note == "" {
		t.Fatalf("extra usage unknown balance: %+v", one.ExtraUsage)
	}
	if len(one.Windows) != 2 || one.Windows[0].UsedPct == nil || *one.Windows[0].UsedPct != 42.5 || one.Windows[1].UsedPct != nil || one.Windows[1].ResetsAt != nil {
		t.Fatalf("windows: unknown must be null: %+v", one.Windows)
	}
	if len(one.Leases) != 1 || one.Leases[0].Pane != "%7" || one.Leases[0].Workspace != "/invented/live" || one.Leases[0].PID != 1 {
		t.Fatalf("leases: %+v", one.Leases)
	}
	var keys []string
	for _, r := range body.Rankings {
		keys = append(keys, r.Key)
	}
	if strings.Join(keys, " ") != "claude/interactive claude/worker codex/interactive codex/worker" {
		t.Fatalf("ranking keys: %v", keys)
	}
	// The read released nothing: the dead lease is hidden, not pruned.
	st, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if left, _ := st.ListLeases(); len(left) != 2 {
		t.Fatalf("quota read pruned leases: %d left", len(left))
	}
}

// The handler's real document is what the browser model admits.
func TestNavQuotasDocumentAdmittedByBrowserModel(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node is required for the quota consumer contract in CI")
		}
		t.Skip("node is not installed; quota consumer contract did not run")
	}
	w := factoryGet(t, quotaFactory(t, quotaDeps(quotaStore(t))), "/api/nav/quotas")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	module, err := filepath.Abs("../../internal/daemon/ui/quota_data.js")
	if err != nil {
		t.Fatal(err)
	}
	script := `import {readFileSync} from 'node:fs';import assert from 'node:assert/strict';const {quotaSnapshot}=await import(process.argv[1]);
const s=quotaSnapshot(JSON.parse(readFileSync(0,'utf8')),{rows:10,arrayEntries:1000,stringUnits:100000});
assert.deepEqual(s.providers.map(p=>p.provider),['claude','codex']);const a=s.providers[0].accounts[0];
assert.equal(a.id,'claude/one');assert.equal(a.windows[0].used,42.5);assert.equal(a.windows[0].remaining,57.5);assert.equal(a.windows[1].used,null);assert.equal(a.windows[1].remaining,null);
assert.equal(a.extraUsage.balanceMinor,null);assert.equal(a.extraUsage.usedMinor,250);assert.equal(a.leases.length,1);assert.equal(a.leases[0].pane,'%7');assert.equal(s.poll.lastAt,null);
assert.deepEqual(s.rankings.map(r=>r.key),['claude/interactive','claude/worker','codex/interactive','codex/worker']);console.log('quota consumer OK');`
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "--input-type=module", "-e", script, (&url.URL{Scheme: "file", Path: module}).String())
	cmd.WaitDelay = 2 * time.Second
	cmd.Stdin = strings.NewReader(w.Body.String())
	out, err := cmd.CombinedOutput()
	if err != nil || string(out) != "quota consumer OK\n" {
		t.Fatalf("consumer: %s %v", out, err)
	}
}

func TestNavQuotasRefusals(t *testing.T) {
	path := quotaStore(t)
	f := quotaFactory(t, quotaDeps(path))
	for _, target := range []string{"/api/nav/quotas?x=1", "/api/nav/quotas?"} {
		if w := factoryGet(t, f, target); w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_request") {
			t.Fatalf("%s: %d %s", target, w.Code, w.Body.String())
		}
	}
	if r := factoryGet(t, quotaFactory(t, nil), "/api/nav/quotas"); r.Code != 503 || !strings.Contains(r.Body.String(), "quotas_unavailable") {
		t.Fatalf("no deps: %d %s", r.Code, r.Body.String())
	}
	failing := quotaDeps(path)
	failing.host = func(context.Context) (string, error) { return "", pool.ErrIdentityUnavailable }
	if r := factoryGet(t, quotaFactory(t, failing), "/api/nav/quotas"); r.Code != 503 || !strings.Contains(r.Body.String(), "quotas_unavailable") {
		t.Fatalf("no identity: %d %s", r.Code, r.Body.String())
	}
	// The factory insists its budgets agree, so narrow them after it is built.
	rows := quotaFactory(t, quotaDeps(path))
	rows.policy.sessions.arrayEntries = 3
	if r := factoryGet(t, rows, "/api/nav/quotas"); r.Code != 503 || !strings.Contains(r.Body.String(), "quotas_too_large") {
		t.Fatalf("rows: %d %s", r.Code, r.Body.String())
	}
	small := quotaFactory(t, quotaDeps(path))
	small.policy.sessions.bodyBytes = 256
	if r := factoryGet(t, small, "/api/nav/quotas"); r.Code != 503 || !strings.Contains(r.Body.String(), "quotas_too_large") {
		t.Fatalf("body: %d %s", r.Code, r.Body.String())
	}
}

// One slot: a second request waits for it, and gives up at the timeout
// without starting a read of its own.
func TestNavQuotasSlotAndCancellation(t *testing.T) {
	path := quotaStore(t)
	reads := 0
	deps := quotaDeps(path)
	read := deps.read
	deps.read = func(ctx context.Context, l state.QuotaSnapshotLimits) (*state.QuotaSnapshot, error) {
		reads++
		return read(ctx, l)
	}
	f := quotaFactory(t, deps)
	f.policy.timeout = 50 * time.Millisecond
	f.quotaSlots <- struct{}{}
	if r := factoryGet(t, f, "/api/nav/quotas"); r.Code != 503 || !strings.Contains(r.Body.String(), "quotas_busy") || reads != 0 {
		t.Fatalf("busy: %d %s reads=%d", r.Code, r.Body.String(), reads)
	}
	<-f.quotaSlots
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/nav/quotas", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	f.api.ServeHTTP(w, req)
	if w.Code != 503 || !strings.Contains(w.Body.String(), "request_cancelled") {
		t.Fatalf("cancelled: %d %s", w.Code, w.Body.String())
	}
	if len(f.quotaSlots) != 0 {
		t.Fatal("slot not released")
	}
}

func TestNavJSONSizeFloatsAndNullables(t *testing.T) {
	one, half := int64(7), 0.5
	for _, v := range []any{0.0, -0.0, 1.5, 42.0, 1e21, 1e-7, 123456789.125, math.MaxFloat64, math.SmallestNonzeroFloat64,
		(*int64)(nil), &one, (*float64)(nil), &half, (*navQuotaExtra)(nil), &navQuotaExtra{Currency: "USD", BalanceMinor: &one}} {
		encoded, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if got, ok := navJSONSize(v, int64(len(encoded))); !ok || got != int64(len(encoded)) {
			t.Fatalf("%T %v: %d/%v, want %d", v, v, got, ok, len(encoded))
		}
	}
	for _, v := range []any{math.NaN(), math.Inf(1), (*string)(nil), float32(1)} {
		if _, ok := navJSONSize(v, 1<<20); ok {
			t.Fatalf("accepted %T %v", v, v)
		}
	}
}
