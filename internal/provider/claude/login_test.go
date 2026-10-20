package claude

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestParseCLICredential(t *testing.T) {
	now := time.Now()
	past, future := now.Add(-time.Hour).UnixMilli(), now.Add(time.Hour).UnixMilli()
	cred := func(access, refresh string, expires, refreshExpires int64) string {
		return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":%q,"expiresAt":%d,"refreshTokenExpiresAt":%d}}`, access, refresh, expires, refreshExpires)
	}
	cases := []struct {
		name     string
		raw      string
		loggedIn bool
		ok       bool
	}{
		{"no credential", "", false, true},
		{"live", cred("at", "rt", future, 0), true, true},
		// An idle login: the access token lapsed, the CLI refreshes it on use.
		{"expired access token", cred("at", "rt", past, 0), true, true},
		// What the CLI leaves after a refused refresh.
		{"tokens wiped", cred("", "", 0, 0), false, true},
		{"refresh token expired", cred("at", "rt", past, past), false, true},
		{"access token only, live", cred("at", "", future, 0), true, true},
		{"garbage", "not json", false, false},
	}
	for _, c := range cases {
		got, ok := parseCLICredential([]byte(c.raw), now)
		if ok != c.ok || got.LoggedIn != c.loggedIn {
			t.Errorf("%s: got loggedIn=%v ok=%v, want %v %v", c.name, got.LoggedIn, ok, c.loggedIn, c.ok)
		}
	}
}

func TestLiveAccessToken(t *testing.T) {
	now := time.Now()
	raw := func(expires time.Time) []byte {
		return []byte(fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"at","refreshToken":"rt","expiresAt":%d}}`, expires.UnixMilli()))
	}
	if tok, live := liveAccessToken(raw(now.Add(time.Hour)), now); !live || tok != "at" {
		t.Errorf("an hour left: got %q %v", tok, live)
	}
	// Too close to expiry to use without a refresh, which is the CLI's to do.
	if _, live := liveAccessToken(raw(now.Add(30*time.Second)), now); live {
		t.Error("30s left read as live")
	}
}

func TestProbeLoginReadsFileWithoutNetworkWhenTokenLapsed(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("the keychain answers first on macOS")
	}
	now := time.Now()
	home := t.TempDir()
	write := func(s string) {
		if err := os.WriteFile(filepath.Join(home, ".credentials.json"), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"at","refreshToken":"rt","expiresAt":%d}}`, now.Add(-time.Hour).UnixMilli()))
	if got, err := ProbeLogin(home, false, now); err != nil || !got.LoggedIn || got.Email != "" {
		t.Fatalf("lapsed login: got %+v, %v", got, err)
	}
	write(`{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0}}`)
	if got, err := ProbeLogin(home, false, now); err != nil || got.LoggedIn {
		t.Fatalf("wiped login: got %+v, %v", got, err)
	}
	write("{")
	if _, err := ProbeLogin(home, false, now); err == nil {
		t.Fatal("an unreadable credential should be an error, not a verdict")
	}
}
