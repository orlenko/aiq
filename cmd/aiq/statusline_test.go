package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/config"
)

func TestRunChainedStatuslinePassesInputAndMarksChain(t *testing.T) {
	t.Setenv(statuslineChainEnv, "")
	var out bytes.Buffer
	prev := `printf '%s ' "$(cat)"; printf '%s' "$` + statuslineChainEnv + `"`
	if !runChainedStatusline(prev, []byte("payload"), &out) {
		t.Fatal("chained statusline did not render")
	}
	if got, want := out.String(), "payload 1"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

// A chained script that calls aiq back must not run itself again.
func TestRunChainedStatuslineDeclinesInsideChain(t *testing.T) {
	t.Setenv(statuslineChainEnv, "1")
	var out bytes.Buffer
	if runChainedStatusline("printf looped", nil, &out) {
		t.Fatal("ran the chained statusline from inside a chain")
	}
	if out.Len() != 0 {
		t.Errorf("output = %q, want none", out.String())
	}
}

func TestRunChainedStatuslineFallsBack(t *testing.T) {
	t.Setenv(statuslineChainEnv, "")
	var out bytes.Buffer
	if runChainedStatusline("", nil, &out) {
		t.Error("empty command reported a render")
	}
	if runChainedStatusline("exit 3", nil, &out) {
		t.Error("failing command reported a render")
	}
}

func TestStatuslineChainedScriptCallsAiqBack(t *testing.T) {
	// The user's statusline renders its own segment, then runs
	// `aiq claude-statusline` to append aiq's quota. Chained from the
	// multiplexer, that inner call must print aiq's line instead of running
	// the script again. The script counts its own depth so a regression
	// fails here instead of recursing without limit.
	dir := t.TempDir()
	bin := filepath.Join(dir, "aiq")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	script := filepath.Join(dir, "statusline-command.sh")
	body := `input=$(cat)
depth=$((${AIQ_TEST_DEPTH:-0} + 1))
[ "$depth" -gt 3 ] && exit 1
export AIQ_TEST_DEPTH=$depth
printf '~/w Opus | '
printf '%s' "$input" | "$AIQ_TEST_BIN" "$AIQ_TEST_SUBCOMMAND"
`
	if err := os.WriteFile(script, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	const claudePayload = `{"model":{"display_name":"Opus"},"rate_limits":{"five_hour":{"used_percentage":42},"seven_day":{"used_percentage":7}}}`

	for _, tt := range []struct {
		name, subcommand, prev, payload, want string
	}{
		{"claude chained once", "claude-statusline", "sh " + script, claudePayload, "~/w Opus | claude/claude1 · 5h 42% · wk 7%\n"},
		{"agy chained once", "agy-statusline", "sh " + script, `{}`, "~/w Opus | aiq agy/claude1\n"},
		{"claude without a previous statusline", "claude-statusline", "", claudePayload, "claude/claude1 · 5h 42% · wk 7%\n"},
		{"claude previous statusline fails", "claude-statusline", "exit 3", claudePayload, "claude/claude1 · 5h 42% · wk 7%\n"},
		{"agy previous statusline fails", "agy-statusline", "exit 3", `{}`, "aiq agy/claude1\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			cfg := config.Default()
			cfg.Telemetry.ClaudeStatuslinePrevious = tt.prev
			cfg.Telemetry.AgyStatuslinePrevious = tt.prev
			t.Setenv("AIQ_CONFIG", filepath.Join(root, "config.toml"))
			if err := config.Save(cfg); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(bin, tt.subcommand)
			cmd.Env = append(os.Environ(),
				"AIQ_DATA_DIR="+root,
				"AIQ_ACCOUNT=claude1",
				"AIQ_PROVIDER=",
				"AIQ_TEST_BIN="+bin,
				"AIQ_TEST_SUBCOMMAND="+tt.subcommand,
				"AIQ_TEST_DEPTH=",
				statuslineChainEnv+"=",
			)
			cmd.Stdin = strings.NewReader(tt.payload)
			cmd.WaitDelay = 5 * time.Second
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("%s: %v", tt.subcommand, err)
			}
			if string(out) != tt.want {
				t.Fatalf("%s printed %q, want %q", tt.subcommand, out, tt.want)
			}
		})
	}
}
