package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/paths"
)

// The login probe reads Claude Code's stored credential instead of running
// the CLI. A short-lived `claude auth status` starts an OAuth refresh at
// start-up and can exit before saving it, which spends the refresh token
// and logs the home out (anthropics/claude-code#95822); a probe that runs
// every poll would do exactly that. Nothing here refreshes or writes the
// credential: the email is read with the CLI's access token only while that
// token is still valid.

// keychainTimeout bounds the macOS keychain read, which can stall behind a
// locked keychain.
const keychainTimeout = 10 * time.Second

// securityItemNotFound is security(1)'s exit status for a missing item
// (errSecItemNotFound).
const securityItemNotFound = 44

// CLILogin is what the probe found in one home.
type CLILogin struct {
	LoggedIn bool
	// Email is the account the CLI's token belongs to; empty when it could
	// not be verified this time (an expired access token, or a failed
	// profile read), which says nothing either way.
	Email string
}

// cliCredential is the stored credential's shape.
type cliCredential struct {
	OAuth struct {
		AccessToken           string `json:"accessToken"`
		RefreshToken          string `json:"refreshToken"`
		ExpiresAt             int64  `json:"expiresAt"`             // unix milliseconds
		RefreshTokenExpiresAt int64  `json:"refreshTokenExpiresAt"` // unix milliseconds, 0 = unknown
	} `json:"claudeAiOauth"`
}

// ProbeLogin reports whether Claude Code is logged in in the account home,
// and as whom when that can be read without a refresh.
func ProbeLogin(home string, native bool, now time.Time) (CLILogin, error) {
	dir := home
	if native {
		dir = paths.RealClaudeHome()
	}
	raw, err := readCLICredential(dir)
	if err != nil {
		return CLILogin{}, err
	}
	c, ok := parseCLICredential(raw, now)
	if !ok {
		return CLILogin{}, fmt.Errorf("cannot parse the stored credential in %s", dir)
	}
	if !c.LoggedIn {
		return c, nil
	}
	if accessToken, live := liveAccessToken(raw, now); live {
		g := &Grant{}
		g.OAuth.AccessToken = accessToken
		if status, body, err := getJSON(oauthProfileURL, authHeaders(g)); err == nil && status == 200 {
			c.Email, _ = ParseProfile(body)
		}
	}
	return c, nil
}

// readCLICredential returns the credential the CLI would use for dir: the
// macOS keychain item first, then the file. No credential at all is not an
// error: it reads as an empty credential, which is logged out.
func readCLICredential(dir string) ([]byte, error) {
	if runtime.GOOS == "darwin" {
		ctx, cancel := context.WithTimeout(context.Background(), keychainTimeout)
		defer cancel()
		out, err := exec.CommandContext(ctx, "security", "find-generic-password", "-s", KeychainService(dir), "-w").Output()
		if ctx.Err() != nil {
			return nil, fmt.Errorf("keychain read timed out after %s", keychainTimeout)
		}
		// Only a missing item means the CLI would fall back to the file; a
		// locked keychain or a denied read says nothing about the login.
		var exit *exec.ExitError
		switch {
		case err == nil && strings.TrimSpace(string(out)) != "":
			return out, nil
		case errors.As(err, &exit) && exit.ExitCode() != securityItemNotFound:
			return nil, fmt.Errorf("keychain read failed (security exit %d)", exit.ExitCode())
		case err != nil && !errors.As(err, &exit):
			return nil, fmt.Errorf("keychain read: %w", err)
		}
	}
	data, err := os.ReadFile(credentialFile(dir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return data, err
}

// parseCLICredential reads the login state from a stored credential. ok is
// false when the credential cannot be parsed, which says nothing.
func parseCLICredential(raw []byte, now time.Time) (CLILogin, bool) {
	if strings.TrimSpace(string(raw)) == "" {
		return CLILogin{}, true
	}
	var c cliCredential
	if json.Unmarshal(raw, &c) != nil {
		return CLILogin{}, false
	}
	o := c.OAuth
	// The CLI empties the tokens when a refresh is refused, and keeps the
	// file; a refresh token past its own expiry cannot be used either.
	if o.RefreshToken == "" && (o.AccessToken == "" || o.ExpiresAt <= now.UnixMilli()) {
		return CLILogin{}, true
	}
	if o.RefreshTokenExpiresAt > 0 && o.RefreshTokenExpiresAt <= now.UnixMilli() {
		return CLILogin{}, true
	}
	return CLILogin{LoggedIn: true}, true
}

// liveAccessToken returns the access token when it has at least a minute
// left, so a read with it neither fails mid-way nor needs a refresh.
func liveAccessToken(raw []byte, now time.Time) (string, bool) {
	var c cliCredential
	if json.Unmarshal(raw, &c) != nil || c.OAuth.AccessToken == "" {
		return "", false
	}
	return c.OAuth.AccessToken, c.OAuth.ExpiresAt > now.Add(time.Minute).UnixMilli()
}
