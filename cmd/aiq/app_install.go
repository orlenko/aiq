package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/orlenko/aiq/internal/herdrmod"
)

func herdrAppHome() (string, error) {
	if home := os.Getenv("HERDR_APP_HOME"); home != "" {
		if !filepath.IsAbs(home) {
			return "", errors.New("HERDR_APP_HOME must be absolute")
		}
		return home, nil
	}
	if home := os.Getenv("XDG_CONFIG_HOME"); home != "" {
		if !filepath.IsAbs(home) {
			return "", errors.New("XDG_CONFIG_HOME must be absolute")
		}
		return filepath.Join(home, "herdr-app"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("cannot locate the Herdr app home")
	}
	return filepath.Join(home, ".config", "herdr-app"), nil
}

func appInstall(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: aiq __app install")
	}
	home, err := herdrAppHome()
	if err != nil {
		return err
	}
	if err := herdrmod.Install(home); err != nil {
		return err
	}
	fmt.Println("aiq native mod installed. Open it with: aiq __app open")
	return nil
}

func appOpen(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: aiq __app open")
	}
	home, err := herdrAppHome()
	if err != nil {
		return err
	}
	if info, err := os.Lstat(filepath.Join(home, "mods", "aiq", "main.js")); err != nil || !info.Mode().IsRegular() {
		return errors.New("install the native mod first: aiq __app install")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	// An already loaded mod handles this without opening a duplicate tab. No
	// terminal is created or resumed by either command.
	if exec.CommandContext(ctx, "herdr-app", "do", "aiq.activate").Run() == nil {
		return nil
	}
	if ctx.Err() != nil {
		return errors.New("Herdr app did not respond")
	}
	if exec.CommandContext(ctx, "herdr-app", "do", "tab.new", `{"mod":"aiq","name":"aiq"}`).Run() != nil {
		return errors.New("cannot open the aiq mod; start Herdr and try again")
	}
	return nil
}
