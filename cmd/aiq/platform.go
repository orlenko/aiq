package main

import (
	"errors"
	"fmt"
	"runtime"
	"slices"
)

// goos is the system aiq runs on. Tests set it to check the Windows gate from any system.
var goos = runtime.GOOS

// windowsReadOnly is what aiq offers on Windows for now: commands that read and print. They
// start no provider, take no raw screen, and write nothing outside aiq's own data and config
// folders (opening the database may create it there). Each entry is a command and, where
// only some of its forms qualify, the subcommand.
//
//   - version, help: print only.
//   - status: reads the database; --refresh polls providers, so it is refused.
//   - top: status, redrawn; it clears the screen with an escape but takes no raw mode.
//   - account list, launcher list, asks list: read the database or the config.
//   - convo: prints a transcript; without /dev/tty it never takes the screen on Windows.
//   - resume --print: prints sessions; resuming starts a provider.
//
// A command listed without subcommands runs on Windows in every form the gate does not
// refuse by name, so a new flag or subcommand on one of these runs there too: give it a
// case in windowsGate unless it only reads and prints.
var windowsReadOnly = map[string][]string{
	"version": nil, "--version": nil, "-v": nil,
	"help": nil, "--help": nil, "-h": nil,
	"status":   nil,
	"top":      nil,
	"account":  {"list"},
	"launcher": {"list"},
	"asks":     {"list"},
	"convo":    nil,
	"resume":   nil,
}

// windowsQuiet are entry points a provider calls on its own: hooks and status lines. aiq
// installs none of them on Windows yet, and they promise to exit 0, so there they do
// nothing and exit 0.
var windowsQuiet = []string{"claude-hook", "codex-hook", "agy-hook", "claude-statusline", "agy-statusline"}

// windowsGate decides, before anything else runs, whether args may run on this system. On
// Windows, a provider entry point is quiet (it does nothing and exits 0), a read-only
// command runs, and anything else is refused. Elsewhere everything runs.
func windowsGate(args []string) (quiet bool, err error) {
	if goos != "windows" {
		return false, nil
	}
	if len(args) == 0 {
		return false, nil // bare aiq prints the command list on Windows; see main
	}
	cmd, rest := args[0], args[1:]
	if cmd == "ui" {
		o, err := parseUIArgs(rest)
		if err != nil {
			return false, err
		}
		if o.mode == uiHelp {
			return false, nil
		}
		return false, errUIPlatform
	}
	if slices.Contains(windowsQuiet, cmd) {
		return true, nil
	}
	subs, ok := windowsReadOnly[cmd]
	switch {
	case !ok:
	case cmd == "status" && slices.Contains(rest, "--refresh"):
	case cmd == "resume" && !resumePrintsOnly(rest):
	case subs == nil:
		return false, nil
	case len(rest) > 0 && slices.Contains(subs, rest[0]):
		return false, nil
	}
	return false, errWindowsCommand(args)
}

func errWindowsCommand(args []string) error {
	what := args[0]
	if len(args) > 1 {
		what += " " + args[1]
	}
	return fmt.Errorf("aiq %s is not supported on Windows yet. On Windows aiq offers only: version, help, status (without --refresh), top, account list, launcher list, asks list, convo, and resume --print", what)
}

// resumePrintsOnly says resume's own parsing reads args as --print: a -p after --, or as a
// launcher's name, is not one.
func resumePrintsOnly(args []string) bool {
	o, err := parseResumeArgs(args)
	return err == nil && o.print
}

// errWindowsScreen is the backstop for the full screens, which the gate refuses on Windows.
var errWindowsScreen = errors.New("full-screen views are not supported on Windows yet")
