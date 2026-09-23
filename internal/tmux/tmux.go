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
	// Keep the pane after the command exits (window option), so a dead
	// session can be respawned in place and the server does not vanish.
	// The option is set in the same tmux invocation: the server runs the
	// command list before it reaps the child, so a command that fails at
	// once still leaves a dead pane with its error on screen. A separate
	// set-option call would find the pane already gone.
	if _, err := run("new-session", "-d", "-s", name, "-c", dir, command,
		";", "set-option", "-w", "remain-on-exit", "on"); err != nil {
		return "", err
	}
	return PaneOf(name)
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
// A tmux query that fails reads as "gone": the caller cannot tell that case
// apart, so use PaneState where the difference matters.
func PaneAlive(pane string) (exists bool, running bool) {
	exists, running, _ = PaneState(pane)
	return exists, running
}

// PaneState is PaneAlive with the query error kept. A failed query returns
// (false, false, err), which is not the same as a pane that really died: a
// caller that respawns on !running would be killing a healthy session.
func PaneState(pane string) (exists bool, running bool, err error) {
	out, err := run("display-message", "-p", "-t", pane, "#{pane_dead}")
	if err != nil {
		return false, false, err
	}
	return true, out == "0", nil
}

// PaneDiag describes a pane for a log line: whether it is dead, the exit
// status of the command that died, its pid and the command it last ran.
func PaneDiag(pane string) string {
	out, err := run("display-message", "-p", "-t", pane,
		"dead=#{pane_dead} status=#{pane_dead_status} pid=#{pane_pid} cmd=#{pane_current_command} panes_in_window=#{window_panes}")
	if err != nil {
		return "pane query failed: " + err.Error()
	}
	return out
}

// Tail returns the last n non-empty lines of the pane's visible text, joined
// with " / " so the whole thing fits on one log line.
func Tail(pane string, n int) string {
	screen, err := Capture(pane)
	if err != nil {
		return "(capture failed: " + err.Error() + ")"
	}
	var lines []string
	for _, l := range strings.Split(screen, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		return "(pane is blank)"
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " / ")
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

// Capture returns the visible text of the pane.
func Capture(pane string) (string, error) {
	return run("capture-pane", "-p", "-t", pane)
}

// SendKeys presses keys in the pane (tmux key names, not literal text).
func SendKeys(pane string, keys ...string) error {
	_, err := run(append([]string{"send-keys", "-t", pane}, keys...)...)
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

// PaneCount returns how many panes the session holds. A long session aiq
// started has one; more means the user split the window and put something of
// their own beside it.
func PaneCount(name string) (int, error) {
	out, err := run("list-panes", "-t", "="+name, "-F", "#{pane_id}")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}
	return n, nil
}
