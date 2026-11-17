package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/daemon"
	"github.com/orlenko/aiq/internal/herdr"
	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/terminal"
)

// go test ./cmd/aiq -run TestAppGolden -update rewrites testdata/app.
var appUpdate = flag.Bool("update", false, "rewrite cmd/aiq/testdata/app golden files")

const (
	appTestRef     = herdr.Prefix + "0123456789abcdef0123456789abcdef-fedcba9876543210fedcba9876543210@2"
	appTestCore    = "0b1c2d3e4f5a69788796a5b4c3d2e1f0"
	appTestLabel   = "9e8d7c6b5a4938270123456789abcdef"
	appTestWork    = "/invented/work"
	appTestOtherWS = "/invented/other"
	appTestCodexID = "b2000000-1111-4222-8333-000000000002"
)

// appTestEnv is a bridge over invented stores: the live fixture's metadata
// (one long lease, here on a Herdr terminal), an asks store with two asks,
// and a fake core that verifies that terminal.
func appTestEnv(t *testing.T) (appEnv, *navAppFixture) {
	t.Helper()
	x, in, db := navLiveFixture(t)
	if _, err := db.Exec(`UPDATE leases SET pane=?`, appTestRef); err != nil {
		t.Fatal(err)
	}
	in.observe = func(_ context.Context, target terminal.ObservationTarget) terminal.Observation {
		if target.Ref != appTestRef {
			return terminal.Unknown
		}
		return terminal.Running
	}
	at := time.Date(2031, 7, 1, 12, 0, 0, 0, time.UTC)
	asks, err := sql.Open("sqlite", x.asksPath)
	if err != nil {
		t.Fatal(err)
	}
	defer asks.Close()
	for _, a := range []struct {
		id                               int64
		workspace, provider, session, st string
		raised, stateAt                  int64
		first, latest, reason            string
		reader                           float64
	}{
		{17, appTestWork, "claude", fullIDFixtureID, "open", at.Unix() - 3600, at.Unix() - 3600, "Should I keep the invented fixture or regenerate it?",
			"Shall I go ahead with option B and push the invented branch now, or wait for the review that is still running? " + strings.Repeat("Invented detail. ", 20), "", 0.9},
		{18, appTestOtherWS, "codex", appTestCodexID, "probably-done", at.Unix() - 7200, at.Unix() - 1800, "Run the invented migration?", "Run the invented migration?", "answered", 0.9},
	} {
		if _, err := asks.Exec(`INSERT INTO asks(id,chain,workspace,provider,session_id,raised_at,state,state_at,state_reason,first_quote,last_quote,last_seen,refs,repeats,asks_reader,risk)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,'[]',0,?,0)`, a.id, "invented-chain", a.workspace, a.provider, a.session, a.raised, a.st, a.stateAt, a.reason, a.first, a.latest, a.raised, a.reader); err != nil {
			t.Fatal(err)
		}
	}
	env := appEnv{version: "0.9.2+invented", now: func() time.Time { return at },
		identity: func(context.Context) (string, error) { return "invented-machine-id", nil },
		hostname: func() (string, error) { return "invented-host", nil },
		dataDir:  "/invented/aiq-data", asksPath: x.asksPath, statePath: filepath.Join(x.root, "metadata.db"), input: in,
		resolve: func(ref string) (herdr.Target, string) {
			if ref != appTestRef {
				return herdr.Target{}, herdr.TargetUnknown
			}
			return herdr.Target{CoreID: appTestCore, Terminal: "t7", PID: 4321, Label: appTestLabel}, ""
		}}
	return env, x
}

func appCall(t *testing.T, env appEnv, args ...string) map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := appRun(env, args, &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() > appMaxBytes || !bytes.HasSuffix(out.Bytes(), []byte("\n")) || bytes.Count(out.Bytes(), []byte("\n")) != 1 {
		t.Fatalf("not one bounded document: %d bytes", out.Len())
	}
	var doc map[string]any
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// appGolden compares (or with -update writes) one reply with its fixture.
func appGolden(t *testing.T, name string, env appEnv, x *navAppFixture, args ...string) map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := appRun(env, args, &out); err != nil {
		t.Fatal(err)
	}
	got := out.Bytes()
	if x != nil && bytes.Contains(got, []byte(x.root)) {
		t.Fatalf("%s carries a temporary path; fixtures must be stable", name)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, got, "", " "); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", "app", name+".json")
	if *appUpdate {
		if err := os.WriteFile(path, pretty.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pretty.Bytes(), want) {
		t.Fatalf("%s differs from %s; rerun with -update after checking the change\n%s", name, path, pretty.String())
	}
	var doc map[string]any
	json.Unmarshal(got, &doc)
	return doc
}

func appData(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	if doc["ok"] != true {
		t.Fatalf("not ok: %v", doc)
	}
	return doc["data"].(map[string]any)
}

func TestAppGolden(t *testing.T) {
	env, x := appTestEnv(t)
	hello := appGolden(t, "hello", env, nil, "hello")
	host := hello["host"].(map[string]any)["id"].(string)
	if len(host) != 32 {
		t.Fatal("host id", host)
	}

	asks := appData(t, appGolden(t, "asks", env, x, "asks"))
	rows := asks["rows"].([]any)
	first := rows[0].(map[string]any)
	if first["section"] != "asks" || first["terminal"].(map[string]any)["relation"] != "origin" || first["latest"].(map[string]any)["truncated"] != true {
		t.Fatalf("ask row %v", first)
	}
	if second := rows[1].(map[string]any); second["terminal"] != nil || second["terminal_reason"] != appNoLease {
		t.Fatalf("unmapped ask got a terminal %v", second)
	}
	revision := first["revision"].(string)

	detail := appData(t, appGolden(t, "ask-detail", env, x, "ask-detail", "--id", "17"))
	if detail["latest"].(map[string]any)["truncated"] != false || !strings.HasSuffix(detail["latest"].(map[string]any)["text"].(string), "Invented detail. ") {
		t.Fatal("ask detail is not the full question", detail)
	}
	appGolden(t, "target", env, x, "target", "--ask", "17", "--revision", revision)
	appGolden(t, "ask-action.conflict", env, x, "ask-action", "--id", "17", "--revision", strings.Repeat("0", 64), "--action", "dismiss", "--host", host)
	done := appData(t, appGolden(t, "ask-action.ok", env, x, "ask-action", "--id", "17", "--revision", revision, "--action", "dismiss", "--host", host))
	if done["state"] != "dismissed" || done["revision"] == revision {
		t.Fatal("dismiss", done)
	}
	// A replay of the applied action is a conflict, never a second write.
	if replay := appCall(t, env, "ask-action", "--id", "17", "--revision", revision, "--action", "dismiss", "--host", host); replay["error"].(map[string]any)["code"] != appConflict {
		t.Fatal("replay", replay)
	}

	missing := env
	missing.asksPath = filepath.Join(x.root, "never", "asks.db")
	appGolden(t, "asks.not_set_up", missing, x, "asks")
	if _, err := os.Stat(missing.asksPath); !os.IsNotExist(err) {
		t.Fatal("reading asks created a store")
	}
	appGolden(t, "error.invalid_request", env, x, "ask-action", "--id", "17")

	q := env
	q.statePath = appQuotaStore(t)
	q.quotas = appQuotaDeps(q.statePath)
	quotas := appData(t, appGolden(t, "quotas", q, x, "quotas"))
	if quotas["poll"].(map[string]any)["known"] != false || quotas["machines"] != nil {
		t.Fatal("quotas claimed daemon or fleet facts", quotas)
	}
}

func TestAppRefusals(t *testing.T) {
	env, _ := appTestEnv(t)
	host := appCall(t, env, "hello")["host"].(map[string]any)["id"].(string)
	revision := strings.Repeat("a", 64)
	for _, args := range [][]string{
		{}, {"app"}, {"unknown"}, {"hello", "extra"}, {"asks", "--id", "1"},
		{"ask-detail"}, {"ask-detail", "--id", "0"}, {"ask-detail", "--id", "1", "--id", "1"}, {"ask-detail", "--id=1"},
		{"ask-action", "--id", "17", "--revision", revision, "--action", "close", "--host", host},
		{"ask-action", "--id", "17", "--revision", revision, "--action", "dismiss", "--host", strings.Repeat("b", 32)},
		{"ask-action", "--id", "17", "--revision", "ABC", "--action", "dismiss", "--host", host},
		{"target"}, {"target", "--ask", "17"}, {"target", "--ask", "17", "--revision", revision, "--provider", "claude", "--conversation", fullIDFixtureID},
		{"target", "--provider", "copilot", "--conversation", fullIDFixtureID}, {"target", "--provider", "claude", "--conversation", "../x"},
	} {
		doc := appCall(t, env, args...)
		if doc["ok"] != false || doc["error"].(map[string]any)["code"] != appInvalid {
			t.Fatalf("%q: %v", args, doc)
		}
	}
	noID := env
	noID.identity = func(context.Context) (string, error) { return "", pool.ErrIdentityUnavailable }
	if doc := appCall(t, noID, "ask-action", "--id", "17", "--revision", revision, "--action", "dismiss", "--host", host); doc["error"].(map[string]any)["code"] != appInvalid {
		t.Fatal("action ran without a source identity", doc)
	}
}

func TestAppTargetReasons(t *testing.T) {
	env, _ := appTestEnv(t)
	conv := func(e appEnv) map[string]any {
		return appData(t, appCall(t, e, "target", "--provider", "claude", "--conversation", fullIDFixtureID))
	}
	if got := conv(env); got["terminal"].(map[string]any)["witness"].(map[string]any)["label"] != appTestLabel {
		t.Fatal("verified conversation target", got)
	}
	label := env
	label.resolve = func(string) (herdr.Target, string) { return herdr.Target{}, herdr.TargetLabelMissing }
	if got := conv(label); got["terminal"] != nil || got["terminal_reason"] != herdr.TargetLabelMissing {
		t.Fatal("unverified target exported", got)
	}
	ended := env
	ended.input.observe = func(context.Context, terminal.ObservationTarget) terminal.Observation { return terminal.Ended }
	if got := conv(ended); got["terminal"] != nil || got["terminal_reason"] != appEnded {
		t.Fatal("ended session targeted", got)
	}
	if got := appData(t, appCall(t, env, "target", "--provider", "codex", "--conversation", appTestCodexID)); got["terminal"] != nil || got["terminal_reason"] != appNoLease {
		t.Fatal("conversation without a lease targeted", got)
	}
	noMeta := env
	noMeta.input.metadata = func(context.Context, state.NavigatorSnapshotLimits) (*state.NavigatorSnapshot, error) {
		return nil, state.ErrNavigatorSnapshotUnavailable
	}
	if got := conv(noMeta); got["terminal"] != nil || got["terminal_reason"] != appUnknownTerm {
		t.Fatal("missing metadata targeted", got)
	}
}

func TestAppAsksStayUnderCap(t *testing.T) {
	env, x := appTestEnv(t)
	db, err := sql.Open("sqlite", x.asksPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	quote := strings.Repeat("invented question text ", 40)
	for i := 0; i < 50000-2; i++ { // with the two fixture asks: the reader's 50,000-row limit
		if _, err := tx.Exec(`INSERT INTO asks(id,chain,workspace,provider,session_id,raised_at,state,state_at,first_quote,last_quote,last_seen,refs,repeats)
			VALUES(?,?,'/invented/bulk','claude','',1,'closed',1,?,?,1,'[]',0)`, 1000+i, "c"+strconv.Itoa(i), quote, quote); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// This checks the output cap at the store's maximum row count. The
	// production wall-clock limit is deliberately separate: race instrumentation
	// makes these 50,000 invented rows substantially slower to read.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	encoded, err := env.encode("asks", env.host(ctx), env.asks(ctx))
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > appMaxBytes {
		t.Fatal("reply exceeded cap", len(encoded))
	}
	var doc map[string]any
	if err := json.Unmarshal(encoded, &doc); err != nil {
		t.Fatal(err)
	}
	data := appData(t, doc)
	counts := data["counts"].(map[string]any)
	if doc["partial"] != true || doc["omitted"].(map[string]any)["rows"].(float64) <= 0 || counts["closed_dismissed"].(float64) != 50000-2 || counts["open"].(float64) != 1 {
		t.Fatal("cap", doc["partial"], doc["omitted"], counts)
	}
	if data["rows"].([]any)[0].(map[string]any)["id"] != "17" {
		t.Fatal("open asks must come first")
	}
}

// appQuotaStore is an invented state store at fixed times.
func appQuotaStore(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	at := time.Date(2031, 7, 1, 11, 59, 0, 0, time.UTC)
	if err := st.AddAccount(state.Account{ID: "alpha", Provider: "claude", Name: "alpha", Enabled: true, Home: "/invented/HIDDEN-HOME", Identity: "alpha@example.invalid", Priority: 100}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetUsageMeta("alpha", "max", 1, "", at); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertWindow(state.Window{AccountID: "alpha", Key: "five_hour", Label: "5h", Kind: state.KindShort, UsedPct: 38, ResetsAt: at.Add(time.Hour).Unix(), Source: "probe", ObservedAt: at.Unix()}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddLease(state.Lease{AccountID: "alpha", PID: 1, Hostname: "invented-host", Mode: state.ModeLong, Cwd: appTestWork, Workspace: appTestWork, Pane: "%7", Args: "HIDDEN-ARGS", StartedAt: at.Unix()}); err != nil {
		t.Fatal(err)
	}
	return path
}

func appQuotaDeps(path string) *navQuotaDeps {
	d := quotaDeps(path)
	d.scope = func(context.Context, string) string { return "" }
	d.poll = func() daemon.PollStatus { return daemon.PollStatus{} }
	d.view = (&pool.Pool{Cfg: config.Default()}).ViewFrom
	return d
}
