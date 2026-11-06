package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// copyTimeout is how long a clipboard tool may take.
var copyTimeout = 3 * time.Second

// copyText puts text on the clipboard and says where it went. Inside tmux it goes to a
// tmux buffer, which tmux also hands to the terminal's clipboard where the terminal
// allows that. A local pbcopy or wl-copy or xclip takes it too. Outside tmux with no
// clipboard tool, the terminal is asked directly (OSC 52). Over SSH only tmux and the
// terminal can reach the person's clipboard. Once ctx ends, the tool running is stopped and
// no other is tried.
func copyText(ctx context.Context, text string, term io.Writer) (string, error) {
	var places []string
	var firstErr error
	run := func(name string, args ...string) bool {
		if ctx.Err() != nil {
			if firstErr == nil {
				firstErr = ctx.Err()
			}
			return false
		}
		// A clipboard tool that hangs must not hold the screen.
		ctx, cancel := context.WithTimeout(ctx, copyTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.WaitDelay = copyTimeout
		cmd.Stdin = strings.NewReader(text)
		if err := cmd.Run(); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", name, err)
			}
			return false
		}
		return true
	}
	if os.Getenv("TMUX") != "" {
		// -w (tmux 3.2 and later) also sets the terminal's clipboard.
		if run("tmux", "load-buffer", "-w", "-") || run("tmux", "load-buffer", "-") {
			places = append(places, "tmux buffer")
		}
	}
	for _, tool := range [][]string{{"pbcopy"}, {"wl-copy"}, {"xclip", "-selection", "clipboard"}} {
		if os.Getenv("SSH_CONNECTION") != "" {
			break // over SSH, this machine's clipboard is not the person's
		}
		if _, err := exec.LookPath(tool[0]); err == nil && run(tool[0], tool[1:]...) {
			places = append(places, "clipboard")
			break
		}
	}
	if len(places) == 0 && os.Getenv("TMUX") == "" && term != nil && ctx.Err() == nil {
		fmt.Fprintf(term, "\x1b]52;c;%s\x07", base64.StdEncoding.EncodeToString([]byte(text)))
		places = append(places, "terminal clipboard")
	}
	if len(places) == 0 {
		if firstErr == nil {
			firstErr = fmt.Errorf("no clipboard found")
		}
		return "", firstErr
	}
	return strings.Join(places, " and "), nil
}

// quoteForReply is text as a quote block with a blank line under it, ready to answer below.
func quoteForReply(text string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight("> "+l, " ")
	}
	return strings.Join(lines, "\n") + "\n\n"
}
