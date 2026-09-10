package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidLauncherName(t *testing.T) {
	for _, ok := range []string{"boxed", "my-box", "box.2", "b", "a_b"} {
		if err := ValidLauncherName(ok); err != nil {
			t.Errorf("%q should be valid: %v", ok, err)
		}
	}
	// An aiq command, and the words `aiq long` has to tell from a launcher.
	for _, bad := range []string{"status", "long", "claude", "codex", "list", "attach", "launcher"} {
		if err := ValidLauncherName(bad); err == nil {
			t.Errorf("%q must be refused: it would shadow a command", bad)
		}
	}
	for _, bad := range []string{"", "Boxed", "-box", "a b", "box/es", strings.Repeat("x", 65)} {
		if err := ValidLauncherName(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

func writeConfig(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIQ_CONFIG", path)
}

// The key that wrapped every session of a provider is gone; a config still
// carrying it has to say so rather than silently doing nothing.
func TestLoadRejectsTheOldWrapperKey(t *testing.T) {
	writeConfig(t, "version = 2\n[providers.claude]\nwrapper = \"/bin/true\"\n")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "launcher add") {
		t.Fatalf("want a message pointing at launchers, got %v", err)
	}
}

func TestLoadValidatesLaunchers(t *testing.T) {
	writeConfig(t, "version = 2\n[launchers.boxed]\nprovider = \"claude\"\ncommand = \"/usr/local/bin/boxed\"\n")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	l, ok := c.Launcher("boxed")
	if !ok || l.Provider != "claude" || l.Command != "/usr/local/bin/boxed" {
		t.Fatalf("launcher not loaded: %+v", l)
	}

	for _, bad := range []string{
		"version = 2\n[launchers.boxed]\nprovider = \"gemini\"\ncommand = \"/bin/true\"\n",
		"version = 2\n[launchers.boxed]\nprovider = \"claude\"\n",
		"version = 2\n[launchers.status]\nprovider = \"claude\"\ncommand = \"/bin/true\"\n",
		"version = 2\n[launchers.boxed]\nprovider = \"claude\"\ncommand = \"/bin/true\"\ncredential = \"keyring\"\n",
	} {
		writeConfig(t, bad)
		if _, err := Load(); err == nil {
			t.Errorf("must be refused:\n%s", bad)
		}
	}
}
