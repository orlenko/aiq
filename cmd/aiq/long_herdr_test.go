package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/state"
)

func TestLongBackendChoiceDoesNotConsumeProviderArgs(t *testing.T) {
	for _, args := range [][]string{{"--backend", "herdr", "codex", "--", "--backend", "literal"}, {"--backend=herdr", "auto", "resume"}} {
		backend, rest, err := longBackendArgs(args)
		if err != nil || backend != "herdr" || len(rest) == 0 {
			t.Fatalf("native flags: %q %q %v", backend, rest, err)
		}
		if args[0] == "--backend" && rest[len(rest)-1] != "literal" {
			t.Fatal("provider arguments consumed")
		}
	}
	if backend, _, err := longBackendArgs([]string{"codex"}); err != nil || backend != "" {
		t.Fatal("omitted backend was made explicit")
	}
	for _, args := range [][]string{{"--backend", "tmux", "auto"}, {"--backend=tmux", "auto"}} {
		if backend, _, err := longBackendArgs(args); err != nil || backend != "tmux" {
			t.Fatal("explicit tmux choice lost")
		}
	}
	if _, _, err := longBackendArgs([]string{"--backend=unknown", "codex"}); err == nil {
		t.Fatal("unknown backend accepted")
	}
}

func TestNativeStartOutsideHerdrDoesNotLaunchTmux(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AIQ_DATA_DIR", root)
	bin := filepath.Join(root, "bin")
	os.Mkdir(bin, 0o700)
	marker := filepath.Join(root, "tmux-started")
	os.WriteFile(filepath.Join(bin, "tmux"), []byte("#!/bin/sh\ncase \"$1\" in has-session) exit 1;; *) touch '"+marker+"';; esac\n"), 0o700)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	st, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a := &app{st: st, cfg: config.Default(), longBackend: "herdr"}
	target, attached, err := a.longExisting(root)
	defer target.unlock()
	if err == nil || attached || !strings.Contains(err.Error(), "direct Herdr-core terminal") {
		t.Fatalf("native start outside core: %v %v", attached, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("native refusal started tmux")
	}
}
