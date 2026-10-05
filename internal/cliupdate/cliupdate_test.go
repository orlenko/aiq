package cliupdate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// npmTree lays out an npm global prefix with pkg at version, the way
// Homebrew's and nvm's node do, and returns the prefix and the bin link.
func npmTree(t *testing.T, provider, version string) (prefix, bin string) {
	t.Helper()
	prefix, _ = filepath.EvalSymlinks(t.TempDir()) // macOS: /var is /private/var
	pkgDir := filepath.Join(prefix, "lib", "node_modules", filepath.FromSlash(Packages[provider]))
	if err := os.MkdirAll(filepath.Join(pkgDir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeVersion(t, pkgDir, version)
	script := filepath.Join(pkgDir, "bin", provider+".js")
	os.WriteFile(script, []byte("#!/usr/bin/env node\n"), 0o755)
	os.MkdirAll(filepath.Join(prefix, "bin"), 0o755)
	bin = filepath.Join(prefix, "bin", provider)
	if err := os.Symlink(script, bin); err != nil {
		t.Fatal(err)
	}
	return prefix, bin
}

func writeVersion(t *testing.T, pkgDir, version string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(pkgDir, "package.json"), []byte(fmt.Sprintf(`{"name":"x","version":%q}`, version)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDetect(t *testing.T) {
	prefix, bin := npmTree(t, "codex", "0.159.3")
	in, ok := Detect("codex", bin)
	if !ok || in.Version != "0.159.3" || in.Package != "@openai/codex" || in.npm() != filepath.Join(prefix, "bin", "npm") {
		t.Fatalf("got %+v, %v", in, ok)
	}
	// Claude Code's native installer keeps versions under ~/.local/share/claude.
	native := filepath.Join(t.TempDir(), "claude", "versions", "2.1.289")
	os.MkdirAll(filepath.Dir(native), 0o755)
	os.WriteFile(native, []byte{}, 0o755)
	if _, ok := Detect("claude", native); ok {
		t.Fatal("a native Claude install was taken for npm")
	}
	if _, ok := Detect("agy", bin); ok {
		t.Fatal("a provider with no npm package was detected")
	}
}

func TestNewer(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"0.160.0", "0.159.3", true},
		{"2.1.290", "2.1.289", true},
		{"2.1.289", "2.1.289", false},
		{"0.159.3", "0.160.0", false},
		{"1.0.0-beta.1", "0.9.9", true},
		{"", "0.1.0", false},
	} {
		if got := Newer(tc.a, tc.b); got != tc.want {
			t.Errorf("Newer(%q, %q) = %v", tc.a, tc.b, got)
		}
	}
}

func TestWaitHoldsLaunchesWhileTheMarkerStands(t *testing.T) {
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	marker := MarkerPath("codex")
	os.MkdirAll(filepath.Dir(marker), 0o700)
	os.WriteFile(marker, nil, 0o600)
	go func() {
		time.Sleep(1500 * time.Millisecond)
		os.Remove(marker)
	}()
	told := 0
	start := time.Now()
	Wait("codex", time.Minute, func() { told++ })
	if waited := time.Since(start); waited < time.Second || told != 1 {
		t.Fatalf("waited %s, told %d times", waited, told)
	}

	// A marker an updater left behind when it died does not hold anyone.
	os.WriteFile(marker, nil, 0o600)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(marker, old, old)
	start = time.Now()
	Wait("codex", time.Minute, nil)
	if time.Since(start) > time.Second {
		t.Fatal("a stale marker held the launch")
	}
}

// fakeNPM answers `npm view` with latest, makes `npm install -g pkg@v`
// write v into package.json (unless v is in broken), and fails
// `<bin> --version` while broken holds the installed version.
func fakeNPM(t *testing.T, prefix, provider, latest string, broken map[string]bool, calls *[]string) Runner {
	pkgDir := filepath.Join(prefix, "lib", "node_modules", filepath.FromSlash(Packages[provider]))
	return func(ctx context.Context, env []string, name string, args ...string) (string, error) {
		*calls = append(*calls, filepath.Base(name)+" "+strings.Join(args, " "))
		if _, err := os.Stat(MarkerPath(provider)); err != nil && filepath.Base(name) == "npm" && args[0] == "install" {
			t.Errorf("npm install ran without the marker")
		}
		switch {
		case filepath.Base(name) == "npm" && args[0] == "view":
			return "npm warn something\n" + latest + "\n", nil
		case filepath.Base(name) == "npm" && args[0] == "install":
			if args[2] != "--prefix" || args[3] != prefix {
				t.Errorf("npm install without --prefix %s: %v", prefix, args)
			}
			spec := args[len(args)-1]
			v := spec[strings.LastIndex(spec, "@")+1:]
			writeVersion(t, pkgDir, v)
			return "added 1 package", nil
		case args[0] == "--version":
			b, _ := os.ReadFile(filepath.Join(pkgDir, "package.json"))
			for v := range broken {
				if strings.Contains(string(b), `"`+v+`"`) {
					return "Error: cannot find module", fmt.Errorf("exit status 1")
				}
			}
			return "codex-cli", nil
		}
		return "", fmt.Errorf("unexpected %s %v", name, args)
	}
}

func newUpdater(bin string, run Runner, events *[]string) *Updater {
	return &Updater{
		Resolve:  func(string) (string, error) { return bin, nil },
		Interval: 24 * time.Hour,
		Logf:     func(string, ...any) {},
		Event:    func(provider, detail string) { *events = append(*events, detail) },
		Run:      run,
	}
}

func TestCheckUpdatesToTheLatestRelease(t *testing.T) {
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	prefix, bin := npmTree(t, "codex", "0.159.3")
	var calls, events []string
	u := newUpdater(bin, fakeNPM(t, prefix, "codex", "0.160.0", nil, &calls), &events)
	if got := u.Check("codex"); got != "codex 0.159.3 -> 0.160.0" {
		t.Fatalf("got %q; calls %v", got, calls)
	}
	if in, _ := Detect("codex", bin); in.Version != "0.160.0" {
		t.Fatalf("installed %s", in.Version)
	}
	if Updating("codex", time.Now()) {
		t.Fatal("the marker outlived the update")
	}
	// Up to date: nothing to install.
	calls = nil
	if got := u.Check("codex"); got != "" || len(calls) != 1 {
		t.Fatalf("got %q; calls %v", got, calls)
	}
}

func TestCheckPutsBackAReleaseThatWillNotStart(t *testing.T) {
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	prefix, bin := npmTree(t, "codex", "0.159.3")
	var calls, events []string
	u := newUpdater(bin, fakeNPM(t, prefix, "codex", "0.160.0", map[string]bool{"0.160.0": true}, &calls), &events)
	got := u.Check("codex")
	if !strings.Contains(got, "failed") || !strings.Contains(got, "putting 0.159.3 back") || len(events) != 1 {
		t.Fatalf("got %q, events %v", got, events)
	}
	if in, _ := Detect("codex", bin); in.Version != "0.159.3" {
		t.Fatalf("installed %s after the rollback", in.Version)
	}
	if want := "@openai/codex@0.159.3"; !strings.Contains(strings.Join(calls, "\n"), "npm install -g --prefix "+prefix+" "+want) {
		t.Fatalf("no rollback install in %v", calls)
	}
}

func TestTickChecksEachProviderOncePerInterval(t *testing.T) {
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	_, bin := npmTree(t, "codex", "0.160.0")
	var calls, events []string
	resolved := 0
	u := newUpdater(bin, func(ctx context.Context, env []string, name string, args ...string) (string, error) {
		calls = append(calls, name)
		return "0.160.0", nil
	}, &events)
	u.Resolve = func(string) (string, error) { resolved++; return bin, nil }
	now := time.Now()
	u.Tick(now)
	u.Tick(now.Add(time.Hour))
	u.Tick(now.Add(25 * time.Hour))
	if resolved != 4 { // claude and codex, twice
		t.Fatalf("resolved %d times, want 4", resolved)
	}
}
