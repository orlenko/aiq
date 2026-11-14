package pool

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/selector"
	"github.com/orlenko/aiq/internal/state"
)

// writeCodexAuth writes an auth.json whose unsigned id token names email,
// or one with no email claim when email is "".
func writeCodexAuth(t *testing.T, home, email string) {
	t.Helper()
	claims := map[string]any{"sub": "invented-subject"}
	if email != "" {
		claims["email"] = email
	}
	raw, _ := json.Marshal(claims)
	token := "hdr." + base64.RawURLEncoding.EncodeToString(raw) + ".sig"
	doc := fmt.Sprintf(`{"tokens":{"id_token":%q,"access_token":"invented","refresh_token":"invented"}}`, token)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
}

func codexTestStore(t *testing.T) *state.Store {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func addCodexAccount(t *testing.T, st *state.Store, identity string) state.Account {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	acc := state.Account{ID: "codex/work", Provider: "codex", Name: "work", Enabled: true, Home: home, Identity: identity, Priority: 100}
	if err := st.AddAccount(acc); err != nil {
		t.Fatal(err)
	}
	return acc
}

// held reports why routing skips the account, or "" when it does not.
func held(t *testing.T, p *Pool) string {
	t.Helper()
	cands, err := p.Candidates("codex")
	if err != nil || len(cands) != 1 {
		t.Fatalf("candidates %+v, %v", cands, err)
	}
	if cands[0].Unavailable != "" && cands[0].Unavailable != selector.UnavailableLoginMismatch {
		t.Fatalf("unexpected hold %q: %s", cands[0].Unavailable, cands[0].UnavailableReason)
	}
	return cands[0].UnavailableReason
}

func loginEvents(t *testing.T, st *state.Store) []state.Event {
	t.Helper()
	evs, err := st.ListEvents(100)
	if err != nil {
		t.Fatal(err)
	}
	var out []state.Event
	for _, e := range evs {
		if e.Type == "login" {
			out = append(out, e)
		}
	}
	return out
}

func TestReconcileIdentity(t *testing.T) {
	cases := []struct {
		name, stored, observed, want string
		mismatch                     bool
	}{
		{"first login", "", "a@example.com", "a@example.com", false},
		{"same address", "a@example.com", "a@example.com", "a@example.com", false},
		{"case only", "A@Example.com", "a@example.com", "a@example.com", false},
		{"someone else", "a@example.com", "b@example.com", "a@example.com", true},
		{"unreadable login keeps the identity", "a@example.com", "", "a@example.com", false},
		{"nothing either side", "", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, mismatch := ReconcileIdentity(c.stored, c.observed)
			if got != c.want || mismatch != c.mismatch {
				t.Errorf("ReconcileIdentity(%q, %q) = %q, %v; want %q, %v", c.stored, c.observed, got, mismatch, c.want, c.mismatch)
			}
		})
	}
}

// Every state an account can be in when a Codex login or poll reports an
// email, including rows from before this check existed.
func TestRecordCodexLogin(t *testing.T) {
	cases := []struct {
		name         string
		stored       string
		usageRow     bool   // a usage row exists (polled before)
		priorCLI     string // an earlier recorded CLI email, "" for none
		observed     string
		wantIdentity string
		wantMismatch bool
		wantHeld     bool
		wantEvents   int
	}{
		{name: "upgrade: empty identity, no usage row", observed: "a@example.com", wantIdentity: "a@example.com"},
		{name: "upgrade: empty identity, polled before", usageRow: true, observed: "a@example.com", wantIdentity: "a@example.com"},
		{name: "upgrade: matching identity, no usage row", stored: "a@example.com", observed: "a@example.com", wantIdentity: "a@example.com"},
		{name: "upgrade: matching identity, polled before", stored: "a@example.com", usageRow: true, observed: "a@example.com", wantIdentity: "a@example.com"},
		{name: "case only", stored: "A@Example.com", observed: "a@example.com", wantIdentity: "a@example.com"},
		{name: "someone else is held", stored: "a@example.com", usageRow: true, observed: "b@example.com", wantIdentity: "a@example.com", wantMismatch: true, wantHeld: true, wantEvents: 1},
		{name: "someone else, no usage row", stored: "a@example.com", observed: "b@example.com", wantIdentity: "a@example.com", wantMismatch: true, wantHeld: true, wantEvents: 1},
		{name: "unreadable login changes nothing", stored: "a@example.com", observed: "", wantIdentity: "a@example.com"},
		{name: "unreadable login keeps an earlier hold", stored: "a@example.com", priorCLI: "b@example.com", observed: "", wantIdentity: "a@example.com", wantHeld: true},
		{name: "the right login clears a hold", stored: "a@example.com", priorCLI: "b@example.com", observed: "a@example.com", wantIdentity: "a@example.com"},
		{name: "the same wrong login logs nothing new", stored: "a@example.com", priorCLI: "b@example.com", observed: "b@example.com", wantIdentity: "a@example.com", wantMismatch: true, wantHeld: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := codexTestStore(t)
			p := &Pool{Cfg: config.Default(), St: st}
			acc := addCodexAccount(t, st, c.stored)
			now := time.Now()
			if c.usageRow {
				st.SetUsageMeta(acc.ID, "plus", 0, "", now)
			}
			if c.priorCLI != "" {
				st.SetCLILogin(acc.ID, true, c.priorCLI, now)
			}
			got, mismatch := p.RecordCodexLogin(acc, c.observed, now)
			stored, err := st.GetAccount(acc.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Identity != c.wantIdentity || stored.Identity != c.wantIdentity || mismatch != c.wantMismatch {
				t.Errorf("identity returned %q stored %q mismatch %v; want %q, %v", got.Identity, stored.Identity, mismatch, c.wantIdentity, c.wantMismatch)
			}
			why := held(t, p)
			if (why != "") != c.wantHeld {
				t.Errorf("held %q, want held %v", why, c.wantHeld)
			}
			if c.wantHeld && (!strings.Contains(why, "a@example.com") || !strings.Contains(why, "b@example.com")) {
				t.Errorf("hold reason %q should name both emails", why)
			}
			if n := len(loginEvents(t, st)); n != c.wantEvents {
				t.Errorf("%d login events, want %d", n, c.wantEvents)
			}
		})
	}
}

// Regression for #36: a poll of a Codex home logged in as someone else
// replaced the account's identity, so routing spent the other person's
// quota. The poll runs end to end against a fake app-server.
func TestCodexPollKeepsIdentityAndHoldsOnMismatch(t *testing.T) {
	cases := []struct {
		name, loggedIn string
		wantHeld       bool
	}{
		{"someone else", "personal@example.com", true},
		{"the account itself", "work@example.com", false},
		{"no email in the token", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := codexTestStore(t)
			acc := addCodexAccount(t, st, "work@example.com")
			writeCodexAuth(t, acc.Home, c.loggedIn)
			p := &Pool{Cfg: config.Default(), St: st, Command: fakeCodexCommand}
			for range 2 {
				if err := p.pollOne(acc, 10*time.Second); err != nil {
					t.Fatalf("poll: %v", err)
				}
			}
			stored, err := st.GetAccount(acc.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Identity != "work@example.com" {
				t.Errorf("identity %q after the poll, want work@example.com", stored.Identity)
			}
			if why := held(t, p); (why != "") != c.wantHeld {
				t.Errorf("held %q, want held %v", why, c.wantHeld)
			}
			want := 0
			if c.wantHeld {
				want = 1
			}
			if n := len(loginEvents(t, st)); n != want {
				t.Errorf("%d login events after two polls, want %d", n, want)
			}
		})
	}
}

func fakeCodexCommand(provider string, args, env []string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestFakeCodexAppServer$")
	cmd.Env = append(env, "AIQ_TEST_FAKE_CODEX=1")
	return cmd
}

// TestFakeCodexAppServer is the fake `codex app-server`: it answers
// initialize and account/rateLimits/read, and refuses anything else.
func TestFakeCodexAppServer(t *testing.T) {
	if os.Getenv("AIQ_TEST_FAKE_CODEX") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(scanner.Bytes(), &req) != nil || len(req.ID) == 0 {
			continue
		}
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		switch req.Method {
		case "initialize":
			resp["result"] = map[string]any{}
		case "account/rateLimits/read":
			resp["result"] = map[string]any{"rateLimits": map[string]any{
				"primary": map[string]any{"usedPercent": 10, "windowDurationMins": 300, "resetsAt": time.Now().Add(time.Hour).Unix()},
			}}
		default:
			resp["error"] = map[string]any{"code": -32601, "message": "not in the fake"}
		}
		out, _ := json.Marshal(resp)
		fmt.Println(string(out))
	}
	os.Exit(0)
}
