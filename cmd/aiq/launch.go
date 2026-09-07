package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/orlenko/aiq/internal/binpath"
	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/proc"
)

// The exec chain.
//
// Every time aiq execs a provider binary it records
// AIQ_CHAIN=<provider>\x1f<pid>\x1f<path>[\x1f<path>...] in the child's
// environment. A wrapper script on PATH (an IDE shim, a personal alias
// script) that runs the bare command name lands back in aiq's shim, hence in
// `aiq run`, either with the same pid (the wrapper exec'd) or as a direct
// child of a shell whose pid is the chain pid (the wrapper forked). A real
// CLI has to fork through its own process to run anything, so neither shape
// matches a legitimate nested call. A matching re-entry is a wrapper hop:
// resolve the next candidate on PATH that is not already in the chain and
// exec it with the environment untouched. No lease, no selection, no loop.
const chainVar = "AIQ_CHAIN"

const chainSep = "\x1f"

type chain struct {
	provider string
	pid      int
	paths    []string
}

func parseChain(v string) (chain, bool) {
	parts := strings.Split(v, chainSep)
	if len(parts) < 3 {
		return chain{}, false
	}
	pid, err := strconv.Atoi(parts[1])
	if err != nil {
		return chain{}, false
	}
	var ps []string
	for _, p := range parts[2:] {
		if p != "" {
			ps = append(ps, p)
		}
	}
	return chain{provider: parts[0], pid: pid, paths: ps}, true
}

func (c chain) String() string {
	return strings.Join(append([]string{c.provider, strconv.Itoa(c.pid)}, c.paths...), chainSep)
}

// inheritedChain returns the chain this process is continuing, if any.
func inheritedChain(provider string) (chain, bool) {
	c, ok := parseChain(os.Getenv(chainVar))
	if !ok || c.provider != provider {
		return chain{}, false
	}
	if c.pid == os.Getpid() {
		return c, true
	}
	if c.pid == os.Getppid() && parentIsShell() {
		return c, true
	}
	return chain{}, false
}

var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true}

// parentIsShell reports whether our parent process is a shell: the shape of
// a wrapper script that forgot to exec.
func parentIsShell() bool {
	ppid := os.Getppid()
	var name string
	if runtime.GOOS == "linux" {
		exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", ppid))
		if err != nil {
			return false
		}
		name = filepath.Base(exe)
	} else {
		out, err := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(ppid)).Output()
		if err != nil {
			return false
		}
		name = filepath.Base(strings.TrimSpace(string(out)))
	}
	name = strings.TrimPrefix(name, "-")
	return shells[name]
}

// resolveNext picks the provider binary to exec, honouring the chain.
func (a *app) resolveNext(provider string, c chain) (string, error) {
	configured := ""
	switch provider {
	case "claude":
		configured = a.cfg.Providers.Claude.Binary
	case "codex":
		configured = a.cfg.Providers.Codex.Binary
	}
	bin, err := binpath.Resolve(provider, configured, paths.ShimsDir(), c.paths)
	if err != nil && len(c.paths) > 0 {
		return "", fmt.Errorf("%w; the last wrapper tried was %s", err, c.paths[len(c.paths)-1])
	}
	return bin, err
}

// execProvider replaces this process with the provider binary, extending
// the chain. env must already be the environment the CLI should see.
func (a *app) execProvider(provider string, args []string, env []string) error {
	c, ok := inheritedChain(provider)
	if !ok {
		c = chain{provider: provider, pid: os.Getpid()}
	}
	c.pid = os.Getpid()
	bin, err := a.resolveNext(provider, c)
	if err != nil {
		return err
	}
	c.paths = append(c.paths, bin)
	env = append(proc.SanitizeEnv(env, chainVar), chainVar+"="+c.String())
	argv := append([]string{bin}, args...)
	return syscall.Exec(bin, argv, env)
}

// providerCommand builds a child process that runs the provider binary via
// the `aiq __launch` trampoline, so the chain logic applies to children too
// (workers, probes, logins) and not only to exec'd interactive sessions.
func providerCommand(provider string, args []string, env []string) *exec.Cmd {
	self, _ := os.Executable()
	cmd := exec.Command(self, append([]string{"__launch", provider, "--"}, args...)...)
	cmd.Env = proc.SanitizeEnv(env, chainVar)
	return cmd
}

// cmdLaunch is the trampoline: `aiq __launch <provider> -- args...`.
func cmdLaunch(args []string) error {
	if len(args) < 2 || args[1] != "--" {
		return fmt.Errorf("usage: aiq __launch <provider> -- args...")
	}
	a, err := openApp()
	if err != nil {
		return err
	}
	a.close()
	return a.execProvider(args[0], args[2:], os.Environ())
}
