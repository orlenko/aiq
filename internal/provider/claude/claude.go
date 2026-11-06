// Package claude implements the Claude Code provider. Each account is a
// CLAUDE_CONFIG_DIR: either the user's real ~/.claude (a "native" account)
// or an overlay home that symlinks everything in ~/.claude and carries only
// its own credential (Keychain entry on macOS, .credentials.json on Linux).
package claude

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/term"

	"github.com/orlenko/aiq/internal/fsutil"
	"github.com/orlenko/aiq/internal/overlay"
	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/proc"
)

// authEnvVars can override subscription auth per Anthropic's documented
// precedence (cloud-provider flags and ANTHROPIC_* rank above the stored
// login), so managed launches must not inherit them.
var authEnvVars = []string{
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN",
	"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
}

// Provider launches Claude Code. Command builds a child process for the
// real CLI (the caller decides how the binary is found).
type Provider struct {
	Command func(args []string, env []string) *exec.Cmd
}

// OverlaySpec describes an account home layered over the real ~/.claude.
func OverlaySpec(home string) overlay.Spec {
	// .claude.json is private per account: Claude Code rewrites it with a
	// tmp+rename on every change, which would detach a shared symlink at the
	// first write and lose one side's changes at the next sync. It is seeded
	// from the real ~/.claude.json when the account is created (see Seed).
	return overlay.Spec{
		Real:         paths.RealClaudeHome(),
		Overlay:      home,
		Exclude:      []string{".credentials.json", ".credentials.json.bak", ".claude.json", ".claude.json.backup"},
		SkipPrefixes: []string{".DS_Store", ".aiq-"},
		LockDir:      paths.LocksDir(),
	}
}

// baseEnv strips auth overrides and any inherited config-dir pointer.
func baseEnv(inheritAuthEnv bool) []string {
	env := os.Environ()
	if !inheritAuthEnv {
		env = proc.SanitizeEnv(env, authEnvVars...)
	}
	// An inherited CLAUDE_CONFIG_DIR from a routed parent must not leak
	// into a differently routed child; the real one is re-derived below.
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" && d != paths.RealClaudeHome() {
		env = proc.SanitizeEnv(env, "CLAUDE_CONFIG_DIR")
	}
	return env
}

// Env builds the child environment for a launch on the given home.
func (p *Provider) Env(home string, native bool, inheritAuthEnv bool) []string {
	env := baseEnv(inheritAuthEnv)
	if !native {
		env = append(env, "CLAUDE_CONFIG_DIR="+home)
	} else if real := paths.RealClaudeHome(); real != filepath.Join(userHome(), ".claude") {
		env = append(env, "CLAUDE_CONFIG_DIR="+real)
	}
	return env
}

func userHome() string {
	h, _ := os.UserHomeDir()
	return h
}

// Login runs `claude auth login` interactively inside the account home.
func (p *Provider) Login(home string, native bool) error {
	fmt.Fprintln(os.Stderr, "Running `claude auth login` — authenticate in the browser as the account you are enrolling.")
	code, err := proc.RunInteractive(p.Command([]string{"auth", "login"}, p.Env(home, native, false)))
	if err != nil {
		return fmt.Errorf("claude auth login: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("claude auth login exited with code %d", code)
	}
	return nil
}

// AuthStatus reports the email logged in for a home ("" when logged out).
func (p *Provider) AuthStatus(home string, native bool) (email string, loggedIn bool, err error) {
	cmd := p.Command([]string{"auth", "status"}, p.Env(home, native, false))
	// A logged-out home exits 1 and still prints the JSON, so the answer is
	// read from the output whatever the exit status.
	out, runErr := cmd.Output()
	return parseAuthStatus(out, runErr)
}

func parseAuthStatus(out []byte, runErr error) (email string, loggedIn bool, err error) {
	var doc struct {
		LoggedIn *bool  `json:"loggedIn"`
		Email    string `json:"email"`
	}
	start := strings.Index(string(out), "{")
	if start < 0 || json.Unmarshal(out[start:], &doc) != nil || doc.LoggedIn == nil {
		if runErr != nil {
			return "", false, fmt.Errorf("claude auth status: %w", runErr)
		}
		return "", false, fmt.Errorf("claude auth status: no login state in output")
	}
	return doc.Email, *doc.LoggedIn, nil
}

// HasCredential reports whether the home is logged in. Cheap heuristic:
// native homes always count; overlays count once a login marker exists.
func HasCredential(home string, native bool) bool {
	if native {
		return true
	}
	if _, err := os.Stat(filepath.Join(home, ".aiq-login")); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(home, ".credentials.json")); err == nil {
		return true
	}
	return false
}

// MarkLoggedIn records that an overlay home authenticated (macOS keeps the
// credential in the Keychain, so the directory itself carries no evidence).
func MarkLoggedIn(home, email string) error {
	return os.WriteFile(filepath.Join(home, ".aiq-login"), []byte(email+"\n"), 0o600)
}

// passthrough lists subcommands that manage the CLI itself and must run on
// the user's real home, unrouted.
var passthrough = map[string]bool{
	"auth": true, "setup-token": true, "update": true, "install": true,
	"doctor": true, "version": true, "--version": true, "-v": true,
	"--help": true, "-h": true,
}

// SeedPrivate copies per-account state that starts from the user's copy.
func SeedPrivate(home string) error {
	return overlay.Seed(paths.ClaudeGlobalJSON(), filepath.Join(home, ".claude.json"))
}

// Passthrough reports whether args should bypass routing.
func Passthrough(args []string) bool {
	if len(args) == 0 {
		return false
	}
	return passthrough[args[0]]
}

// IsWorker classifies an invocation: -p/--print, or a non-terminal stdout,
// means a disposable worker; everything else is an interactive session.
func IsWorker(args []string) bool {
	for _, a := range args {
		if a == "-p" || a == "--print" || strings.HasPrefix(a, "--print=") {
			return true
		}
	}
	return !term.IsTerminal(int(os.Stdout.Fd()))
}

var modelRe = regexp.MustCompile(`"model"\s*:\s*"([^"]+)"`)

// DefaultModel reads the "model" setting from the real settings.json.
func DefaultModel() string {
	data, err := os.ReadFile(filepath.Join(paths.RealClaudeHome(), "settings.json"))
	if err != nil {
		return ""
	}
	if m := modelRe.FindSubmatch(data); m != nil {
		return string(m[1])
	}
	return ""
}

// CopyProjectTrust copies the per-project entry of .claude.json (which holds
// the trust-dialog acceptance and per-project settings) from one account to
// another, so a session moved between accounts is not stopped by the trust
// prompt. Missing entries are not an error.
func CopyProjectTrust(srcHome string, srcNative bool, dstHome string, dstNative bool, workspace string) error {
	src := filepath.Join(srcHome, ".claude.json")
	if srcNative {
		src = paths.ClaudeGlobalJSON()
	}
	dst := filepath.Join(dstHome, ".claude.json")
	if dstNative {
		dst = paths.ClaudeGlobalJSON()
	}
	if src == dst {
		return nil
	}
	srcData, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	var srcDoc map[string]json.RawMessage
	if err := json.Unmarshal(srcData, &srcDoc); err != nil {
		return err
	}
	var srcProjects map[string]json.RawMessage
	if json.Unmarshal(srcDoc["projects"], &srcProjects) != nil || srcProjects[workspace] == nil {
		return nil
	}
	dstDoc := map[string]json.RawMessage{}
	if data, err := os.ReadFile(dst); err == nil {
		if err := json.Unmarshal(data, &dstDoc); err != nil {
			return err
		}
	}
	dstProjects := map[string]json.RawMessage{}
	if raw, ok := dstDoc["projects"]; ok {
		json.Unmarshal(raw, &dstProjects)
	}
	if _, exists := dstProjects[workspace]; exists {
		return nil // keep the destination's own history and settings
	}
	dstProjects[workspace] = srcProjects[workspace]
	enc, err := json.Marshal(dstProjects)
	if err != nil {
		return err
	}
	dstDoc["projects"] = enc
	out, err := json.MarshalIndent(dstDoc, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(dst, out)
}

// HookSettings renders the --settings JSON that wires aiq's hooks into a
// long session. Hooks are only attached to sessions launched this way.
func HookSettings(aiqBin string) string {
	return LaunchSettings(aiqBin, "")
}

// LaunchSettings renders one --settings JSON (Claude Code honours only the
// last --settings flag) carrying aiq's hooks when aiqBin is set and the
// auto-memory directory when memoryDir is set. Empty when neither is.
func LaunchSettings(aiqBin, memoryDir string) string {
	var parts []string
	if memoryDir != "" {
		enc, _ := json.Marshal(memoryDir)
		parts = append(parts, `"autoMemoryDirectory":`+string(enc))
	}
	if aiqBin != "" {
		hook := func(event string) string {
			return fmt.Sprintf(`"%s":[{"hooks":[{"type":"command","command":%q,"timeout":30}]}]`, event, aiqBin+" claude-hook "+strings.ToLower(event))
		}
		parts = append(parts, `"hooks":{`+hook("SessionStart")+`,`+hook("UserPromptSubmit")+`,`+hook("Stop")+`}`)
	}
	if len(parts) == 0 {
		return ""
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// OverlayMemoryDir is the auto-memory directory an overlay launch in dir
// should use: the project's folder under the real ~/.claude/projects.
//
// Left alone, Claude Code puts memory under $CLAUDE_CONFIG_DIR/projects,
// which in an overlay is a symlink into ~/.claude. Every memory write then
// resolves into a .claude directory, and Claude Code's sensitive-path check
// asks for it before any allow rule is consulted, so no permission grant
// stops the prompt. Naming the real directory keeps the write on one
// spelling, which Claude Code's own memory carve-out allows.
//
// Empty (keep Claude Code's default) when the user's settings already pick
// a memory directory or the folder name would need Claude Code's hash
// suffix.
func OverlayMemoryDir(dir string) string {
	if userSetsMemoryDir() {
		return ""
	}
	slug := projectSlug(projectRoot(dir))
	if slug == "" {
		return ""
	}
	return filepath.Join(paths.RealClaudeHome(), "projects", slug, "memory")
}

func userSetsMemoryDir() bool {
	data, err := os.ReadFile(filepath.Join(paths.RealClaudeHome(), "settings.json"))
	if err != nil {
		return false
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(data, &doc) != nil {
		return false
	}
	_, ok := doc["autoMemoryDirectory"]
	return ok
}

// projectRoot is the directory Claude Code keys project memory on: the main
// checkout of a git repository (linked worktrees share it), else dir itself
// with symlinks resolved, as the CLI's own working directory is.
func projectRoot(dir string) string {
	if out, err := exec.Command("git", "-C", dir, "rev-parse", "--path-format=absolute", "--git-common-dir").Output(); err == nil {
		if common := strings.TrimSpace(string(out)); filepath.Base(common) == ".git" {
			return filepath.Dir(common)
		}
	}
	if out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output(); err == nil {
		if top := strings.TrimSpace(string(out)); top != "" {
			return top
		}
	}
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		return r
	}
	return filepath.Clean(dir)
}

// maxSlug is the longest folder name Claude Code uses unhashed.
const maxSlug = 200

// projectSlug names a project's folder under projects/ the way Claude Code
// does: every UTF-16 code unit that is not an ASCII letter or digit becomes
// "-". Names past maxSlug get a hash suffix aiq does not reproduce, so
// those return "".
func projectSlug(root string) string {
	if root == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range root {
		switch {
		case r < 0x80 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'):
			b.WriteRune(r)
		case r > 0xFFFF:
			b.WriteString("--") // a surrogate pair is two code units
		default:
			b.WriteByte('-')
		}
	}
	if b.Len() > maxSlug {
		return ""
	}
	return b.String()
}
