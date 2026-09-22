package agy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/state"
)

func TestSession(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"--conversation", "abc"}, "abc"},
		{[]string{"--dangerously-skip-permissions", "--conversation=abc"}, "abc"},
		{[]string{"--continue"}, ""},
		{[]string{"-c"}, ""},
		{[]string{"--conversation"}, ""},
		{[]string{"-p", "hi"}, ""},
		{nil, ""},
	}
	for _, c := range cases {
		if got := Session(c.in); got != c.want {
			t.Errorf("%v: got %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPassthrough(t *testing.T) {
	for _, args := range [][]string{{"models"}, {"update"}, {"mcp", "list"}, {"--version"}, {"plugin", "list"}} {
		if !Passthrough(args) {
			t.Errorf("%v should pass through", args)
		}
	}
	for _, args := range [][]string{{"-p", "hi"}, {"--conversation", "x"}, nil, {"--model", "gemini-3.8-flash-low"}} {
		if Passthrough(args) {
			t.Errorf("%v should be routed", args)
		}
	}
}

func TestModelArgs(t *testing.T) {
	cases := []struct {
		model, level string
		want         string
	}{
		{"", "", ""},
		{"", "high", "--effort high"},
		{"gemini-3.8-flash-medium", "", "--model gemini-3.8-flash-medium"},
		{"gemini-3.8-flash-medium", "low", "--model gemini-3.8-flash-low"},
		{"gemini-3.1-pro-high", "medium", "--model gemini-3.1-pro-high"},
		{"gemini-3.1-pro-high", "low", "--model gemini-3.1-pro-low"},
		{"claude-opus-4-6-thinking", "high", "--model claude-opus-4-6-thinking"},
		{"gpt-oss-120b-medium", "", "--model gpt-oss-120b-medium"},
	}
	for _, c := range cases {
		if got := strings.Join(ModelArgs(c.model, c.level), " "); got != c.want {
			t.Errorf("%q %q: got %q, want %q", c.model, c.level, got, c.want)
		}
	}
	if !SameModel("gemini-3.8-flash-high", "gemini-3.8-flash-medium") || SameModel("gemini-3.8-flash-high", "gemini-3.7-flash-high") {
		t.Error("SameModel")
	}
}

func TestTrustWorkspace(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(AppDataDir(dir), 0o700)
	os.WriteFile(SettingsPath(dir), []byte(`{"model":"x","trustedWorkspaces":["/a"]}`), 0o600)
	if changed, err := TrustWorkspace(dir, "/b"); err != nil || !changed {
		t.Fatalf("trust: %v %v", changed, err)
	}
	if changed, err := TrustWorkspace(dir, "/b"); err != nil || changed {
		t.Fatalf("second trust must be a no-op: %v %v", changed, err)
	}
	data, _ := os.ReadFile(SettingsPath(dir))
	var doc map[string]any
	json.Unmarshal(data, &doc)
	if list, _ := doc["trustedWorkspaces"].([]any); len(list) != 2 || list[0] != "/a" || list[1] != "/b" || doc["model"] != "x" {
		t.Fatalf("settings: %s", data)
	}
}

func TestScopeOf(t *testing.T) {
	cases := map[string]string{
		"":                             "",
		"Gemini 3.8 Flash (Medium)":    "",
		"gemini-3.1-pro-high":          "",
		"claude-opus-4-6-thinking":     "3p",
		"Claude Sonnet 4.6 (Thinking)": "3p",
		"gpt-oss-120b-medium":          "3p",
	}
	for model, want := range cases {
		if got := ScopeOf(model); got != want {
			t.Errorf("%q: got %q, want %q", model, got, want)
		}
	}
}

const statusPayload = `{"cwd":"/w","session_id":"","conversation_id":"","model":{"id":"Gemini 3.8 Flash (Medium)","display_name":"Gemini 3.8 Flash (Medium)"},
"version":"1.2.8","product":"antigravity","agent_state":"initializing","plan_tier":"Google AI Pro","email":"dev@example.com",
"quota":{"3p-5h":{"remaining_fraction":1,"reset_time":"2026-09-22T20:30:18Z","reset_in_seconds":17999},
"3p-weekly":{"remaining_fraction":0.5,"reset_time":"2026-09-29T15:30:18Z","reset_in_seconds":604799},
"gemini-5h":{"remaining_fraction":0.9993881,"reset_time":"2026-09-22T20:29:52Z","reset_in_seconds":17973},
"gemini-weekly":{"remaining_fraction":0,"reset_time":"2026-09-29T15:29:52Z","reset_in_seconds":604773}}}`

func TestParseStatusAndWindows(t *testing.T) {
	if _, ok := ParseStatus([]byte(`{"agent_state":"authenticating","quota":null}`)); ok {
		t.Fatal("a payload without quota is not usable")
	}
	st, ok := ParseStatus([]byte(statusPayload))
	if !ok || st.Email != "dev@example.com" || st.Plan != "Google AI Pro" || st.Model != "Gemini 3.8 Flash (Medium)" {
		t.Fatalf("parse: ok=%v %+v", ok, st)
	}
	now := time.Date(2026, 9, 22, 15, 30, 0, 0, time.UTC)
	ws := st.Windows(now)
	byKey := map[string]state.Window{}
	for _, w := range ws {
		byKey[w.Key] = w
	}
	if len(ws) != 4 {
		t.Fatalf("want 4 windows, got %d: %+v", len(ws), ws)
	}
	g5 := byKey["gemini:5h"]
	if g5.Kind != state.KindShort || g5.Scope != "" || g5.WindowSeconds != 5*3600 || g5.Label != "5h" || g5.UsedPct < 0.06 || g5.UsedPct > 0.07 {
		t.Errorf("gemini 5h: %+v", g5)
	}
	if g5.ResetsAt != time.Date(2026, 9, 22, 20, 29, 52, 0, time.UTC).Unix() {
		t.Errorf("gemini 5h reset: %d", g5.ResetsAt)
	}
	gw := byKey["gemini:weekly"]
	if gw.Kind != state.KindWeekly || gw.UsedPct != 100 || gw.Severity != "critical" || gw.Scope != "" {
		t.Errorf("gemini weekly: %+v", gw)
	}
	tp := byKey["3p:weekly"]
	if tp.Scope != "3p" || tp.UsedPct != 50 || tp.Kind != state.KindWeekly || !strings.HasPrefix(tp.Label, "3p") {
		t.Errorf("3p weekly: %+v", tp)
	}
	if byKey["3p:5h"].UsedPct != 0 {
		t.Errorf("3p 5h: %+v", byKey["3p:5h"])
	}
	line := DefaultStatusLine("main", st, true)
	if !strings.Contains(line, "agy/main") || !strings.Contains(line, "5h 0%") || !strings.Contains(line, "wk 100%") || strings.Contains(line, "3p") {
		t.Errorf("status line: %q", line)
	}
}

func TestWindowsFallBackToResetInSeconds(t *testing.T) {
	st := Status{Quota: map[string]Bucket{"gemini-5h": {RemainingFraction: 0.25, ResetInSeconds: 600}}}
	now := time.Unix(1_000_000, 0)
	ws := st.Windows(now)
	if len(ws) != 1 || ws[0].ResetsAt != 1_000_600 || ws[0].UsedPct != 75 {
		t.Fatalf("%+v", ws)
	}
}

func TestCredentialAndEnv(t *testing.T) {
	home := t.TempDir()
	if !HasCredential(home, true) {
		t.Fatal("a native home always counts as logged in")
	}
	// A HOME-style account counts once its login marker exists.
	fake := t.TempDir()
	if HasCredential(fake, false) {
		t.Fatal("an overlay home without a marker is not logged in")
	}
	if err := MarkLoggedIn(fake, "other@example.com"); err != nil {
		t.Fatal(err)
	}
	if !HasCredential(fake, false) {
		t.Fatal("overlay login not seen")
	}
	if GeminiDir(fake, false) != filepath.Join(fake, ".gemini") || GeminiDir(home, true) != home {
		t.Fatal("gemini dir placement")
	}
	p := &Provider{}
	env := p.Env(fake, false, false)
	if !contains(env, "HOME="+fake) {
		t.Fatal("overlay launch must set HOME")
	}
	if contains(p.Env(home, true, false), "HOME="+home) {
		t.Fatal("native launch must leave HOME alone")
	}
}

func contains(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

func TestInstallHooks(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(ConfigDir(dir), 0o700)
	os.WriteFile(HooksPath(dir), []byte(`{"mine":{"PostToolUse":[{"matcher":"run_command","hooks":[{"type":"command","command":"./lint"}]}]}}`), 0o600)
	changed, err := InstallHooks(dir, "/usr/local/bin/aiq")
	if err != nil || !changed {
		t.Fatalf("install: changed=%v err=%v", changed, err)
	}
	changed, err = InstallHooks(dir, "/usr/local/bin/aiq")
	if err != nil || changed {
		t.Fatalf("second install must be a no-op: changed=%v err=%v", changed, err)
	}
	changed, err = InstallHooks(dir, "/opt/aiq")
	if err != nil || !changed {
		t.Fatalf("a moved binary re-installs: changed=%v err=%v", changed, err)
	}
	data, _ := os.ReadFile(HooksPath(dir))
	var doc map[string]map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["mine"] == nil {
		t.Fatal("the user's own hook was lost")
	}
	aiq := doc["aiq-long"]
	if aiq["PreInvocation"] == nil || aiq["Stop"] == nil || !strings.Contains(string(data), "/opt/aiq agy-hook stop") {
		t.Fatalf("aiq hooks: %s", data)
	}
	if err := UninstallHooks(dir); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(HooksPath(dir))
	if strings.Contains(string(data), "aiq-long") || !strings.Contains(string(data), "mine") {
		t.Fatalf("uninstall: %s", data)
	}
	// A missing file is created with just aiq's entry.
	fresh := t.TempDir()
	if _, err := InstallHooks(fresh, "/opt/aiq"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(HooksPath(fresh)); err != nil {
		t.Fatal(err)
	}
}

func TestInstallStatusline(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(AppDataDir(dir), 0o700)
	os.WriteFile(SettingsPath(dir), []byte(`{"model":"Gemini 3.8 Flash (Medium)","permissions":{"allow":["command(npx)"]}}`), 0o600)
	if DefaultModel(dir) != "Gemini 3.8 Flash (Medium)" {
		t.Fatalf("default model: %q", DefaultModel(dir))
	}
	prev, err := InstallStatusline(dir, "/opt/aiq")
	if err != nil || prev != "" {
		t.Fatalf("install: prev=%q err=%v", prev, err)
	}
	cmd, installed, err := InstalledStatusline(dir)
	if err != nil || !installed || cmd != "/opt/aiq agy-statusline" {
		t.Fatalf("installed: %q %v %v", cmd, installed, err)
	}
	data, _ := os.ReadFile(SettingsPath(dir))
	var doc map[string]any
	json.Unmarshal(data, &doc)
	sl := doc["statusLine"].(map[string]any)
	if sl["stack_with_default"] != true || doc["model"] != "Gemini 3.8 Flash (Medium)" || doc["permissions"] == nil {
		t.Fatalf("settings after install: %s", data)
	}
	if err := UninstallStatusline(dir, ""); err != nil {
		t.Fatal(err)
	}
	if _, installed, _ := InstalledStatusline(dir); installed {
		t.Fatal("still installed after uninstall")
	}
	data, _ = os.ReadFile(SettingsPath(dir))
	if strings.Contains(string(data), "statusLine") {
		t.Fatalf("statusLine block should be gone: %s", data)
	}

	// A user's own command is chained, not replaced, and not stacked.
	os.WriteFile(SettingsPath(dir), []byte(`{"statusLine":{"type":"command","command":"~/mine.sh","padding":1}}`), 0o600)
	prev, err = InstallStatusline(dir, "/opt/aiq")
	if err != nil || prev != "~/mine.sh" {
		t.Fatalf("install over user's: prev=%q err=%v", prev, err)
	}
	data, _ = os.ReadFile(SettingsPath(dir))
	json.Unmarshal(data, &doc)
	sl = doc["statusLine"].(map[string]any)
	if _, stacked := sl["stack_with_default"]; stacked || sl["padding"] == nil {
		t.Fatalf("settings after install over user's: %s", data)
	}
	if prev, err := InstallStatusline(dir, "/opt/aiq"); err != nil || prev != "" {
		t.Fatalf("reinstall must not report aiq as the previous command: %q %v", prev, err)
	}
	if err := UninstallStatusline(dir, "~/mine.sh"); err != nil {
		t.Fatal(err)
	}
	if cmd, installed, _ := InstalledStatusline(dir); installed || cmd != "~/mine.sh" {
		t.Fatalf("uninstall restored %q", cmd)
	}
	// No settings file at all.
	if _, installed, err := InstalledStatusline(t.TempDir()); installed || err != nil {
		t.Fatal("missing settings must read as not installed")
	}
}
