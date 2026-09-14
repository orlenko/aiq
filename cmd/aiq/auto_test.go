package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/proc"
	"github.com/orlenko/aiq/internal/selector"
	"github.com/orlenko/aiq/internal/state"
)

func TestAutoArguments(t *testing.T) {
	for tier := 0; tier <= 3; tier++ {
		for effort := 1; effort <= 6; effort++ {
			f, r, err := parseAutoFlags([]string{"-p", "do this 'thing'\n--effort", "--model-tier", fmt.Sprint(tier), "--effort=" + fmt.Sprint(effort)})
			if err != nil {
				t.Fatal(err)
			}
			for _, provider := range []string{"claude", "codex"} {
				f.rest = r.args(provider)
				got, err := modelFlags(provider, f)
				if err != nil {
					t.Fatal(err)
				}
				level := []string{"low", "medium", "high", "xhigh", "max", "ultra"}[effort-1]
				var want []string
				if provider == "claude" {
					if effort == 6 {
						level = "ultracode"
					}
					want = []string{"--model", tierModels[provider][tier], "--effort", level, "-p", "--dangerously-skip-permissions", "--", r.prompt}
				} else {
					want = []string{"exec", "--model", tierModels[provider][tier], "-c", "model_reasoning_effort=" + level, "--yolo", "--", r.prompt}
				}
				if !reflect.DeepEqual(got.rest, want) {
					t.Fatalf("%s tier %d effort %d: %q != %q", provider, tier, effort, got.rest, want)
				}
			}
		}
	}
	f, r, err := parseAutoFlags(nil)
	if err != nil || *f.modelTier != 1 || f.effort != 0 || r.print {
		t.Fatalf("defaults: %+v %+v %v", f, r, err)
	}
	for _, args := range [][]string{
		{"--model-tier", "4"}, {"--model-tier=-1"}, {"--model-tier"},
		{"--effort", "0"}, {"--effort=7"}, {"--effort=high"}, {"--effort"},
		{"-p"}, {"-p", "a", "-p", "b"}, {"--account", "test"},
		{"--launcher", "test"}, {"--long"}, {"--model-scope", "fable"}, {"--unknown"},
	} {
		if _, _, err := parseAutoFlags(args); err == nil {
			t.Errorf("accepted invalid args %q", args)
		}
	}
}

func TestAutoImplicitYolo(t *testing.T) {
	for _, input := range [][]string{nil, {"--yolo"}} {
		_, request, err := parseAutoFlags(input)
		if err != nil {
			t.Fatal(err)
		}
		for provider, flag := range map[string]string{"claude": "--dangerously-skip-permissions", "codex": "--yolo"} {
			if got := request.args(provider); !reflect.DeepEqual(got, []string{flag}) {
				t.Fatalf("%s interactive args: %q", provider, got)
			}
		}
	}
}

func TestModelFlagsExplicitProviders(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		f := parseRunFlags([]string{"--model-tier=0", "--effort", "6", "--", "--", "--model=literal prompt"})
		got, err := modelFlags(provider, f)
		if err != nil || got.modelScope != map[string]string{"claude": "fable", "codex": "astra"}[provider] {
			t.Fatalf("%+v %v", got, err)
		}
		for _, rest := range [][]string{{"--model", "other"}, {"--model=other"}, {"--effort", "low"}} {
			f.rest = rest
			if _, err := modelFlags(provider, f); err == nil {
				t.Errorf("accepted conflicting %q", rest)
			}
		}
	}
}

func TestAutoRanking(t *testing.T) {
	now := time.Now()
	policies := map[string]selector.Policy{}
	for _, p := range []string{"claude", "codex"} {
		policies[p] = selector.Policy{Now: now, Mode: state.ModeWorker, MaxWorkers: 1, ModelScope: map[string]string{"claude": "fable", "codex": "astra"}[p]}
	}
	window := func(scope string, used float64) state.Window {
		return state.Window{Scope: scope, Kind: state.KindWeekly, UsedPct: used, ResetsAt: now.Add(time.Hour).Unix(), ObservedAt: now.Unix()}
	}
	cands := []selector.Candidate{
		{ID: "claude/a", Enabled: true, HasCredential: true, Windows: []state.Window{window("", 0), window("Fable", 100)}},
		{ID: "codex/b", Enabled: true, HasCredential: true, Windows: []state.Window{window("", 50)}},
	}
	if got := rankAuto(policies, cands); got[0].ID != "codex/b" || !got[0].Eligible {
		t.Fatalf("Fable cap ignored: %+v", got)
	}
	p := policies["claude"]
	p.ModelScope = "opus"
	policies["claude"] = p
	if got := rankAuto(policies, cands); got[0].ID != "claude/a" {
		t.Fatalf("Opus bound by Fable cap: %+v", got)
	}
	cands[0].WorkerLeases = 1
	if got := rankAuto(policies, cands); got[0].ID != "codex/b" {
		t.Fatalf("worker cap ignored: %+v", got)
	}
	cands[1].CooldownUntil = now.Add(time.Hour).Unix()
	for _, got := range rankAuto(policies, cands) {
		if got.Eligible {
			t.Fatalf("exhausted pool: %+v", got)
		}
	}
}

func TestAutoEndToEnd(t *testing.T) {
	// Real CLI, database, worker trampoline, leases and retries; only the AI
	// executables are fakes. No accounts, credentials or quota outside temp dirs.
	dir := t.TempDir()
	bin := filepath.Join(dir, "aiq")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	for _, tt := range []struct {
		name                  string
		claudeUsed, codexUsed float64
		rejectClaude          bool
		want                  string
		code                  int
	}{
		{"claude available", 0, 100, false, "claude", 0},
		{"codex available", 100, 0, false, "codex", 0},
		{"both dry", 100, 100, false, "", 75},
		{"retry across providers", 0, 50, true, "codex", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("AIQ_CONFIG", filepath.Join(root, "config.toml"))
			t.Setenv("AIQ_DATA_DIR", root)
			cfg := config.Default()
			cfg.Daemon.Listen = "127.0.0.1:1"
			cfg.Worker.Retry = true
			cfg.Worker.WaitForSlotSeconds = 0
			for _, p := range []string{"claude", "codex"} {
				path := filepath.Join(root, "fake-"+p)
				script := "#!/bin/sh\nprintf '%s\\n' \"$AIQ_PROVIDER/$AIQ_ACCOUNT\" \"$@\"\n"
				if p == "claude" && tt.rejectClaude {
					script = "#!/bin/sh\nprintf 'hit your usage limit\\n' >&2\nexit 1\n"
				}
				if err := os.WriteFile(path, []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				if p == "claude" {
					cfg.Providers.Claude.Binary = path
				} else {
					cfg.Providers.Codex.Binary = path
				}
			}
			if err := config.Save(cfg); err != nil {
				t.Fatal(err)
			}
			st, err := state.Open(filepath.Join(root, "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			for _, p := range []string{"claude", "codex"} {
				id := p + "/test"
				if err := st.AddAccount(state.Account{ID: id, Provider: p, Name: "test", Enabled: true, Native: true, Home: root}); err != nil {
					t.Fatal(err)
				}
				used := tt.claudeUsed
				if p == "codex" {
					used = tt.codexUsed
				}
				if err := st.UpsertWindow(state.Window{AccountID: id, Key: "weekly", Kind: state.KindWeekly, UsedPct: used, ResetsAt: time.Now().Add(time.Hour).Unix(), ObservedAt: time.Now().Unix()}); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(bin, "run", "auto", "-p", "do this thing", "--model-tier", "0", "--effort", "6")
			cmd.Env = proc.SanitizeEnv(os.Environ(), "AIQ_BYPASS", "AIQ_MODE", "AIQ_WAIT", "AIQ_ACCOUNT", "AIQ_DEPTH", chainVar)
			out, err := cmd.CombinedOutput()
			code := 0
			if err != nil {
				if ee, ok := err.(*exec.ExitError); ok {
					code = ee.ExitCode()
				} else {
					t.Fatal(err)
				}
			}
			if code != tt.code {
				t.Fatalf("exit %d want %d: %s", code, tt.code, out)
			}
			if tt.want != "" {
				f, r, _ := parseAutoFlags([]string{"-p", "do this thing", "--model-tier", "0", "--effort", "6"})
				f.rest = r.args(tt.want)
				f, _ = modelFlags(tt.want, f)
				want := tt.want + "/test\n" + strings.Join(f.rest, "\n") + "\n"
				if !strings.Contains(string(out), want) {
					t.Fatalf("output %s lacks %q", out, want)
				}
			}
			leases, err := st.ListLeases()
			if err != nil || len(leases) != 0 {
				t.Fatalf("leaked worker lease: %+v %v", leases, err)
			}
		})
	}
}
