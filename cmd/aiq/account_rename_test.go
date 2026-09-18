package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/provider/claude"
	"github.com/orlenko/aiq/internal/state"
)

func renameApp(t *testing.T) *app {
	t.Helper()
	root := t.TempDir()
	t.Setenv("AIQ_DATA_DIR", root)
	t.Setenv("AIQ_CONFIG", filepath.Join(root, "config.toml"))
	st, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &app{cfg: config.Default(), st: st}
}

func addTestAccount(t *testing.T, a *app, provider, name string) state.Account {
	t.Helper()
	acc := state.Account{ID: state.AccountID(provider, name), Provider: provider, Name: name,
		Enabled: true, Home: homeFor(provider, name), CreatedAt: 1}
	if err := os.MkdirAll(acc.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := a.st.AddAccount(acc); err != nil {
		t.Fatal(err)
	}
	return acc
}

func TestAccountRename(t *testing.T) {
	a := renameApp(t)
	old := addTestAccount(t, a, "codex", "codex2")
	addTestAccount(t, a, "codex", "taken")
	if err := os.WriteFile(filepath.Join(old.Home, "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	a.cfg.Display.Labels[old.ID] = "Codex 2"
	a.cfg.Display.Order = []string{"codex/taken", old.ID}
	a.st.SetAffinity("codex", "/w", old.ID, now)
	a.st.UpsertWindow(state.Window{AccountID: old.ID, Key: "session", Kind: state.KindShort, UsedPct: 40})
	a.st.LogEvent("codex", old.ID, "login", "x", now)

	for _, bad := range []string{"taken", "claude/work", "a b", ".hidden", "codex2"} {
		if err := a.accountRename(old.ID, bad, false); err == nil {
			t.Errorf("rename to %q accepted", bad)
		}
	}
	lease, err := a.st.AddLease(state.Lease{AccountID: old.ID, PID: os.Getpid(), Hostname: pool.Hostname(),
		Mode: "interactive", StartedAt: now.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.accountRename(old.ID, "work", false); err == nil || !strings.Contains(err.Error(), "running sessions") {
		t.Fatalf("rename with a live session: err = %v", err)
	}
	a.st.ReleaseLease(lease)

	if err := a.accountRename(old.ID, "codex/work", false); err != nil {
		t.Fatal(err)
	}
	acc, err := a.st.GetAccount("codex/work")
	if err != nil {
		t.Fatal(err)
	}
	if acc.Name != "work" || acc.Home != homeFor("codex", "work") {
		t.Fatalf("renamed account = %+v", acc)
	}
	if _, err := a.st.GetAccount(old.ID); err == nil {
		t.Fatal("old id still exists")
	}
	if _, err := os.Stat(filepath.Join(acc.Home, "auth.json")); err != nil {
		t.Fatalf("home not moved: %v", err)
	}
	if _, err := os.Stat(old.Home); !os.IsNotExist(err) {
		t.Fatalf("old home still there: %v", err)
	}
	if got, _ := a.st.GetAffinity("codex", "/w"); got != "codex/work" {
		t.Errorf("affinity = %q", got)
	}
	if w, _ := a.st.ListWindows("codex/work"); len(w) != 1 {
		t.Errorf("windows = %v", w)
	}
	events, _ := a.st.ListEvents(10)
	for _, e := range events {
		if e.AccountID == old.ID {
			t.Errorf("event still under the old id: %+v", e)
		}
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Display.Labels["codex/work"] != "Codex 2" || cfg.Display.Labels[old.ID] != "" {
		t.Errorf("labels = %v", cfg.Display.Labels)
	}
	if strings.Join(cfg.Display.Order, " ") != "codex/taken codex/work" {
		t.Errorf("order = %v", cfg.Display.Order)
	}
}

func TestAccountRenameKeepsOtherHomes(t *testing.T) {
	a := renameApp(t)
	native := state.Account{ID: "claude/me", Provider: "claude", Name: "me", Enabled: true,
		Home: t.TempDir(), Native: true, CreatedAt: 1}
	if err := a.st.AddAccount(native); err != nil {
		t.Fatal(err)
	}
	if err := a.accountRename(native.ID, "vlad", false); err != nil {
		t.Fatal(err)
	}
	if acc, _ := a.st.GetAccount("claude/vlad"); acc.Home != native.Home {
		t.Errorf("native home moved to %s", acc.Home)
	}
	acc := addTestAccount(t, a, "codex", "one")
	if err := a.accountRename(acc.ID, "two", true); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.st.GetAccount("codex/two"); got.Home != acc.Home {
		t.Errorf("--keep-home moved the home to %s", got.Home)
	}
}

func TestAccountRenameCarriesKeychainLogin(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the keychain credential exists on macOS only")
	}
	// A fake security(1) keeps items as files named after their service.
	bin, kc := t.TempDir(), t.TempDir()
	script := `#!/bin/sh
kc="$AIQ_TEST_KEYCHAIN"
case "$1" in
find-generic-password)
  [ -f "$kc/$3" ] || exit 44
  if [ "$4" = "-w" ]; then cat "$kc/$3"; echo; else printf '    "acct"<blob>="tester"\n    "svce"<blob>="%s"\n' "$3"; fi ;;
delete-generic-password) rm "$kc/$3" || exit 44 ;;
-i)
  read -r line; printf '%s\n' "$line" >> "$kc/commands"
  svc=$(printf '%s' "$line" | sed 's/.* -s "\([^"]*\)".*/\1/')
  printf '%s' "$line" | sed 's/.* -X //' | xxd -r -p > "$kc/$svc" ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "security"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AIQ_TEST_KEYCHAIN", kc)

	a := renameApp(t)
	old := addTestAccount(t, a, "claude", "claude4")
	secret := `{"claudeAiOauth":{"accessToken":"sk-\"x\""}}`
	if err := os.WriteFile(filepath.Join(kc, claude.KeychainService(old.Home)), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.accountRename(old.ID, "fourth", false); err != nil {
		t.Fatal(err)
	}
	newHome := homeFor("claude", "fourth")
	got, err := os.ReadFile(filepath.Join(kc, claude.KeychainService(newHome)))
	if err != nil || string(got) != secret {
		t.Fatalf("new keychain item = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(kc, claude.KeychainService(old.Home))); !os.IsNotExist(err) {
		t.Errorf("old keychain item kept: %v", err)
	}
	cmds, _ := os.ReadFile(filepath.Join(kc, "commands"))
	if strings.Contains(string(cmds), "accessToken") || !strings.Contains(string(cmds), `-a "tester"`) {
		t.Errorf("security -i input = %s", cmds)
	}
}
