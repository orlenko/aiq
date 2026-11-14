package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

func resumeFixtureStore(t *testing.T) *state.Store {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "invented-state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestResumeFullIDAllProvidersResolveBeforeDirectoryEntry(t *testing.T) {
	for _, provider := range []string{"claude", "codex", "agy"} {
		t.Run(provider, func(t *testing.T) {
			roots := fullIDRoots(t)
			work := t.TempDir()
			if err := os.Chmod(work, 0700); err != nil {
				t.Fatal(err)
			}
			caller := t.TempDir()
			path := fullIDWrite(t, roots, provider, fullIDFixtureID, work)
			st := resumeFixtureStore(t)
			selected, err := resolveResumeSelection(st, roots, nil, nil, fullIDFixtureID)
			if err != nil || !selected.global || selected.s.Provider != provider || selected.s.ID != fullIDFixtureID || canonicalDir(selected.s.Cwd) != canonicalDir(work) || selected.s.Path != path {
				t.Fatal("wrong cross-directory selection", selected, err)
			}
			t.Chdir(caller)
			d := defaultResumeDirDeps()
			d.read = func(string) ([]byte, error) { t.Fatal("0700 fixture reached real account files"); return nil, nil }
			entered, changed, err := enterResumeDirectory(d, selected.s.Cwd, selected.s.ID)
			if err != nil || !changed || canonicalDir(entered) != canonicalDir(work) {
				t.Fatal("wrong entered directory", entered, changed, err)
			}
		})
	}
}

func TestResumeFullIDPrefixFirstAmbiguityAndPanicClass(t *testing.T) {
	local := transcript.Session{Provider: "codex", ID: "local-id", Cwd: "/invented/local"}
	if selected, err := resolveResumeSelection(nil, transcript.Roots{}, []transcript.Session{local}, nil, "local"); err != nil || selected.global || selected.s.ID != "local-id" {
		t.Fatal("prefix lookup widened", selected, err)
	}
	if _, err := resolveResumeSelection(nil, transcript.Roots{}, nil, nil, "short-prefix"); !errors.Is(err, errSessionNoMatch) {
		t.Fatal("nonfull ID searched global roots", err)
	}
	for _, id := range []string{"bad\x1bid", "bad\x7fid"} {
		if _, err := parseResumeArgs([]string{id, "--", "extra"}); err == nil || strings.ContainsAny(err.Error(), "\x1b\x7f") {
			t.Fatal("control ID accepted or echoed raw", err)
		}
	}
	if _, err := parseResumeArgs([]string{"--bare", "--launcher", "invented", "--"}); err == nil {
		t.Fatal("separator bypassed flag validation")
	}
	for _, global := range []bool{false, true} {
		roots := fullIDRoots(t)
		sessions := []transcript.Session{}
		for _, provider := range []string{"claude", "codex"} {
			if global {
				fullIDWrite(t, roots, provider, fullIDFixtureID, t.TempDir())
			} else {
				sessions = append(sessions, transcript.Session{Provider: provider, ID: fullIDFixtureID})
			}
		}
		if _, err := resolveResumeSelection(resumeFixtureStore(t), roots, sessions, nil, fullIDFixtureID); !errors.Is(err, errConvoAmbiguousID) {
			t.Fatal("two-provider exact match selected an arbitrary provider", err)
		}
	}
	roots := fullIDRoots(t)
	path := fullIDWrite(t, roots, "claude", fullIDFixtureID, t.TempDir())
	restore := transcript.SetParserForTest(func(string) (*transcript.Session, error) { panic("INVENTED PRIVATE PARSER VALUE") })
	defer restore()
	_, err := resolveResumeExact(roots, fullIDFixtureID)
	var load *convoLoadError
	if !errors.As(err, &load) || load.path != path || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatal("post-hit panic escaped recovering loader", err)
	}
}

func TestResumeNewestOverlayAccountUsesRecordedHomes(t *testing.T) {
	for _, tc := range []string{"overlay only after keep-home rename", "newest overlay", "newest real", "ambiguous shared home", "symlinked data directory"} {
		t.Run(tc, func(t *testing.T) {
			roots := fullIDRoots(t)
			if err := os.MkdirAll(roots.ClaudeProjects[0], 0700); err != nil {
				t.Fatal(err)
			}
			home := filepath.Join(t.TempDir(), "old-directory-name")
			work := t.TempDir()
			overlay := transcript.Roots{ClaudeProjects: []string{filepath.Join(home, "projects")}}
			path := fullIDWrite(t, overlay, "claude", fullIDFixtureID, work)
			accountHome := home
			if tc == "symlinked data directory" {
				alias := filepath.Join(t.TempDir(), "invented-alias")
				if err := os.Symlink(home, alias); err != nil {
					t.Fatal(err)
				}
				accountHome = alias
				roots.ClaudeProjects = append(roots.ClaudeProjects, overlay.ClaudeProjects[0])
			}
			st := resumeFixtureStore(t)
			account := state.Account{ID: "claude/new-name", Provider: "claude", Name: "new-name", Enabled: true, Home: accountHome}
			if err := st.AddAccount(account); err != nil {
				t.Fatal(err)
			}
			want := "claude/new-name"
			if tc == "ambiguous shared home" {
				account.ID = "claude/another"
				account.Name = "another"
				if err := st.AddAccount(account); err != nil {
					t.Fatal(err)
				}
				want = ""
			}
			if tc == "newest real" || tc == "newest overlay" {
				real := fullIDWrite(t, roots, "claude", fullIDFixtureID, work)
				old := time.Unix(1000, 0)
				newer := time.Unix(2000, 0)
				realTime, overlayTime := old, newer
				if tc == "newest real" {
					realTime, overlayTime = newer, old
					want = ""
					path = real
				}
				if err := os.Chtimes(real, realTime, realTime); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(filepath.Join(overlay.ClaudeProjects[0], claudeNonAlnum.ReplaceAllString(work, "-"), fullIDFixtureID+".jsonl"), overlayTime, overlayTime); err != nil {
					t.Fatal(err)
				}
			}
			selected, err := resolveResumeSelection(st, roots, nil, nil, fullIDFixtureID)
			if err != nil || selected.forced != want || canonicalDir(selected.s.Path) != canonicalDir(path) {
				t.Fatal("wrong newest-copy account or path", selected.forced, selected.s.Path, err)
			}
			local, err := resolveResumeSelection(st, roots, []transcript.Session{selected.s}, nil, fullIDFixtureID)
			if err != nil || local.global || local.forced != want {
				t.Fatal("complete local ID lost newest-copy account", local, err)
			}
			prefix, err := resolveResumeSelection(st, roots, []transcript.Session{selected.s}, nil, fullIDFixtureID[:8])
			if err != nil || prefix.global || prefix.forced != "" {
				t.Fatal("legacy prefix account routing changed", prefix, err)
			}
			if want != "" {
				a := &app{st: st, cfg: config.Default()}
				name, err := a.resumeCopyAccount("claude", want, "")
				if err != nil || name != "new-name" {
					t.Fatal("recorded account rename was guessed from directory", name, err)
				}
			}
		})
	}
}

func TestResumeAgyMissingOrAmbiguousDirectoryIsNotGuessed(t *testing.T) {
	for _, tc := range []string{"missing", "relative", "ambiguous"} {
		t.Run(tc, func(t *testing.T) {
			roots := fullIDRoots(t)
			work := t.TempDir()
			fullIDWrite(t, roots, "agy", fullIDFixtureID, work)
			history := filepath.Join(roots.AgyAppData[0], "history.jsonl")
			switch tc {
			case "missing":
				if err := os.WriteFile(history, nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "relative":
				goldenWrite(t, history, `{"workspace":"relative","conversationId":"`+fullIDFixtureID+`"}`)
			case "ambiguous":
				goldenWrite(t, history, `{"workspace":"/invented/one","conversationId":"`+fullIDFixtureID+`"}`, `{"workspace":"/invented/two","conversationId":"`+fullIDFixtureID+`"}`)
			}
			selected, err := resolveResumeExact(roots, fullIDFixtureID)
			if err != nil {
				t.Fatal(err)
			}
			if selected.s.Cwd != "" {
				t.Fatal("AGY missing directory guessed", selected.s.Cwd)
			}
			if _, _, err := enterResumeDirectory(resumeDirFixture(&fixtureResumeDir{}), selected.s.Cwd, selected.s.ID); err == nil {
				t.Fatal("missing directory could start a provider")
			}
		})
	}
}
