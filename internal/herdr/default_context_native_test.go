//go:build darwin || linux

package herdr

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type defaultContextProof struct {
	Socket          string
	Terminal        string
	SelectorPresent bool
	SessionPresent  bool
}

// Runs only in the invented PTY child; no real environments are enumerated.
func defaultContextProbe(root string) (Core, error) {
	_, selector := os.LookupEnv("HC_SOCKET")
	_, session := os.LookupEnv("HC_SESSION")
	owner := os.Getenv("HC_TERMINAL_SOCKET")
	proof := defaultContextProof{owner, os.Getenv("HC_TERMINAL_ID"), selector, session}
	if selector || session || !filepath.IsAbs(owner) || !strings.HasPrefix(owner, root+string(os.PathSeparator)) {
		return Core{}, fmt.Errorf("default fixture did not receive an isolated owner-only context")
	}
	b, _ := json.Marshal(proof)
	if err := os.WriteFile(filepath.Join(root, "context.json"), b, 0600); err != nil {
		return Core{}, err
	}
	core, err := CurrentCore()
	if err != nil {
		return core, err
	}
	// A real nested hc viewer would block. The marker makes accidental attach
	// dispatch observable and finite while all identity queries stay real RPC.
	bin := filepath.Join(root, "probe-bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		return core, err
	}
	marker := filepath.Join(root, "nested-attach")
	script := "#!/bin/sh\nprintf '%s\\n' \"$1\" \"$2\" \"$HC_SOCKET\" \"${HC_TERMINAL_SOCKET-unset}\" \"${HC_TERMINAL_ID-unset}\" > \"$GO_HERDR_ATTACH_PROOF\"\n"
	if err := os.WriteFile(filepath.Join(bin, "hc"), []byte(script), 0700); err != nil {
		return core, err
	}
	os.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	os.Setenv("GO_HERDR_ATTACH_PROOF", marker)
	if err := core.attach(); err != nil {
		return core, err
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		return core, fmt.Errorf("same-terminal attach launched a nested viewer")
	}
	os.Setenv("HC_SOCKET", core.Socket)
	for _, bad := range []string{"", "relative.sock", filepath.Join(root, "missing-owner")} {
		os.Setenv("HC_TERMINAL_SOCKET", bad)
		if err := core.attach(); err == nil {
			return core, fmt.Errorf("invalid owner allowed same-ID attach")
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			return core, fmt.Errorf("invalid owner dispatched external attach")
		}
	}
	os.Setenv("HC_TERMINAL_SOCKET", owner)
	os.Unsetenv("HC_SOCKET")
	var foreign Core
	if err := json.Unmarshal([]byte(os.Getenv("GO_HERDR_FOREIGN_TARGET")), &foreign); err != nil {
		return core, err
	}
	if foreign.Terminal != core.Terminal {
		return core, fmt.Errorf("cross-core fixture needs equal local IDs")
	}
	if err := foreign.attach(); err != nil {
		return core, err
	}
	b, err = os.ReadFile(marker)
	if err != nil || strings.TrimSpace(string(b)) != "attach\n"+foreign.Terminal+"\n"+foreign.Socket+"\nunset\nunset" {
		return core, fmt.Errorf("cross-core attach did not pin target and clear caller context")
	}
	os.Remove(marker)
	// Explicit-selector compatibility is retained only when owner is absent.
	os.Unsetenv("HC_TERMINAL_SOCKET")
	os.Setenv("HC_SOCKET", owner)
	fallback, err := CurrentCore()
	if err != nil || fallback != core {
		return core, fmt.Errorf("explicit selector context failed: %v", err)
	}
	os.Setenv("HC_TERMINAL_SOCKET", owner)
	os.Setenv("HC_SOCKET", filepath.Join(root, "wrong-selector"))
	selected, err := CurrentCore()
	if err != nil || selected != core {
		return core, fmt.Errorf("client selector replaced owner: %v", err)
	}
	os.Unsetenv("HC_SOCKET")
	fmt.Println("DEFAULT OWNER AND ATTACH VERIFIED")
	return core, nil
}

func TestNativeDefaultCoreContextStartsAttachesAndStops(t *testing.T) {
	hc, err := exec.LookPath("hc")
	if err != nil {
		t.Skip("hc unavailable; default-core PTY evidence not run")
	}
	root, err := os.MkdirTemp("/tmp", "aiqh-default-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	// Short, independent roots and no session/selector before the first hc call.
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR", "AIQ_DATA_DIR", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "COPILOT_HOME", "QUOTA_HOME"} {
		dir := filepath.Join(root, strings.ToLower(key))
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(key, dir)
	}
	for _, key := range []string{"HC_SOCKET", "HC_SESSION", "HC_TERMINAL_SOCKET", "HC_TERMINAL_ID", "GO_HERDR_WRONG_TTY_SOCKET"} {
		unsetContext(t, key)
	}
	t.Setenv("GO_WANT_HERDR_FIXTURE", "1")
	t.Setenv("GO_HERDR_WORKSPACE", root)
	targetSocket, _ := addressEndpoint(t, 1, "foreign-core", "t1", "live", os.Getpid())
	foreign, _ := json.Marshal(Core{Socket: targetSocket, ID: "foreign-core", Terminal: "t1", PID: os.Getpid()})
	t.Setenv("GO_HERDR_FOREIGN_TARGET", string(foreign))
	isolatedEnv := os.Environ()
	// Register before launch; partial startup still uses only the isolated
	// default's HOME/XDG roots. Once published, pin its actual owner explicitly.
	t.Cleanup(func() {
		env := isolatedEnv
		if b, e := os.ReadFile(filepath.Join(root, "context.json")); e == nil {
			var proof defaultContextProof
			if json.Unmarshal(b, &proof) != nil || !strings.HasPrefix(proof.Socket, root+string(os.PathSeparator)) {
				t.Error("invalid default cleanup owner")
				return
			}
			env = append(env, "HC_SOCKET="+proof.Socket)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, hc, "server", "stop", "--json")
		cmd.Env = env
		cmd.WaitDelay = time.Second
		if _, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("private default-core cleanup failed: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, hc, "run", "--bg", "--json", "--no-parent", "--cwd", root, "--", os.Args[0], "-test.run=^TestHerdrFixture$", "--", "runner-default")
	cmd.Env = isolatedEnv
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("private default launch: %v %s", err, out)
	}
	var launched struct {
		Address string `json:"address"`
	}
	if json.Unmarshal(out, &launched) != nil {
		t.Fatal("invalid private default launch reply")
	}
	var proof defaultContextProof
	until := time.Now().Add(5 * time.Second)
	for {
		if b, e := os.ReadFile(filepath.Join(root, "context.json")); e == nil && json.Unmarshal(b, &proof) == nil {
			break
		}
		if time.Now().After(until) {
			t.Fatal("default child did not publish its context")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if proof.SelectorPresent || proof.SessionPresent || proof.Terminal != launched.Address {
		t.Fatalf("default context was overridden: %+v", proof)
	}
	// Parent uses the child's verified private endpoint, never another core.
	var hello struct {
		Core struct {
			ID string `json:"id"`
		} `json:"core"`
	}
	if err := exchange(proof.Socket, "hello", map[string]any{}, &hello, time.Second); err != nil {
		t.Fatal(err)
	}
	var terminal coreTerminal
	if err := exchange(proof.Socket, "terminal.get", map[string]any{"terminal": launched.Address}, &terminal, time.Second); err != nil {
		t.Fatal(err)
	}
	core := Core{Socket: proof.Socket, ID: hello.Core.ID, Terminal: launched.Address, PID: terminal.PID}
	waitText(t, core, "NATIVE READY 1")
	waitText(t, core, "DEFAULT OWNER AND ATTACH VERIFIED")
	r, owned, err := Workspace(root)
	if err != nil || !owned || r.Core != core {
		t.Fatalf("default ownership not preserved: %+v %v %v", r, owned, err)
	}
	if err := Stop(r.Target(), r.PID); err != nil {
		t.Fatal(err)
	}
	ended, err := loadKey(workspaceKey(root))
	if err != nil || ended.State != "ended" {
		t.Fatalf("default stop did not end ownership: %+v %v", ended, err)
	}
}
