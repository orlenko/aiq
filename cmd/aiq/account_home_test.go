package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/orlenko/aiq/internal/state"
)

// homeApp is renameApp with every home a provider or overlay could write to in
// a temporary directory, and no provider binary to find.
func homeApp(t *testing.T) *app {
	t.Helper()
	a := renameApp(t)
	for _, v := range []string{"HOME", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "COPILOT_HOME", "PATH"} {
		t.Setenv(v, t.TempDir())
	}
	return a
}

// sharedPair records two accounts on one home, written to state directly.
func sharedPair(t *testing.T, a *app) (state.Account, state.Account) {
	t.Helper()
	first := addTestAccount(t, a, "codex", "work")
	second := first
	second.ID, second.Name = "codex/other", "other"
	if err := a.st.AddAccount(second); err != nil {
		t.Fatal(err)
	}
	return first, second
}

func accountIDs(t *testing.T, a *app) []string {
	t.Helper()
	accounts, err := a.st.ListAccounts("")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, acc := range accounts {
		ids = append(ids, acc.ID+"="+acc.Home)
	}
	slices.Sort(ids)
	return ids
}

func stdoutOf(t *testing.T, f func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()
	ferr := f()
	w.Close()
	out, _ := io.ReadAll(r)
	return string(out), ferr
}

func TestAccountAddRefusesARecordedHome(t *testing.T) {
	a := homeApp(t)
	acc := addTestAccount(t, a, "codex", "work")
	login := filepath.Join(acc.Home, "auth.json")
	if err := os.WriteFile(login, []byte(`{"who":"work"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := stdoutOf(t, func() error { return a.accountRename(acc.ID, "new", true) }); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml"), []byte("model = \"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.cfg.Display.Order = []string{"codex/new"}
	before := accountIDs(t, a)
	order := slices.Clone(a.cfg.Display.Order)
	listing := dirNames(t, acc.Home)
	_, err := stdoutOf(t, func() error { return a.accountAdd("codex", "work", false) })
	if err == nil || !strings.HasSuffix(err.Error(), "is already the home of codex/new; pick another name") {
		t.Fatalf("add: %v", err)
	}
	if got := accountIDs(t, a); !slices.Equal(got, before) {
		t.Errorf("accounts %v, were %v", got, before)
	}
	if !slices.Equal(a.cfg.Display.Order, order) {
		t.Errorf("order %v, was %v", a.cfg.Display.Order, order)
	}
	if b, _ := os.ReadFile(login); string(b) != `{"who":"work"}` {
		t.Errorf("the home's login is now %q", b)
	}
	if got := dirNames(t, acc.Home); !slices.Equal(got, listing) {
		t.Errorf("the home holds %v, held %v", got, listing)
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// A rename never moves a home onto one another account records, even one
// missing on disk, nor moves a home another account still uses.
func TestAccountRenameRefusesAnotherAccountsHome(t *testing.T) {
	a := homeApp(t)
	acc := addTestAccount(t, a, "codex", "one")
	elsewhere := state.Account{ID: "codex/two", Provider: "codex", Name: "two", Enabled: true,
		Home: homeFor("codex", "three"), CreatedAt: 1}
	if err := a.st.AddAccount(elsewhere); err != nil {
		t.Fatal(err)
	}
	before := accountIDs(t, a)
	_, err := stdoutOf(t, func() error { return a.accountRename(acc.ID, "three", false) })
	if err == nil || !strings.Contains(err.Error(), "is already the home of codex/two; pick another name or pass --keep-home") {
		t.Fatalf("onto a recorded home: %v", err)
	}
	if got := accountIDs(t, a); !slices.Equal(got, before) {
		t.Errorf("accounts %v, were %v", got, before)
	}

	b := homeApp(t)
	first, _ := sharedPair(t, b)
	before = accountIDs(t, b)
	_, err = stdoutOf(t, func() error { return b.accountRename(first.ID, "moved", false) })
	if err == nil || !strings.Contains(err.Error(), "is also the home of codex/other; pass --keep-home") {
		t.Fatalf("a shared home: %v", err)
	}
	if _, err := os.Stat(first.Home); err != nil {
		t.Errorf("the shared home: %v", err)
	}
	if got := accountIDs(t, b); !slices.Equal(got, before) {
		t.Errorf("accounts %v, were %v", got, before)
	}
}

func TestAccountRemoveOfAPairNamesTheOther(t *testing.T) {
	a := homeApp(t)
	first, _ := sharedPair(t, a)
	out, err := stdoutOf(t, func() error { return a.accountRemove(first.ID, false) })
	if err != nil || !strings.Contains(out, ": codex/other uses it too\n") || strings.Contains(out, "--purge") {
		t.Errorf("output %q, %v", out, err)
	}
}

func TestAccountRemovePurgeKeepsASharedHome(t *testing.T) {
	a := homeApp(t)
	first, second := sharedPair(t, a)
	out, err := stdoutOf(t, func() error { return a.accountRemove(first.ID, true) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "home kept at "+shortHome(first.Home)+": codex/other uses it too\n") || strings.Contains(out, "--purge") {
		t.Errorf("output: %q", out)
	}
	if _, err := os.Stat(first.Home); err != nil {
		t.Errorf("the shared home: %v", err)
	}
	if got, err := a.st.GetAccount(second.ID); err != nil || got.Home != second.Home {
		t.Errorf("the other account: %+v, %v", got, err)
	}
	// With the pair gone, the last one's purge deletes the home.
	out, err = stdoutOf(t, func() error { return a.accountRemove(second.ID, true) })
	if _, statErr := os.Stat(second.Home); err != nil || !os.IsNotExist(statErr) {
		t.Errorf("last purge: %v, %q, home %v", err, out, statErr)
	}
}

func TestAccountImportRefusesARecordedHome(t *testing.T) {
	a := homeApp(t)
	acc := addTestAccount(t, a, "codex", "work")
	login := filepath.Join(acc.Home, "auth.json")
	if err := os.WriteFile(login, []byte(`{"who":"work"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := stdoutOf(t, func() error { return a.accountRename(acc.ID, "new", true) }); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	creds := filepath.Join(dir, "codex-work.json")
	if err := os.WriteFile(creds, []byte(`{"who":"imported"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(map[string]any{"accounts": []map[string]string{{"id": "codex-work", "provider": "codex", "credentials": creds}}})
	a.cfg.Aiquota.Config = filepath.Join(dir, "config.json")
	a.cfg.Aiquota.Snapshot = filepath.Join(dir, "missing.json")
	if err := os.WriteFile(a.cfg.Aiquota.Config, cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	before := accountIDs(t, a)
	_, err := stdoutOf(t, func() error { return a.accountImport(nil) })
	if err == nil || !strings.Contains(err.Error(), "is already the home of codex/new; pick another name with codex-work=<name>") {
		t.Fatalf("import: %v", err)
	}
	if b, _ := os.ReadFile(login); !bytes.Equal(b, []byte(`{"who":"work"}`)) {
		t.Errorf("the home's login is now %q", b)
	}
	if b, _ := os.ReadFile(creds); !bytes.Equal(b, []byte(`{"who":"imported"}`)) {
		t.Errorf("aiquota's credentials are now %q", b)
	}
	if b, _ := os.ReadFile(a.cfg.Aiquota.Config); !bytes.Equal(b, cfg) {
		t.Errorf("aiquota's config is now %s", b)
	}
	if got := accountIDs(t, a); !slices.Equal(got, before) {
		t.Errorf("accounts %v, were %v", got, before)
	}
}

func TestHomeOwner(t *testing.T) {
	a := homeApp(t)
	acc := addTestAccount(t, a, "codex", "work")
	for _, c := range []struct {
		home, except, want string
	}{
		{acc.Home, "", acc.ID},
		{acc.Home + string(filepath.Separator), "", acc.ID},
		{acc.Home, acc.ID, ""},
		{homeFor("codex", "else"), "", ""},
	} {
		if got, err := a.homeOwner(c.home, c.except); err != nil || got != c.want {
			t.Errorf("homeOwner(%q, %q) = %q, %v; want %q", c.home, c.except, got, err, c.want)
		}
	}
	// A recorded home missing on disk is matched by its path alone.
	gone := state.Account{ID: "codex/gone", Provider: "codex", Name: "gone", Enabled: true, Home: homeFor("codex", "gone"), CreatedAt: 1}
	if err := a.st.AddAccount(gone); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.homeOwner(gone.Home+string(filepath.Separator)+".", ""); got != gone.ID {
		t.Errorf("a missing home: %q", got)
	}
	wantGone := ""
	if runtime.GOOS == "darwin" {
		wantGone = gone.ID
	}
	if got, _ := a.homeOwner(filepath.Join(filepath.Dir(gone.Home), "GONE"), ""); got != wantGone {
		t.Errorf("a missing home in upper case: %q, want %q", got, wantGone)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(acc.Home, alias); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.homeOwner(alias, ""); got != acc.ID {
		t.Errorf("a link to the home: %q", got)
	}
	upper := filepath.Join(filepath.Dir(acc.Home), "WORK")
	want := ""
	if runtime.GOOS == "darwin" {
		want = acc.ID
	}
	if got, _ := a.homeOwner(upper, ""); got != want {
		t.Errorf("the home in upper case: %q, want %q", got, want)
	}
}
