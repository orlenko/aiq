// Package proc runs provider CLIs as child processes with the terminal
// inherited, and answers pid-liveness questions for lease bookkeeping.
package proc

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// RunInteractive runs cmd with the caller's terminal. SIGINT/SIGQUIT are
// ignored in the parent (the terminal delivers them to the child directly);
// SIGTERM and SIGHUP are forwarded. Returns the child's exit code.
func RunInteractive(cmd *exec.Cmd) (int, error) {
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return Wait(cmd)
}

// Wait starts cmd and waits for it, forwarding SIGTERM/SIGHUP and leaving
// SIGINT/SIGQUIT to the terminal. Returns the child's exit code.
func Wait(cmd *exec.Cmd) (int, error) {
	signal.Ignore(os.Interrupt, syscall.SIGQUIT)
	defer signal.Reset(os.Interrupt, syscall.SIGQUIT)

	fwd := make(chan os.Signal, 4)
	signal.Notify(fwd, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(fwd)

	if err := cmd.Start(); err != nil {
		return -1, err
	}
	done := make(chan struct{})
	go func() {
		for {
			select {
			case sig := <-fwd:
				cmd.Process.Signal(sig)
			case <-done:
				return
			}
		}
	}()
	err := cmd.Wait()
	close(done)
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return -1, err
}

// Alive reports whether pid refers to a live process on this host.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// SanitizeEnv returns environ without the named variables.
func SanitizeEnv(environ []string, drop ...string) []string {
	blocked := map[string]bool{}
	for _, d := range drop {
		blocked[d] = true
	}
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name := kv
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				name = kv[:i]
				break
			}
		}
		if !blocked[name] {
			out = append(out, kv)
		}
	}
	return out
}
