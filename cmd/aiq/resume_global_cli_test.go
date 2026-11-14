package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/longrun"
	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

type resumeCLIFixture struct {
	a                         *app
	root, caller, target, bin string
}

func newResumeCLIFixture(t *testing.T) resumeCLIFixture {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("owned POSIX command fixtures")
	}
	root := t.TempDir()
	caller := filepath.Join(root, "caller")
	target := filepath.Join(root, "target")
	bin := filepath.Join(root, "bin")
	for _, path := range []string{caller, target, bin, filepath.Join(root, "data"), filepath.Join(root, "home"), filepath.Join(root, "real-claude"), filepath.Join(root, "real-codex")} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("AIQ_DATA_DIR", filepath.Join(root, "data"))
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "real-claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "real-codex"))
	t.Setenv("AIQ_BYPASS", "")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AIQ_TEST_RESUME_OUT", bin)
	t.Setenv("AIQ_TEST_RESUME_GIT_ROOT", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("TMUX", "")
	git := "#!/bin/sh\n[ \"$*\" = '--no-optional-locks -c core.fsmonitor=false -c core.hooksPath=/dev/null rev-parse --show-toplevel' ] || exit 2\nprintf '%s\\n' \"$*\" >> \"$AIQ_TEST_RESUME_OUT/git-args\"\nif [ -n \"$AIQ_TEST_RESUME_GIT_ROOT\" ]; then printf '%s\\n' \"$AIQ_TEST_RESUME_GIT_ROOT\"; else pwd -P; fi\n"
	tmux := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$AIQ_TEST_RESUME_OUT/tmux-args\"\ncase \"$1\" in\nhas-session) [ \"$3\" = \"$AIQ_TEST_RESUME_CALLER_NAME\" ] && exit 0; exit 1;;\nnew-session) printf '%s' \"$6\" > \"$AIQ_TEST_RESUME_OUT/dir\"; printf '%s' \"$9\" > \"$AIQ_TEST_RESUME_OUT/command\";;\nlist-panes) printf '%%1\\n';;\nattach-session|select-pane|kill-session) exit 2;;\nesac\n"
	for name, body := range map[string]string{"git": git, "tmux": tmux} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	st, err := state.Open(paths.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Default()
	a := &app{cfg: cfg, st: st, longBackend: "tmux"}
	t.Chdir(caller)
	t.Setenv("AIQ_TEST_RESUME_CALLER_NAME", longrun.SessionName(cfg.Long.TmuxPrefix, canonicalDir(caller)))
	return resumeCLIFixture{a, root, caller, target, bin}
}

func TestResumeGlobalRunAutoUsesRecordedDirectoryForEveryProvider(t *testing.T) {
	for _, provider := range []string{"claude", "codex", "agy"} {
		t.Run(provider, func(t *testing.T) {
			f := newResumeCLIFixture(t)
			fullIDWrite(t, defaultRoots(), provider, fullIDFixtureID, f.target)
			got, args, err := f.a.runAutoResumeArgs([]string{fullIDFixtureID})
			if err != nil || got != provider || !strings.Contains(strings.Join(args, " "), fullIDFixtureID) {
				t.Fatal("wrong resumed provider/session", got, args, err)
			}
			cwd, err := os.Getwd()
			if err != nil || canonicalDir(cwd) != canonicalDir(f.target) {
				t.Fatal("run auto used caller directory", cwd, err)
			}
			if _, err := os.Stat(filepath.Join(f.bin, "tmux-args")); !os.IsNotExist(err) {
				t.Fatal("run auto invoked a workspace terminal action")
			}
		})
	}
}

func TestResumeGlobalLongResolvesBeforeExistingCallerWorkspace(t *testing.T) {
	for _, provider := range []string{"claude", "codex", "agy"} {
		t.Run(provider, func(t *testing.T) {
			f := newResumeCLIFixture(t)
			fullIDWrite(t, defaultRoots(), provider, fullIDFixtureID, f.target)
			if err := f.a.longResume([]string{fullIDFixtureID}, true); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(f.bin, "dir"))
			if err != nil || canonicalDir(string(data)) != canonicalDir(f.target) {
				t.Fatal("long launch used caller instead of recorded cwd", string(data), err)
			}
			command, err := os.ReadFile(filepath.Join(f.bin, "command"))
			if err != nil || !strings.Contains(string(command), "--resume-session "+fullIDFixtureID) {
				t.Fatal("wrong long session command", string(command), err)
			}
			calls, err := os.ReadFile(filepath.Join(f.bin, "tmux-args"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(calls), longrun.SessionName(f.a.cfg.Long.TmuxPrefix, canonicalDir(f.caller))) || strings.Contains(string(calls), "attach-session") {
				t.Fatal("caller workspace was inspected or attached before resolving ID")
			}
			git, err := os.ReadFile(filepath.Join(f.bin, "git-args"))
			if err != nil || !strings.Contains(string(git), "--no-optional-locks -c core.fsmonitor=false -c core.hooksPath=/dev/null rev-parse --show-toplevel") {
				t.Fatal("workspace ID ran unhardened git", err)
			}
		})
	}
}

func TestResumeGlobalUnsafeTargetAndWorkspaceNeverReachTmux(t *testing.T) {
	for _, tc := range []string{"unsafe recorded directory", "shared workspace", "launcher shared workspace", "run launcher shared workspace", "plain launcher shared workspace"} {
		t.Run(tc, func(t *testing.T) {
			f := newResumeCLIFixture(t)
			fullIDWrite(t, defaultRoots(), "claude", fullIDFixtureID, f.target)
			args := []string{fullIDFixtureID}
			want := "other_write"
			if tc == "unsafe recorded directory" {
				if err := os.Chmod(f.target, 0777); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Chmod(f.root, 0775); err != nil {
					t.Fatal(err)
				}
				t.Setenv("AIQ_TEST_RESUME_GIT_ROOT", f.root)
				facts, err := statResumeDir(f.root)
				if err != nil {
					t.Fatal(err)
				}
				previous := resumeDirectoryRuntime
				resumeDirectoryRuntime = func() resumeDirDeps {
					d := defaultResumeDirDeps()
					d.read = func(path string) ([]byte, error) {
						if path == "/etc/passwd" {
							return []byte(fmt.Sprintf("invented:x:%d:%d::/invented:/bin/sh\n", facts.uid, facts.gid)), nil
						}
						return []byte(fmt.Sprintf("shared:x:%d:invented,another\n", facts.gid)), nil
					}
					return d
				}
				t.Cleanup(func() { resumeDirectoryRuntime = previous })
				want = "workspace_group_unconfirmed"
				if runtime.GOOS != "linux" {
					want = "workspace_not_linux"
				}
				if strings.Contains(tc, "launcher") {
					f.a.cfg.Launchers = map[string]config.Launcher{"fixture": {Provider: "claude"}}
					args = []string{"--launcher", "fixture", fullIDFixtureID}
				}
			}
			var err error
			switch tc {
			case "run launcher shared workspace":
				_, _, err = f.a.runAutoResumeArgs(args)
			case "plain launcher shared workspace":
				if _, _, e := enterResumeDirectory(resumeDirectoryRuntime(), f.target, fullIDFixtureID); e != nil {
					t.Fatal(e)
				}
				err = f.a.resumeSession(transcript.Session{Provider: "claude", ID: fullIDFixtureID, Cwd: f.target}, nil, liveNote{tmux: longrun.SessionName(f.a.cfg.Long.TmuxPrefix, canonicalDir(f.caller))}, resumeOpts{launcher: "fixture", changedDir: true})
			default:
				err = f.a.longResume(args, false)
			}
			resumeReason(t, err, want)
			if _, err := os.Stat(filepath.Join(f.bin, "tmux-args")); !os.IsNotExist(err) {
				t.Fatal("unsafe target reached terminal decisions")
			}
			cwd, _ := os.Getwd()
			if canonicalDir(cwd) != canonicalDir(f.target) {
				t.Fatal("refusal fell back to caller", cwd)
			}
		})
	}
}

func TestResumeGlobalForcedAccountConflictFailsBeforeDirectoryChange(t *testing.T) {
	f := newResumeCLIFixture(t)
	home := filepath.Join(f.root, "kept-overlay-name")
	roots := defaultRoots()
	roots.ClaudeProjects = []string{filepath.Join(home, "projects")}
	fullIDWrite(t, roots, "claude", fullIDFixtureID, f.target)
	for _, account := range []state.Account{{ID: "claude/renamed", Provider: "claude", Name: "renamed", Enabled: true, Home: home}, {ID: "claude/requested", Provider: "claude", Name: "requested", Enabled: true}} {
		if err := f.a.st.AddAccount(account); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := f.a.runAutoResumeArgs([]string{"--account", "requested", fullIDFixtureID}); err == nil || !strings.Contains(err.Error(), "cannot read that copy") {
		t.Fatal("unreadable account override accepted", err)
	}
	cwd, _ := os.Getwd()
	if canonicalDir(cwd) != canonicalDir(f.caller) {
		t.Fatal("account conflict changed directory")
	}
	if _, err := os.Stat(filepath.Join(f.bin, "tmux-args")); !os.IsNotExist(err) {
		t.Fatal("account conflict reached a terminal action")
	}
}
