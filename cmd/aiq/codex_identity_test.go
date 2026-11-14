package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/state"
)

// identityApp is an app on a temporary data directory with one Codex
// account recorded as identity, its home a temporary directory.
func identityApp(t *testing.T, identity string) (*app, state.Account) {
	t.Helper()
	a := renameApp(t)
	a.pool = &pool.Pool{Cfg: a.cfg, St: a.st}
	acc := state.Account{ID: "codex/work", Provider: "codex", Name: "work", Enabled: true, Home: t.TempDir(), Identity: identity, Priority: 100}
	if err := a.st.AddAccount(acc); err != nil {
		t.Fatal(err)
	}
	return a, acc
}

// writeAuth writes an auth.json whose unsigned id token names email, or
// none when email is "".
func writeAuth(t *testing.T, home, email string) {
	t.Helper()
	claims := map[string]any{"sub": "invented-subject"}
	if email != "" {
		claims["email"] = email
	}
	raw, _ := json.Marshal(claims)
	token := "hdr." + base64.RawURLEncoding.EncodeToString(raw) + ".sig"
	doc := fmt.Sprintf(`{"tokens":{"id_token":%q,"access_token":"invented-access","refresh_token":"invented-refresh"}}`, token)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
}

func stderrOf(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = old }()
	f()
	w.Close()
	out, _ := io.ReadAll(r)
	return string(out)
}

// The identity step of `aiq account login codex/<name>`. The CLI login
// itself re-runs the aiq binary as a trampoline, so it has no seam for a
// fake codex; this covers what the login does with the auth.json it left.
func TestCodexLoginKeepsTheRecordedIdentity(t *testing.T) {
	cases := []struct {
		name, stored, loggedIn string
		wantIdentity, wantWho  string
		wantWarn               string
	}{
		{"first login", "", "a@example.com", "a@example.com", "a@example.com", ""},
		{"same account", "a@example.com", "a@example.com", "a@example.com", "a@example.com", ""},
		{"case only", "A@example.com", "a@example.com", "a@example.com", "a@example.com", ""},
		{"someone else", "a@example.com", "b@example.com", "a@example.com", "b@example.com", "aiq account authorize codex/work"},
		{"no email in the token", "a@example.com", "", "a@example.com", "", "could not read which account signed in"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, acc := identityApp(t, c.stored)
			writeAuth(t, acc.Home, c.loggedIn)
			var got state.Account
			var who string
			warn := stderrOf(t, func() { got, who = a.checkCodexLogin(acc) })
			if got.Identity != c.wantIdentity || who != c.wantWho {
				t.Errorf("identity %q who %q; want %q, %q", got.Identity, who, c.wantIdentity, c.wantWho)
			}
			if (c.wantWarn == "") != (warn == "") || !strings.Contains(warn, c.wantWarn) {
				t.Errorf("stderr %q, want it to contain %q", warn, c.wantWarn)
			}
			if strings.Contains(warn, "invented-") {
				t.Errorf("stderr shows token material: %q", warn)
			}
			u, _, _ := a.st.GetUsage(acc.ID)
			if mismatch := pool.CLIMismatch(got.Identity, u); mismatch != (c.name == "someone else") {
				t.Errorf("recorded mismatch %v (CLI %q)", mismatch, u.CLIIdentity)
			}
		})
	}
}

func TestAuthorizeCodexAcceptsTheHomeLogin(t *testing.T) {
	a, acc := identityApp(t, "a@example.com")
	writeAuth(t, acc.Home, "b@example.com")
	a.st.SetCLILogin(acc.ID, true, "b@example.com", time.Now())
	out, err := stdoutOf(t, func() error { return a.accountAuthorize(acc.ID) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "was a@example.com, now b@example.com") || strings.Contains(out, "invented-") {
		t.Errorf("output %q", out)
	}
	stored, _ := a.st.GetAccount(acc.ID)
	u, _, _ := a.st.GetUsage(acc.ID)
	if stored.Identity != "b@example.com" || pool.CLILoginProblem(stored, u) != "" {
		t.Errorf("identity %q, problem %q", stored.Identity, pool.CLILoginProblem(stored, u))
	}
	evs, _ := a.st.ListEvents(10)
	if len(evs) == 0 || evs[0].Type != "login" || !strings.Contains(evs[0].Detail, "now b@example.com") {
		t.Errorf("events %+v", evs)
	}
}

func TestAuthorizeCodexRefusesAnUnreadableLogin(t *testing.T) {
	cases := []struct {
		name  string
		write func(home string)
	}{
		{"no auth.json", func(string) {}},
		{"no email claim", func(home string) { writeAuth(t, home, "") }},
		{"not JSON", func(home string) { os.WriteFile(filepath.Join(home, "auth.json"), []byte("{"), 0o600) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, acc := identityApp(t, "a@example.com")
			c.write(acc.Home)
			if _, err := stdoutOf(t, func() error { return a.accountAuthorize(acc.ID) }); err == nil || !strings.Contains(err.Error(), "aiq account login") {
				t.Fatalf("err %v, want a refusal naming the login command", err)
			}
			if stored, _ := a.st.GetAccount(acc.ID); stored.Identity != "a@example.com" {
				t.Errorf("identity %q after a refused authorize", stored.Identity)
			}
		})
	}
}

func TestDoctorReportsACodexMismatch(t *testing.T) {
	for _, c := range []struct {
		loggedIn string
		wantFail bool
	}{{"b@example.com", true}, {"a@example.com", false}} {
		t.Run(c.loggedIn, func(t *testing.T) {
			a, acc := identityApp(t, "a@example.com")
			writeAuth(t, acc.Home, c.loggedIn)
			r := &doctorReport{byKey: map[string]*doctorFix{}}
			a.checkAccount(r, acc, time.Now(), nil)
			found := false
			for _, ch := range r.checks {
				if strings.Contains(ch.text, "CLI logged in as") {
					found = true
					if ch.ok == c.wantFail || (c.wantFail && !strings.Contains(ch.text, "aiq account authorize codex/work")) {
						t.Errorf("check ok=%v %q", ch.ok, ch.text)
					}
				}
			}
			if !found {
				t.Errorf("no CLI login check in %+v", r.checks)
			}
		})
	}
}
