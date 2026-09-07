package main

import (
	"fmt"
	"log"
	"os"

	"github.com/orlenko/aiq/internal/daemon"
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
