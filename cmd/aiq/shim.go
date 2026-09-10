package main

import (
	"fmt"
	"github.com/orlenko/aiq/internal/config"
	"os"
	"path/filepath"

	"github.com/orlenko/aiq/internal/paths"
)

// shimScript is what `claude` and `codex` become on PATH. AIQ_SHIM marks it
// so binpath never mistakes a copy for the real CLI.
const shimScript = `#!/bin/sh
# AIQ_SHIM: routes %[1]s to the pooled account with the most perishable quota.
exec %[2]q run %[1]s -- "$@"
`

// launcherShimScript routes a registered launcher's name. The launcher is
// named explicitly, so nothing is wrapped unless this name was typed.
const launcherShimScript = `#!/bin/sh
# AIQ_SHIM: routes %[1]s to the pooled account with the most perishable quota,
# then starts it through the %[1]s launcher.
exec %[2]q run %[3]s --launcher %[1]s -- "$@"
`

// writeLauncherShim writes the shim for a launcher name and returns its path.
func writeLauncherShim(name, provider string) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	self, _ = filepath.EvalSymlinks(self)
	dir := paths.ShimsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := paths.ShimPath(name)
	body := fmt.Sprintf(launcherShimScript, name, self, provider)
	return path, os.WriteFile(path, []byte(body), 0o755)
}

func cmdShim(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: aiq shim install|uninstall|path")
	}
	dir := paths.ShimsDir()
	switch args[0] {
	case "path":
		fmt.Println(dir)
		return nil
	case "install":
		self, err := os.Executable()
		if err != nil {
			return err
		}
		self, _ = filepath.EvalSymlinks(self)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		for _, name := range []string{"claude", "codex"} {
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, []byte(fmt.Sprintf(shimScript, name, self)), 0o755); err != nil {
				return err
			}
		}
		// Registered launchers get their shims back too, so `shim install`
		// after an aiq upgrade regenerates every name.
		if cfg, err := config.Load(); err == nil {
			for lname, l := range cfg.Launchers {
				if _, err := writeLauncherShim(lname, l.Provider); err != nil {
					return err
				}
			}
		}
		fmt.Printf("shims written to %s\n\n", dir)
		fmt.Printf("Put them first on PATH (add to ~/.zshrc / ~/.bashrc):\n\n  export PATH=%q:$PATH\n\n", dir)
		fmt.Println("Every shell and every agent-spawned subprocess then routes claude/codex through aiq.")
		return nil
	case "uninstall":
		for _, name := range []string{"claude", "codex"} {
			os.Remove(filepath.Join(dir, name))
		}
		fmt.Printf("shims removed from %s (drop the PATH line from your shell rc)\n", dir)
		return nil
	}
	return fmt.Errorf("unknown shim subcommand %q", args[0])
}
