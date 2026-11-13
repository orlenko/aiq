package tmux

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var (
	ErrAttachUnavailable = errors.New("recorded tmux terminal unavailable")
	ErrAttachChanged     = errors.New("recorded tmux terminal changed")
)

const attachMetadataLimit = 64 << 10
const attachFormat = "#{pane_id}|#{pane_pid}|#{pane_dead}|#{session_id}|#{window_id}|#{socket_path}"

type attachInfo struct {
	pane, session, window, socket string
	pid                           int
}

func attachToken(s string, prefix byte) bool {
	if len(s) < 2 || len(s) > 21 || s[0] != prefix {
		return false
	}
	n, err := strconv.ParseUint(s[1:], 10, 64)
	return err == nil && strconv.FormatUint(n, 10) == s[1:]
}

func attachPositivePID(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	return n, err == nil && n > 0 && strconv.Itoa(n) == s
}

func attachSocket(s string) bool {
	return len(s) > 0 && len(s) <= 4096 && strings.IndexByte(s, 0) < 0 && filepath.IsAbs(s)
}

// TMUX's socket is followed by two numeric fields; the socket itself may
// contain commas. Never fall back to a default server on malformed context.
func attachContext(raw string) (string, error) {
	last := strings.LastIndexByte(raw, ',')
	if last < 0 {
		return "", ErrAttachUnavailable
	}
	previous := strings.LastIndexByte(raw[:last], ',')
	if previous < 0 || !attachSocket(raw[:previous]) {
		return "", ErrAttachUnavailable
	}
	if _, ok := attachPositivePID(raw[previous+1 : last]); !ok || !attachToken("$"+raw[last+1:], '$') {
		return "", ErrAttachUnavailable
	}
	return raw[:previous], nil
}

func parseAttachInfo(data []byte, pane string, pid int) (attachInfo, error) {
	if len(data) > attachMetadataLimit || len(data) == 0 || data[len(data)-1] != '\n' {
		return attachInfo{}, ErrAttachUnavailable
	}
	// Only the final protocol newline is removed. Socket pathname bytes may
	// contain separators or whitespace and are not trimmed or interpreted.
	fields := strings.SplitN(string(data[:len(data)-1]), "|", 6)
	if len(fields) != 6 || !attachToken(fields[0], '%') || !attachToken(fields[3], '$') || !attachToken(fields[4], '@') || !attachSocket(fields[5]) {
		return attachInfo{}, ErrAttachUnavailable
	}
	actualPID, ok := attachPositivePID(fields[1])
	if !ok || fields[2] != "0" || fields[0] != pane || actualPID != pid {
		return attachInfo{}, ErrAttachChanged
	}
	return attachInfo{pane: fields[0], pid: actualPID, session: fields[3], window: fields[4], socket: fields[5]}, nil
}

func attachEnv(env []string, key string) string {
	value := ""
	for _, item := range env {
		k, v, found := strings.Cut(item, "=")
		if found && k == key {
			value = v
		}
	}
	return value
}

type attachCapture struct{ bytes []byte }

func (b *attachCapture) Write(p []byte) (int, error) {
	remaining := attachMetadataLimit - len(b.bytes)
	if len(p) > remaining {
		b.bytes = append(b.bytes, p[:remaining]...)
		return remaining, ErrAttachUnavailable
	}
	b.bytes = append(b.bytes, p...)
	return len(p), nil
}

// PrepareAttach pins the executable, server context and numeric pane identity.
// It does not search other servers when the current/default context disagrees.
func PrepareAttach(pane string, pid int) (func() error, error) {
	if !attachToken(pane, '%') || pid <= 0 {
		return nil, ErrAttachChanged
	}
	binary, err := exec.LookPath("tmux")
	if err != nil || !filepath.IsAbs(binary) {
		return nil, ErrAttachUnavailable
	}
	return prepareAttach(pane, pid, os.Environ(), binary, 4*time.Second)
}

func prepareAttach(pane string, pid int, environment []string, binary string, timeout time.Duration) (func() error, error) {
	if !attachToken(pane, '%') || pid <= 0 || !filepath.IsAbs(binary) || timeout < 4*time.Nanosecond {
		return nil, ErrAttachChanged
	}
	env := append([]string(nil), environment...)
	inside := attachEnv(env, "TMUX") != ""
	args := []string{"-N"}
	if inside {
		socket, err := attachContext(attachEnv(env, "TMUX"))
		if err != nil {
			return nil, err
		}
		args = append(args, "-S", socket)
	}
	args = append(args, "display-message", "-p", "-t", pane, attachFormat)
	// Reserve part of the nominal control budget for pipes retained after the
	// command exits. Cancellation alone cannot bound os/exec's EOF wait.
	drain := min(100*time.Millisecond, timeout/4)
	ctx, cancel := context.WithTimeout(context.Background(), timeout-drain)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.WaitDelay = drain
	cmd.Env, cmd.Stdin, cmd.Stderr = env, nil, io.Discard
	capture := &attachCapture{bytes: make([]byte, 0, attachMetadataLimit)}
	cmd.Stdout = capture
	if err := cmd.Run(); err != nil {
		return nil, ErrAttachUnavailable
	}
	target, err := parseAttachInfo(capture.bytes, pane, pid)
	if err != nil {
		return nil, err
	}
	operation := []string{"-N", "-S", target.socket}
	if inside {
		operation = append(operation, "switch-client", "-t", target.pane)
	} else {
		operation = append(operation, "select-window", "-t", target.pane, ";", "select-pane", "-t", target.pane, ";", "attach-session", "-t", target.session)
	}
	return func() error {
		// This is an interactive viewer, with no control-query timeout. A
		// vanished server cannot be recreated, and stderr is not raw AIQ text.
		cmd := exec.Command(binary, operation...)
		cmd.Env = append([]string(nil), env...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, io.Discard
		if err := cmd.Run(); err != nil {
			return ErrAttachUnavailable
		}
		return nil
	}, nil
}
