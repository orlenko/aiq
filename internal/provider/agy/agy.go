// Package agy implements the Antigravity CLI provider. The CLI keeps its
// login (oauth_creds.json, google_accounts.json) in ~/.gemini and its own
// state (settings, conversations, transcripts, hooks) in
// ~/.gemini/antigravity-cli; the only way to point it elsewhere is HOME. An
// account is either the user's real ~/.gemini (native) or, later, a home of
// its own that HOME is set to for the launch.
package agy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/term"

	"github.com/orlenko/aiq/internal/fsutil"
	"github.com/orlenko/aiq/internal/proc"
)

// authEnvVars can switch the CLI from its stored login to an API key or a
// service account, so managed launches must not inherit them.
var authEnvVars = []string{"GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS"}

// Provider launches the Antigravity CLI. Command builds a child process for
// the real CLI (the caller decides how the binary is found).
type Provider struct {
	Command func(args []string, env []string) *exec.Cmd
}

// GeminiDir is the ~/.gemini of an account home: the home itself for a
// native account, <home>/.gemini for a home that stands in for HOME.
func GeminiDir(home string, native bool) string {
	if native {
		return home
	}
	return filepath.Join(home, ".gemini")
}

// AppDataDir is where the CLI keeps its state under a gemini dir.
func AppDataDir(geminiDir string) string { return filepath.Join(geminiDir, "antigravity-cli") }

// ConfigDir holds the global hooks.json under a gemini dir.
func ConfigDir(geminiDir string) string { return filepath.Join(geminiDir, "config") }

// SettingsPath is the CLI's settings.json under a gemini dir.
func SettingsPath(geminiDir string) string {
	return filepath.Join(AppDataDir(geminiDir), "settings.json")
}

// Env builds the child environment for a launch on the given home.
func (p *Provider) Env(home string, native bool, inheritAuthEnv bool) []string {
	env := os.Environ()
	if !inheritAuthEnv {
		env = proc.SanitizeEnv(env, authEnvVars...)
	}
	if !native {
		env = append(proc.SanitizeEnv(env, "HOME"), "HOME="+home)
	}
	return env
}

// Login runs the CLI interactively: with no login stored it opens the
// browser sign-in and, once signed in, drops into the prompt.
func (p *Provider) Login(home string, native bool) error {
	fmt.Fprintln(os.Stderr, "Running `agy` — sign in as the account you are enrolling, then leave the session (/exit or ctrl-c).")
	code, err := proc.RunInteractive(p.Command(nil, p.Env(home, native, false)))
	if err != nil {
		return fmt.Errorf("agy: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("agy exited with code %d", code)
	}
	return nil
}

// HasCredential reports whether the home is logged in. The CLI keeps its
// token in the OS keyring (the files in ~/.gemini belong to the Gemini CLI),
// so the directory carries no evidence: a native home always counts, and an
// overlay home counts once a login marker exists. The poll is the real
// check: a logged-out account fails it with a login error.
func HasCredential(home string, native bool) bool {
	if native {
		return true
	}
	_, err := os.Stat(filepath.Join(home, ".aiq-login"))
	return err == nil
}

// MarkLoggedIn records that an overlay home authenticated.
func MarkLoggedIn(home, email string) error {
	return os.WriteFile(filepath.Join(home, ".aiq-login"), []byte(email+"\n"), 0o600)
}

// passthrough lists subcommands that manage the CLI itself and must run on
// the user's real home, unrouted.
var passthrough = map[string]bool{
	"install": true, "update": true, "changelog": true, "help": true,
	"mcp": true, "plugin": true, "plugins": true, "models": true,
	"agent": true, "agents": true, "remote-control": true, "mic-serve": true,
	"--version": true, "--help": true, "-h": true,
}

// Passthrough reports whether args should bypass routing.
func Passthrough(args []string) bool {
	if len(args) == 0 {
		return false
	}
	return passthrough[args[0]]
}

// IsWorker classifies an invocation: -p/--print/--prompt, or a non-terminal
// stdout, means a disposable worker; everything else is an interactive
// session.
func IsWorker(args []string) bool {
	for _, a := range args {
		if a == "-p" || a == "--print" || a == "--prompt" || strings.HasPrefix(a, "--print=") || strings.HasPrefix(a, "--prompt=") {
			return true
		}
	}
	return !term.IsTerminal(int(os.Stdout.Fd()))
}

// Session returns the conversation a launch resumes (--conversation <id>),
// or "" when the launch starts a new one or continues the latest. The CLI
// has no flag to choose a new conversation's id, so a new one is matched to
// its launch later by directory and time.
func Session(args []string) string {
	for i, a := range args {
		switch {
		case a == "--conversation" && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-"):
			return args[i+1]
		case strings.HasPrefix(a, "--conversation="):
			return strings.TrimPrefix(a, "--conversation=")
		}
	}
	return ""
}

// Bypass is the CLI's permission-bypass flag.
const Bypass = "--dangerously-skip-permissions"

var modelRe = regexp.MustCompile(`"model"\s*:\s*"([^"]+)"`)

// DefaultModel reads the "model" setting (a display name such as "Gemini
// 3.8 Flash (Medium)") from the CLI's settings.json under geminiDir.
func DefaultModel(geminiDir string) string {
	data, err := os.ReadFile(SettingsPath(geminiDir))
	if err != nil {
		return ""
	}
	if m := modelRe.FindSubmatch(data); m != nil {
		return string(m[1])
	}
	return ""
}

// effortSuffixes are the reasoning levels the CLI bakes into its Gemini
// model names (gemini-3.8-flash-high); --effort conflicts with a model that
// already carries one.
var effortSuffixes = []string{"-low", "-medium", "-high"}

// BaseModel strips the effort suffix from a model name.
func BaseModel(model string) string {
	for _, s := range effortSuffixes {
		if strings.HasSuffix(model, s) {
			return strings.TrimSuffix(model, s)
		}
	}
	return model
}

// SameModel reports whether two names are the same model at any effort.
func SameModel(a, b string) bool { return BaseModel(a) == BaseModel(b) }

// ModelArgs spells a model and an effort level the way the CLI accepts
// them: a Gemini model with the level as its suffix (the CLI lists which
// levels each model has; an absent one rounds to the nearest), a model
// without variants as it is, and --effort alone when no model is named.
func ModelArgs(model, level string) []string {
	switch {
	case model == "" && level == "":
		return nil
	case model == "":
		return []string{"--effort", level}
	case level == "" || BaseModel(model) == model && !strings.HasPrefix(model, "gemini-"):
		return []string{"--model", model}
	}
	base := BaseModel(model)
	if strings.Contains(base, "-pro") && level == "medium" {
		level = "high" // the pro models come in low and high only
	}
	return []string{"--model", base + "-" + level}
}

// ScopeOf names the quota bucket a model draws on: Gemini models share the
// account's main windows (no scope); Claude and other third-party models
// have windows of their own, scoped "3p".
func ScopeOf(model string) string {
	m := strings.ToLower(model)
	if m == "" || strings.Contains(m, "gemini") {
		return ""
	}
	return ThirdPartyScope
}

// ThirdPartyScope is the scope of the windows for non-Gemini models.
const ThirdPartyScope = "3p"

// TrustWorkspace adds a workspace to the CLI's trusted list in settings.json,
// so a supervised session (nobody at the keyboard) is not stopped at the
// trust prompt. It reports whether the file changed.
func TrustWorkspace(geminiDir, workspace string) (changed bool, err error) {
	path := SettingsPath(geminiDir)
	settings, err := readSettings(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	var trusted []any
	if list, ok := settings["trustedWorkspaces"].([]any); ok {
		trusted = list
	}
	for _, t := range trusted {
		if s, ok := t.(string); ok && s == workspace {
			return false, nil
		}
	}
	settings["trustedWorkspaces"] = append(trusted, workspace)
	if err := os.MkdirAll(AppDataDir(geminiDir), 0o755); err != nil {
		return false, err
	}
	return true, writeSettings(path, settings)
}

// --- hooks ---

// hooksKey is the entry aiq owns in the global hooks.json.
const hooksKey = "aiq-long"

// HooksPath is the global hooks file under a gemini dir.
func HooksPath(geminiDir string) string { return filepath.Join(ConfigDir(geminiDir), "hooks.json") }

// hookHandlers renders the handlers for one event.
func hookHandlers(aiqBin, event string) []any {
	return []any{map[string]any{"type": "command", "command": aiqBin + " agy-hook " + event, "timeout": 30}}
}

// InstallHooks makes sure the global hooks.json carries aiq's long-session
// hooks. The CLI has no per-launch hook flag, so they are global and do
// nothing unless the session carries an aiq long lease. Other entries in the
// file are left as they are. It reports whether the file changed.
func InstallHooks(geminiDir, aiqBin string) (changed bool, err error) {
	path := HooksPath(geminiDir)
	doc := map[string]json.RawMessage{}
	data, err := os.ReadFile(path)
	if err == nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, &doc); err != nil {
			return false, fmt.Errorf("parse %s: %w", path, err)
		}
	} else if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	want, _ := json.Marshal(map[string]any{
		"PreInvocation": hookHandlers(aiqBin, "userpromptsubmit"),
		"Stop":          hookHandlers(aiqBin, "stop"),
	})
	if have, ok := doc[hooksKey]; ok {
		var compact bytes.Buffer
		if json.Compact(&compact, have) == nil && bytes.Equal(compact.Bytes(), want) {
			return false, nil
		}
	}
	doc[hooksKey] = want
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	return true, fsutil.WriteFileAtomic(path, append(out, '\n'))
}

// UninstallHooks removes aiq's entry from the global hooks.json.
func UninstallHooks(geminiDir string) error {
	path := HooksPath(geminiDir)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	doc := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if _, ok := doc[hooksKey]; !ok {
		return nil
	}
	delete(doc, hooksKey)
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, append(out, '\n'))
}
