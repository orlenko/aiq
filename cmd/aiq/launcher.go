package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/paths"
)

// aiq launcher add <name> --provider <p> [flags] -- <command> [args...]
// aiq launcher list
// aiq launcher remove <name>
func cmdLauncher(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: aiq launcher add|list|remove ...")
	}
	switch args[0] {
	case "list":
		return launcherList()
	case "add":
		return launcherAdd(args[1:])
	case "remove", "rm":
		if len(args) < 2 {
			return fmt.Errorf("usage: aiq launcher remove <name>")
		}
		return launcherRemove(args[1])
	}
	return fmt.Errorf("unknown launcher subcommand %q", args[0])
}

func launcherList() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if len(cfg.Launchers) == 0 {
		fmt.Println("no launchers — register one with: aiq launcher add <name> --provider claude -- <command>")
		return nil
	}
	names := make([]string, 0, len(cfg.Launchers))
	for n := range cfg.Launchers {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Printf("%-20s %-8s %-10s %s\n", "LAUNCHER", "PROVIDER", "CREDENTIAL", "COMMAND")
	for _, n := range names {
		l := cfg.Launchers[n]
		cred := l.Credential
		if cred == "" {
			cred = "-"
		}
		cmd := l.Command
		if len(l.Args) > 0 {
			cmd += " " + strings.Join(l.Args, " ")
		}
		fmt.Printf("%-20s %-8s %-10s %s\n", n, l.Provider, cred, cmd)
	}
	return nil
}

func launcherAdd(args []string) error {
	if len(args) == 0 {
		return launcherAddUsage()
	}
	name := args[0]
	if err := config.ValidLauncherName(name); err != nil {
		return err
	}
	l := config.Launcher{Env: map[string]string{}}
	i := 1
	for ; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			i++
			goto command
		case a == "--provider" && i+1 < len(args):
			l.Provider = args[i+1]
			i++
		case strings.HasPrefix(a, "--provider="):
			l.Provider = strings.TrimPrefix(a, "--provider=")
		case a == "--credential" && i+1 < len(args):
			l.Credential = args[i+1]
			i++
		case strings.HasPrefix(a, "--credential="):
			l.Credential = strings.TrimPrefix(a, "--credential=")
		case a == "--env" && i+1 < len(args):
			k, v, ok := strings.Cut(args[i+1], "=")
			if !ok {
				return fmt.Errorf("--env wants KEY=VALUE, got %q", args[i+1])
			}
			l.Env[k] = v
			i++
		case strings.HasPrefix(a, "--env="):
			k, v, ok := strings.Cut(strings.TrimPrefix(a, "--env="), "=")
			if !ok {
				return fmt.Errorf("--env wants KEY=VALUE, got %q", a)
			}
			l.Env[k] = v
		case a == "--fallback" && i+1 < len(args):
			l.Fallback = strings.Split(args[i+1], ",")
			i++
		case strings.HasPrefix(a, "--fallback="):
			l.Fallback = strings.Split(strings.TrimPrefix(a, "--fallback="), ",")
		default:
			return fmt.Errorf("unknown flag %q\n\n%s", a, launcherAddUsageText())
		}
	}
command:
	if i < len(args) {
		l.Command = args[i]
		l.Args = args[i+1:]
	}
	if l.Provider != "claude" && l.Provider != "codex" {
		return fmt.Errorf("--provider must be claude or codex")
	}
	if l.Command == "" {
		return fmt.Errorf("no command; put it after `--`\n\n%s", launcherAddUsageText())
	}
	if l.Credential != "" && l.Credential != "file" {
		return fmt.Errorf(`--credential must be "file" (or left unset)`)
	}
	if len(l.Env) == 0 {
		l.Env = nil
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.Launchers == nil {
		cfg.Launchers = map[string]config.Launcher{}
	}
	_, existed := cfg.Launchers[name]
	cfg.Launchers[name] = l
	if err := config.Save(cfg); err != nil {
		return err
	}
	path, err := writeLauncherShim(name, l.Provider)
	if err != nil {
		return err
	}
	verb := "registered"
	if existed {
		verb = "updated"
	}
	fmt.Printf("%s launcher %q → %s\n", verb, name, l.Command)
	fmt.Printf("shim: %s\n\n", path)
	fmt.Printf("Start it by name:\n  %s [args...]        (or: aiq %s [args...])\n  aiq long %s [args...]\n", name, name, name)
	if l.Provider == "claude" && l.Credential != "file" {
		fmt.Fprintf(os.Stderr, "\nnote: a launcher that keeps the CLI from reaching the system keychain\n"+
			"      needs --credential file, or Claude Code will ask you to log in.\n")
	}
	return nil
}

func launcherRemove(name string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if _, ok := cfg.Launchers[name]; !ok {
		return fmt.Errorf("no launcher %q", name)
	}
	delete(cfg.Launchers, name)
	if err := config.Save(cfg); err != nil {
		return err
	}
	shim := paths.ShimPath(name)
	os.Remove(shim)
	fmt.Printf("removed launcher %q and its shim %s\n", name, shim)
	return nil
}

func launcherAddUsage() error { return fmt.Errorf("%s", launcherAddUsageText()) }

func launcherAddUsageText() string {
	return `usage: aiq launcher add <name> --provider claude|codex [flags] -- <command> [args...]

  --credential file     write the account's credential into the overlay home
                        first, for a launcher that cuts the CLI off from the
                        system keychain
  --env KEY=VALUE       add to the launch (repeatable). $CLAUDE_CONFIG_DIR and
                        $CODEX_HOME expand to the chosen account's overlay home
  --fallback a,b        launchers a long session may move to, in order

example:
  aiq launcher add boxed --provider claude --credential file \
    --env BOX_ALLOW='$CLAUDE_CONFIG_DIR' -- /usr/local/bin/boxed --profile strict`
}
