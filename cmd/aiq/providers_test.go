package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/proc"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/testenv"
)

func TestLongArgsPerProvider(t *testing.T) {
	f := runFlags{rest: []string{"--model", "x"}, resumeSession: "s1", nudge: "carry on"}
	cases := map[string][]string{
		"claude": {"--settings", `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"/bin/aiq claude-hook sessionstart","timeout":30}]}],"UserPromptSubmit":[{"hooks":[{"type":"command","command":"/bin/aiq claude-hook userpromptsubmit","timeout":30}]}],"Stop":[{"hooks":[{"type":"command","command":"/bin/aiq claude-hook stop","timeout":30}]}]}}`,
			"--model", "x", "--resume", "s1", "carry on"},
		"agy": {"--model", "x", "--conversation", "s1", "--prompt-interactive", "carry on"},
	}
	for provider, want := range cases {
		if got := mustLongArgs(t, provider, f); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %q, want %q", provider, got, want)
		}
	}
	codex := mustLongArgs(t, "codex", f)
	if n := len(codex); n < 6 || codex[n-3] != "resume" || codex[n-2] != "s1" || codex[n-1] != "carry on" {
		t.Errorf("codex: %q", codex)
	}
}

func mustLongArgs(t *testing.T, provider string, f runFlags) []string {
	t.Helper()
	args, err := longArgs(provider, "/bin/aiq", f)
	if err != nil {
		t.Fatalf("%s: %v", provider, err)
	}
	return args
}

func TestTakeoverNameIsAnArgumentNotPromptText(t *testing.T) {
	const title = "Harp 雪 'quoted'"
	f := parseRunFlags([]string{"--long", "--session-name", title, "--nudge", "carry on", "--", "--model", "x"})
	if f.sessionName != title || f.nudge != "carry on" {
		t.Fatalf("flags: %+v", f)
	}
	for _, provider := range []string{"claude", "copilot"} {
		named := f
		named.rest = namedSessionArgs(provider, f.sessionName, f.rest)
		args := mustLongArgs(t, provider, named)
		found := false
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--name" && args[i+1] == title {
				found = true
			}
		}
		if !found {
			t.Errorf("%s omitted inherited name: %q", provider, args)
		}
	}
	// Codex uses a pre-named thread and resumes it with the original nudge.
	f.resumeSession = "named-thread"
	args := mustLongArgs(t, "codex", f)
	if got := args[len(args)-3:]; !reflect.DeepEqual(got, []string{"resume", "named-thread", "carry on"}) {
		t.Fatalf("Codex resume: %q", args)
	}
}

func TestInheritedCopilotNameSurvivesTheNextHandoff(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "aiq")
	if out, err := testenv.GoCommand("build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	t.Setenv("AIQ_CONFIG", filepath.Join(root, "config.toml"))
	t.Setenv("AIQ_DATA_DIR", root)
	fake := filepath.Join(root, "fake-copilot")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Providers.Copilot.Binary = fake
	cfg.Daemon.Listen = "127.0.0.1:1"
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.AddAccount(state.Account{ID: "copilot/test", Provider: "copilot", Name: "test", Enabled: true, Native: true, Home: root}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "run", "copilot", "--long", "--account", "test", "--session-name", "Harp", "--nudge", "carry on", "--", "--yolo")
	cmd.Env = proc.SanitizeEnv(os.Environ(), "AIQ_ACCOUNT", "AIQ_MODE", "AIQ_DEPTH", "AIQ_WAIT", "TMUX_PANE", chainVar)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("launch: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "--name\nHarp\n") {
		t.Fatalf("executed args lost name: %s", out)
	}
	leases, err := st.ListLeases()
	if err != nil || len(leases) != 1 {
		t.Fatalf("leases: %+v %v", leases, err)
	}
	var args []string
	if err := json.Unmarshal([]byte(leases[0].Args), &args); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(args, []string{"--name", "Harp", "--yolo"}) {
		t.Fatalf("recorded args lost inherited name: %q", args)
	}
}

func TestAutoWorkerArgsPerProvider(t *testing.T) {
	r := autoRequest{print: true, hasPrompt: true, prompt: "do it"}
	cases := map[string][]string{
		"claude":  {"-p", "--dangerously-skip-permissions", "--", "do it"},
		"codex":   {"exec", "--yolo", "--", "do it"},
		"agy":     {"--dangerously-skip-permissions", "-p", "do it"},
		"copilot": {"--yolo", "-p", "do it"},
	}
	for provider, want := range cases {
		if got := r.args(provider); !reflect.DeepEqual(got, want) {
			t.Errorf("%s worker: got %q, want %q", provider, got, want)
		}
	}
	interactive := autoRequest{}
	for provider, want := range map[string][]string{"claude": {"--dangerously-skip-permissions"}, "codex": {"--yolo"}, "agy": {"--dangerously-skip-permissions"}, "copilot": {"--yolo"}} {
		if got := interactive.args(provider); !reflect.DeepEqual(got, want) {
			t.Errorf("%s interactive: got %q, want %q", provider, got, want)
		}
	}
	first := autoRequest{hasPrompt: true, prompt: "start"}
	for provider, want := range map[string][]string{"claude": {"--dangerously-skip-permissions", "--", "start"}, "copilot": {"--yolo", "-i", "start"}} {
		if got := first.args(provider); !reflect.DeepEqual(got, want) {
			t.Errorf("%s first prompt: got %q, want %q", provider, got, want)
		}
	}
}

func TestProviderList(t *testing.T) {
	if got := providerList(); got != "claude, codex, agy or copilot" {
		t.Errorf("providerList: %q", got)
	}
	if got := joinOr([]string{"one"}); got != "one" {
		t.Errorf("joinOr one: %q", got)
	}
}

func TestOverlayMemoryOnlyForOverlayClaude(t *testing.T) {
	const mem = "/r/projects/p/memory"
	asked := 0
	dir := func() string { asked++; return mem }
	for _, c := range []struct {
		provider string
		native   bool
	}{{"claude", true}, {"codex", false}, {"agy", false}} {
		f := runFlags{rest: []string{"-p", "hi"}}
		overlayMemory(&f, c.provider, c.native, state.ModeInteractive, dir)
		if f.memoryDir != "" || !reflect.DeepEqual(f.rest, []string{"-p", "hi"}) {
			t.Errorf("%s native=%v: got %q %q", c.provider, c.native, f.memoryDir, f.rest)
		}
	}
	if asked != 0 {
		t.Errorf("memory dir computed for %d launches that do not use it", asked)
	}
	for _, mode := range []string{state.ModeInteractive, state.ModeWorker} {
		f := runFlags{rest: []string{"-p", "hi"}}
		overlayMemory(&f, "claude", false, mode, dir)
		if f.memoryDir != mem || len(f.rest) != 4 || f.rest[0] != "--settings" || !strings.Contains(f.rest[1], `"autoMemoryDirectory":"`+mem+`"`) {
			t.Errorf("%s: got %q %q", mode, f.memoryDir, f.rest)
		}
	}
	f := runFlags{rest: []string{"-p", "hi"}}
	overlayMemory(&f, "claude", false, state.ModeLong, dir)
	if f.memoryDir != mem || !reflect.DeepEqual(f.rest, []string{"-p", "hi"}) {
		t.Errorf("long: the memory dir must ride in longArgs' settings, not a second --settings: %q %q", f.memoryDir, f.rest)
	}
	f = runFlags{rest: []string{"-p", "hi"}}
	overlayMemory(&f, "claude", false, state.ModeInteractive, func() string { return "" })
	if !reflect.DeepEqual(f.rest, []string{"-p", "hi"}) {
		t.Errorf("empty memory dir changed the args: %q", f.rest)
	}
}

// cmdRun must point an overlay launch's memory before it starts the CLI with
// an account, on every path that starts one.
func TestCmdRunSetsOverlayMemoryBeforeStartingTheCLI(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "run.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var set []token.Pos
	var starts []string
	var startAt []token.Pos
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "cmdRun" {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch c := call.Fun.(type) {
			case *ast.Ident:
				if c.Name == "overlayMemory" {
					set = append(set, call.Pos())
				}
			case *ast.SelectorExpr:
				// Passthrough and AIQ_BYPASS start the CLI with the caller's own
				// environment (os.Environ()) and no account; skip those.
				if c.Sel.Name == "execProvider" && len(call.Args) == 4 {
					if id, ok := call.Args[2].(*ast.Ident); !ok || id.Name != "env" {
						return true
					}
				}
				if c.Sel.Name == "execProvider" || c.Sel.Name == "RunWorker" {
					starts, startAt = append(starts, c.Sel.Name), append(startAt, call.Pos())
				}
			}
			return true
		})
	}
	if len(set) != 1 || len(starts) != 3 {
		t.Fatalf("overlayMemory calls %d, CLI starts %q", len(set), starts)
	}
	for i, at := range startAt {
		if at < set[0] {
			t.Errorf("%s starts the CLI before overlayMemory", starts[i])
		}
	}
}

func TestLongArgsCarryMemoryDir(t *testing.T) {
	args, err := longArgs("claude", "/bin/aiq", runFlags{memoryDir: "/r/projects/p/memory"})
	if err != nil || len(args) != 2 || !strings.Contains(args[1], `"autoMemoryDirectory":"/r/projects/p/memory"`) || !strings.Contains(args[1], `"hooks"`) {
		t.Fatalf("got %q, %v", args, err)
	}
}

// A --settings file gets a note, on an interactive launch only: a worker's
// stderr is kept for failures.
func TestOverlayMemoryNotesASettingsFileInteractively(t *testing.T) {
	note := func(mode string) string {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		old := os.Stderr
		os.Stderr = w
		f := runFlags{rest: []string{"--settings", "f.json"}}
		overlayMemory(&f, "claude", false, mode, func() string { return "/r/projects/p/memory" })
		os.Stderr = old
		w.Close()
		out, _ := io.ReadAll(r)
		return string(out)
	}
	if got := note(state.ModeInteractive); !strings.Contains(got, "autoMemoryDirectory") {
		t.Errorf("interactive: no note, got %q", got)
	}
	if got := note(state.ModeWorker); got != "" {
		t.Errorf("worker: got %q, want nothing", got)
	}
}

// A launcher's Env is what Claude Code sees, and a --settings in its Args
// would be replaced by aiq's, so either one leaves the memory folder alone.
func TestLaunchMemoryDirReadsTheLauncher(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "proj")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=/usr/bin", "CLAUDE_CONFIG_DIR=/overlay"}
	if launchMemoryDir(config.Launcher{}, false, dir, env) == "" {
		t.Fatal("no memory folder without a launcher")
	}
	if launchMemoryDir(config.Launcher{Env: map[string]string{"FOO": "1"}, Args: []string{"--verbose"}}, true, dir, env) == "" {
		t.Error("a launcher that chooses nothing stopped the memory folder")
	}
	for name, l := range map[string]config.Launcher{
		"env":            {Env: map[string]string{"CLAUDE_CODE_PROJECT_DIR_NAME": "box"}},
		"expanded env":   {Env: map[string]string{"CLAUDE_COWORK_MEMORY_PATH_OVERRIDE": "$CLAUDE_CONFIG_DIR/m"}},
		"settings arg":   {Args: []string{"--settings", `{"a":1}`}},
		"settings= arg":  {Args: []string{`--settings={"a":1}`}},
		"moves project":  {Args: []string{"--project-config-root", "/p"}},
		"moves after --": {Args: []string{"--sandbox", "--", "claude", "--deep-link-cwd-b64=L3A="}},
	} {
		args := slices.Clone(l.Args)
		if got := launchMemoryDir(l, true, dir, env); got != "" {
			t.Errorf("%s: got %q, want empty", name, got)
		}
		if !slices.Equal(l.Args, args) {
			t.Errorf("%s: the launcher's args changed to %q", name, l.Args)
		}
	}
	if got := launchMemoryDir(config.Launcher{}, false, dir, append(env, "CLAUDE_CODE_REMOTE_MEMORY_DIR=/r")); got != "" {
		t.Errorf("launch env: got %q, want empty", got)
	}
	if env[1] != "CLAUDE_CONFIG_DIR=/overlay" || len(env) != 2 {
		t.Errorf("launchMemoryDir changed its env: %q", env)
	}
}

// A flag that moves the session's project leaves the memory folder alone.
func TestOverlayMemoryLeavesAMovedProjectAlone(t *testing.T) {
	for _, args := range [][]string{
		{"--project-config-root", "/elsewhere"},
		{"--deep-link-cwd-b64=L3g"},
	} {
		f := runFlags{rest: append([]string(nil), args...)}
		overlayMemory(&f, "claude", false, state.ModeInteractive, func() string { return "/m" })
		if f.memoryDir != "" || !reflect.DeepEqual(f.rest, args) {
			t.Errorf("%q: got %q %q", args, f.memoryDir, f.rest)
		}
	}
}

// The memory folder is judged with the launcher each mode starts through.
func TestMemoryLauncherByMode(t *testing.T) {
	cfg := config.Default()
	cfg.Launchers = map[string]config.Launcher{"box": {Provider: "claude", Args: []string{"--settings", "{}"}}}
	for _, mode := range []string{state.ModeInteractive, state.ModeLong} {
		if l, ok := memoryLauncher(cfg, "box", mode); !ok || len(l.Args) != 2 {
			t.Errorf("%s: got %+v %v", mode, l, ok)
		}
	}
	if _, ok := memoryLauncher(cfg, "box", state.ModeWorker); ok {
		t.Error("worker mode got a launcher")
	}
	if _, ok := memoryLauncher(cfg, "", state.ModeInteractive); ok {
		t.Error("no launcher named, yet one was used")
	}
}

// A launcher starts the CLI with its own Env (expanded against the launch's)
// and its own Args before the CLI's.
func TestLauncherCommandAppliesTheLaunchersEnvAndArgs(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh here")
	}
	l := config.Launcher{Provider: "claude", Command: sh, Args: []string{"-x"}, Env: map[string]string{"BOX_ALLOW": "$CLAUDE_CONFIG_DIR"}}
	shimPath := "PATH=" + paths.ShimsDir() + string(os.PathListSeparator) + "/usr/bin"
	path, argv, env, err := launcherCommand("box", l, "/bin/claude", []string{"-p", "hi"}, []string{"CLAUDE_CONFIG_DIR=/overlay", "BOX_ALLOW=old", shimPath})
	if err != nil {
		t.Fatal(err)
	}
	if path != sh || !reflect.DeepEqual(argv, []string{sh, "-x", "-p", "hi"}) {
		t.Errorf("argv = %q", argv)
	}
	n := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, "BOX_ALLOW=") {
			n++
			if kv != "BOX_ALLOW=/overlay" {
				t.Errorf("BOX_ALLOW: %s", kv)
			}
		}
	}
	if !slices.Contains(env, "PATH=/usr/bin") {
		t.Errorf("the shim folder stayed on the launcher's PATH: %q", env)
	}
	if n != 1 || !slices.Contains(env, binaryVar+"=/bin/claude") || !slices.Contains(env, launcherVar+"=box") {
		t.Errorf("env = %q", env)
	}
}

// calls returns, for each call to name inside function fn of file, its
// arguments as source text.
func calls(t *testing.T, file, fn, name string) [][]string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	src, _ := os.ReadFile(file)
	var out [][]string
	for _, d := range f.Decls {
		decl, ok := d.(*ast.FuncDecl)
		if !ok || decl.Name.Name != fn {
			continue
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var got string
			switch c := call.Fun.(type) {
			case *ast.Ident:
				got = c.Name
			case *ast.SelectorExpr:
				got = c.Sel.Name
			}
			if got == name {
				var args []string
				for _, a := range call.Args {
					args = append(args, string(src[fset.Position(a.Pos()).Offset:fset.Position(a.End()).Offset]))
				}
				out = append(out, args)
			}
			return true
		})
	}
	return out
}

// cmdRun judges the memory folder with the launch's own launcher, and
// execProvider starts a launcher with the env launcherCommand built.
func TestLauncherWiring(t *testing.T) {
	if got := calls(t, "run.go", "cmdRun", "memoryLauncher"); !reflect.DeepEqual(got, [][]string{{"a.cfg", "f.launcher", "mode"}}) {
		t.Errorf("cmdRun's memoryLauncher calls: %q", got)
	}
	if got := calls(t, "run.go", "cmdRun", "launchMemoryDir"); !reflect.DeepEqual(got, [][]string{{"l", "hasLauncher", "claude.WorkingDir()", "env"}}) {
		t.Errorf("cmdRun's launchMemoryDir calls: %q", got)
	}
	if got := calls(t, "launch.go", "execProvider", "launcherCommand"); len(got) != 1 {
		t.Fatalf("execProvider's launcherCommand calls: %q", got)
	}
	f, err := parser.ParseFile(token.NewFileSet(), "launch.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ok := false
	for _, d := range f.Decls {
		decl, isFn := d.(*ast.FuncDecl)
		if !isFn || decl.Name.Name != "execProvider" {
			continue
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			as, isAssign := n.(*ast.AssignStmt)
			if !isAssign || len(as.Rhs) != 1 || len(as.Lhs) != 4 {
				return true
			}
			if call, isCall := as.Rhs[0].(*ast.CallExpr); isCall {
				if id, isID := call.Fun.(*ast.Ident); isID && id.Name == "launcherCommand" {
					env, isEnv := as.Lhs[2].(*ast.Ident)
					ok = isEnv && env.Name == "env"
				}
			}
			return true
		})
	}
	if !ok {
		t.Error("execProvider drops the env launcherCommand returns")
	}
	if got := calls(t, "launch.go", "execProvider", "Exec"); len(got) != 2 || got[1][2] != "env" {
		t.Errorf("execProvider's Exec calls: %q", got)
	}
}
