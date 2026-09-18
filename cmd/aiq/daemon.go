package main

import (
	"fmt"
	"log"
	"os"

	"github.com/orlenko/aiq/internal/binpath"
	"github.com/orlenko/aiq/internal/daemon"
	"github.com/orlenko/aiq/internal/paths"
)

func cmdDaemon(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: aiq daemon run|install|uninstall|status")
	}
	switch args[0] {
	case "run":
		a, err := openApp()
		if err != nil {
			return err
		}
		defer a.close()
		logger := log.New(os.Stderr, "", log.LstdFlags)
		a.maybeInstallStatusline()
		return daemon.New(a.cfg, a.pool, logger).Run()
	case "install":
		self, err := os.Executable()
		if err != nil {
			return err
		}
		a, err := openApp()
		if err != nil {
			return err
		}
		a.maybeInstallStatusline()
		listen := a.cfg.Daemon.Listen
		a.close()
		path, err := daemon.Install(self)
		if err != nil {
			return err
		}
		fmt.Printf("installed %s\nlog: %s\nui:  http://%s/\n", path, daemon.LogPath(), listen)
		// Takeovers start the CLI with the service's PATH; say so now if
		// that cannot find one, rather than when a successor dies.
		servicePath := daemon.ServicePATH()
		for _, provider := range []string{"claude", "codex"} {
			saved := os.Getenv("PATH")
			os.Setenv("PATH", servicePath)
			_, err := binpath.Resolve(provider, "", paths.ShimsDir(), nil)
			os.Setenv("PATH", saved)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: the daemon's PATH has no %s, so a long session cannot be moved onto it; "+
					"reinstall from a login shell that has it, or set providers.%s.binary in the config\n", provider, provider)
			}
		}
		return nil
	case "uninstall":
		path, err := daemon.Uninstall()
		if err != nil {
			return err
		}
		fmt.Printf("removed %s\n", path)
		return nil
	case "status":
		a, err := openApp()
		if err != nil {
			return err
		}
		defer a.close()
		path, installed := daemon.Installed()
		fmt.Printf("service: %s (%s)\n", map[bool]string{true: "installed", false: "not installed"}[installed], path)
		if daemon.Alive(a.cfg.Daemon.Listen) {
			fmt.Printf("daemon:  running at http://%s/\n", a.cfg.Daemon.Listen)
		} else {
			fmt.Printf("daemon:  not answering on %s\n", a.cfg.Daemon.Listen)
		}
		fmt.Printf("log:     %s\n", daemon.LogPath())
		return nil
	}
	return fmt.Errorf("unknown daemon subcommand %q", args[0])
}
