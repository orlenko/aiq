package herdrmod

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/testenv"
)

func TestMain(m *testing.M) { testenv.Main(m) }

func TestInstallPreservesPersonalChanges(t *testing.T) {
	home := t.TempDir()
	if err := Install(home); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(filepath.Join(home, "mods", "aiq", "main.js"))
	if err != nil {
		t.Fatal(err)
	}
	if err := Install(home); err != nil {
		t.Fatal("managed update:", err)
	}
	after, err := os.Stat(filepath.Join(home, "mods", "aiq", "main.js"))
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("unchanged install would reload the active view", err)
	}
	path := filepath.Join(home, "mods", "aiq", "main.js")
	if err := os.WriteFile(path, []byte("// personal changes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Install(home); err == nil {
		t.Fatal("overwrote personal changes")
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "// personal changes" {
		t.Fatal("personal file changed", err)
	}
	entries, err := os.ReadDir(filepath.Join(home, "mods"))
	if err != nil || len(entries) != 1 {
		t.Fatal("staging or lock left behind", err)
	}
}

func TestInstallRefusesUnownedAndLinkedMods(t *testing.T) {
	for _, kind := range []string{"unowned", "extra", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			home := t.TempDir()
			other := t.TempDir()
			target := filepath.Join(home, "mods", "aiq")
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "unowned":
				if err := os.Mkdir(target, 0700); err != nil {
					t.Fatal(err)
				}
			case "extra":
				if err := Install(home); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(target, "personal.js"), []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(other, target); err != nil {
					t.Skip("symlinks unavailable")
				}
			}
			if err := Install(home); err == nil {
				t.Fatal("replaced an unowned mod")
			}
		})
	}
}

func TestInstallPreservesHerdrGeneratedFiles(t *testing.T) {
	home := t.TempDir()
	if err := Install(home); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "mods", "aiq", "gpui-kit.d.ts")
	if err := os.WriteFile(path, []byte("// generated SDK with personal notes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Install(home); err != nil {
		t.Fatal("generated SDK prevented update", err)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "// generated SDK with personal notes" {
		t.Fatal("changed the SDK", err)
	}
}

func TestNativeClient(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node required in CI")
		}
		t.Skip("node unavailable; native client tests did not run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "--experimental-loader", "./native_loader.mjs", "--test", "client_test.mjs", "interaction_test.mjs")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("native client: %v\n%s", err, output)
	}
}
