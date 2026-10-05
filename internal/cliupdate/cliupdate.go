// Package cliupdate keeps npm-installed provider CLIs current in the
// background, so a session gets the new version the next time it starts
// (by hand, or when the daemon moves it to another account) without anyone
// stopping work to upgrade.
//
// Only npm installs are touched. Claude Code's native installer updates
// itself, and anything else (a Homebrew cask, a hand-built binary) is left to
// whoever installed it. The update pins the version it found, through the
// npm of the installation the launches use, so a release that will not
// start can be put back exactly.
//
// An npm reinstall removes the package for a while. A marker file stands
// while one runs, and every launch waits for it (see Wait), so neither a
// takeover nor a worker starts a half-installed CLI.
package cliupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/paths"
)

// Packages maps a provider to the npm package its CLI ships in.
var Packages = map[string]string{
	"claude": "@anthropic-ai/claude-code",
	"codex":  "@openai/codex",
}

// Install is a CLI that runs out of an npm global package.
type Install struct {
	Provider string
	Package  string
	Bin      string // the executable launches resolve
	Prefix   string // npm's global prefix: Prefix/lib/node_modules/Package
	Version  string
}

func (in Install) pkgDir() string {
	return filepath.Join(in.Prefix, "lib", "node_modules", filepath.FromSlash(in.Package))
}

// Detect reports whether bin runs out of provider's npm package, and where.
func Detect(provider, bin string) (Install, bool) {
	pkg, ok := Packages[provider]
	if !ok || bin == "" {
		return Install{}, false
	}
	real, err := filepath.EvalSymlinks(bin)
	if err != nil {
		return Install{}, false
	}
	marker := string(filepath.Separator) + filepath.Join("lib", "node_modules", filepath.FromSlash(pkg)) + string(filepath.Separator)
	i := strings.Index(real, marker)
	if i < 0 {
		return Install{}, false
	}
	in := Install{Provider: provider, Package: pkg, Bin: bin, Prefix: real[:i]}
	in.Version = installedVersion(in.pkgDir())
	return in, in.Version != ""
}

func installedVersion(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return ""
	}
	var doc struct {
		Version string `json:"version"`
	}
	json.Unmarshal(b, &doc)
	return doc.Version
}

// Newer reports whether version a is newer than b, comparing the numeric
// parts of major.minor.patch. A prerelease suffix is ignored.
func Newer(a, b string) bool {
	pa, pb := parts(a), parts(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

func parts(v string) [3]int {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	for i, p := range strings.SplitN(v, ".", 3) {
		out[i], _ = strconv.Atoi(p)
	}
	return out
}

// --- the marker launches wait on ---

// markerStale bounds how long a marker is believed: an updater that died
// mid-install must not hold every launch forever.
const markerStale = 15 * time.Minute

// MarkerPath is the file that stands while provider's CLI is reinstalled.
func MarkerPath(provider string) string {
	return filepath.Join(paths.DataDir(), "updating", provider)
}

// Updating reports whether provider's CLI is being reinstalled right now.
func Updating(provider string, now time.Time) bool {
	fi, err := os.Stat(MarkerPath(provider))
	return err == nil && now.Sub(fi.ModTime()) < markerStale
}

// Wait blocks while provider's CLI is being reinstalled, for at most max.
// notify runs once if it has to wait at all.
func Wait(provider string, max time.Duration, notify func()) {
	deadline := time.Now().Add(max)
	for told := false; Updating(provider, time.Now()) && time.Now().Before(deadline); time.Sleep(time.Second) {
		if !told && notify != nil {
			notify()
			told = true
		}
	}
}

// --- the updater ---

// Runner runs a command and returns its combined output.
type Runner func(ctx context.Context, env []string, name string, args ...string) (string, error)

func execRunner(ctx context.Context, env []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// Updater checks each provider's CLI at most once per Interval.
type Updater struct {
	// Resolve finds the CLI binary a launch would exec.
	Resolve  func(provider string) (string, error)
	Interval time.Duration
	Logf     func(format string, v ...any)
	// Event records an outcome worth keeping (an update, a rollback).
	Event func(provider, detail string)
	Run   Runner // nil runs commands for real

	last map[string]time.Time
}

// Tick checks every provider whose interval has passed.
func (u *Updater) Tick(now time.Time) {
	if u.last == nil {
		u.last = map[string]time.Time{}
	}
	for _, provider := range []string{"claude", "codex"} {
		if t, ok := u.last[provider]; ok && now.Sub(t) < u.Interval {
			continue
		}
		u.last[provider] = now
		u.Check(provider)
	}
}

// Check updates provider's CLI if it is an npm install with a newer
// release, and returns what happened ("" when nothing was due).
func (u *Updater) Check(provider string) string {
	bin, err := u.Resolve(provider)
	if err != nil {
		return ""
	}
	in, ok := Detect(provider, bin)
	if !ok {
		return "" // native or unmanaged: it updates itself, or its owner does
	}
	env := in.env()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	latest, err := u.run(ctx, env, in.npm(), "view", in.Package, "version")
	cancel()
	if err != nil {
		u.Logf("update %s: npm view %s: %v: %s", provider, in.Package, err, lastLine(latest))
		return ""
	}
	latest = lastLine(latest)
	if !Newer(latest, in.Version) {
		return ""
	}
	return u.update(in, latest)
}

func (u *Updater) update(in Install, latest string) string {
	marker := MarkerPath(in.Provider)
	os.MkdirAll(filepath.Dir(marker), 0o700)
	if err := os.WriteFile(marker, []byte(fmt.Sprintf("%s %s -> %s\n", in.Package, in.Version, latest)), 0o600); err != nil {
		u.Logf("update %s: %v", in.Provider, err)
		return ""
	}
	defer os.Remove(marker)

	if err := u.install(in, latest); err != nil {
		detail := fmt.Sprintf("%s %s -> %s failed (%v); putting %s back", in.Provider, in.Version, latest, err, in.Version)
		if rerr := u.install(in, in.Version); rerr != nil {
			detail += fmt.Sprintf(", and that failed too (%v): fix it by hand", rerr)
		}
		u.Logf("update %s", detail)
		u.Event(in.Provider, detail)
		return detail
	}
	detail := fmt.Sprintf("%s %s -> %s", in.Provider, in.Version, latest)
	u.Logf("update %s", detail)
	u.Event(in.Provider, detail)
	return detail
}

// install puts version in place and checks that it starts.
func (u *Updater) install(in Install, version string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	// --prefix pins the target: npm's own idea of its global prefix can be
	// another node's (a symlinked npm, an npmrc), and the update must land
	// in the package the launches run.
	if out, err := u.run(ctx, in.env(), in.npm(), "install", "-g", "--prefix", in.Prefix, in.Package+"@"+version); err != nil {
		return fmt.Errorf("npm install: %v: %s", err, lastLine(out))
	}
	if got := installedVersion(in.pkgDir()); got != version {
		return fmt.Errorf("npm left %q installed", got)
	}
	vctx, vcancel := context.WithTimeout(context.Background(), time.Minute)
	defer vcancel()
	if out, err := u.run(vctx, in.env(), in.Bin, "--version"); err != nil {
		return fmt.Errorf("%s --version: %v: %s", in.Provider, err, lastLine(out))
	}
	return nil
}

func (u *Updater) run(ctx context.Context, env []string, name string, args ...string) (string, error) {
	if u.Run != nil {
		return u.Run(ctx, env, name, args...)
	}
	return execRunner(ctx, env, name, args...)
}

// npm is the installation's own npm, so the update lands where the
// launches look, whichever node (Homebrew, nvm) the shell would pick.
func (in Install) npm() string { return filepath.Join(in.Prefix, "bin", "npm") }

// env puts the installation's node first: npm and the CLI's own launcher
// script find node through PATH.
func (in Install) env() []string {
	env := os.Environ()
	for i, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			env[i] = "PATH=" + filepath.Join(in.Prefix, "bin") + string(os.PathListSeparator) + strings.TrimPrefix(kv, "PATH=")
			return env
		}
	}
	return append(env, "PATH="+filepath.Join(in.Prefix, "bin"))
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(s)
}
