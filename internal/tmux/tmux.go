// Package tmux drives the durable terminal that long sessions live in.
package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Available reports whether tmux is on PATH.
func Available() bool {
	_, err := exec.LookPath("tmux")
	return err == nil
}

func run(args ...string) (string, error) {
	out, err := exec.Command("tmux", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("tmux %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// HasSession reports whether a session exists.
func HasSession(name string) bool {
	return exec.Command("tmux", "has-session", "-t", "="+name).Run() == nil
}

// NewSession starts a detached session whose first pane runs command in
// dir. The pane is kept after the command exits so it can be respawned.
// Returns the pane id.
func NewSession(name, dir, command string) (string, error) {
	if _, err := run("new-session", "-d", "-s", name, "-c", dir, command); err != nil {
		return "", err
	}
	// Keep the pane after the command exits (window option), so a dead
	// session can be respawned in place and the server does not vanish.
	pane, err := PaneOf(name)
	if err != nil {
		return "", err
	}
	if _, err := run("set-option", "-w", "-t", pane, "remain-on-exit", "on"); err != nil {
		return pane, err
	}
	return pane, nil
}

// PaneOf returns the id of the session's first pane.
func PaneOf(name string) (string, error) {
	out, err := run("list-panes", "-t", "="+name, "-F", "#{pane_id}")
	if err != nil {
		return "", err
	}
	if i := strings.IndexByte(out, '\n'); i >= 0 {
		out = out[:i]
	}
	return out, nil
}

// PaneAlive reports whether the pane exists and its process is running.
func PaneAlive(pane string) (exists bool, running bool) {
	out, err := run("display-message", "-p", "-t", pane, "#{pane_dead}")
	if err != nil {
		return false, false
	}
	return true, out == "0"
}

// Respawn kills whatever runs in pane and starts command there.
func Respawn(pane, dir, command string) error {
	_, err := run("respawn-pane", "-k", "-c", dir, "-t", pane, command)
	return err
}

// SendLine types a line into the pane, as a user would.
func SendLine(pane, text string) error {
	if _, err := run("send-keys", "-t", pane, "-l", text); err != nil {
		return err
	}
	// A TUI treats text that arrives in one burst as a paste; give it a
	// moment before the Enter or the line stays in the composer.
	time.Sleep(500 * time.Millisecond)
	_, err := run("send-keys", "-t", pane, "Enter")
	return err
}

// Attach attaches the calling terminal to the session, switching clients
// when already inside tmux.
func Attach(name string) error {
	var cmd *exec.Cmd
	if os.Getenv("TMUX") != "" {
		cmd = exec.Command("tmux", "switch-client", "-t", "="+name)
	} else {
		cmd = exec.Command("tmux", "attach-session", "-t", "="+name)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// Kill ends the session.
func Kill(name string) error {
	_, err := run("kill-session", "-t", "="+name)
	return err
}

// Quote renders args as a POSIX shell command line for respawn-pane.
func Quote(args []string) string {
	parts := make([]string, 0, len(args))
	for _, a := range args {
		if a == "" {
			parts = append(parts, "''")
			continue
		}
		if !strings.ContainsAny(a, " \t\n'\"\\$`!*?[](){}<>|;&#~") {
			parts = append(parts, a)
			continue
		}
		parts = append(parts, "'"+strings.ReplaceAll(a, "'", `'\''`)+"'")
	}
	return strings.Join(parts, " ")
}
