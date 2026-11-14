package terminal

import (
	"context"
	"errors"
	"io"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/orlenko/aiq/internal/herdr"
)

// Observation is a positive read-only observation, never mutation authority.
// A missing endpoint, moved generation or failed query is Unknown, not Ended.
type Observation uint8

const (
	Unknown Observation = iota
	Running
	Ended
)

type ObservationTarget struct {
	Ref, Workspace    string
	PID               int
	LeaseID, LaunchID int64
}
type observationDeps struct {
	tmux  func(context.Context, string) (string, error)
	herdr func(string, time.Duration) (herdr.Record, error)
}

const observationTimeout = 250 * time.Millisecond

func Observe(ctx context.Context, target ObservationTarget) Observation {
	return observeWith(ctx, target, observationDeps{tmux: observeTmux, herdr: herdr.InfoWithin})
}
func observeWith(parent context.Context, t ObservationTarget, d observationDeps) Observation {
	if parent == nil || parent.Err() != nil || t.PID <= 0 || t.PID > math.MaxInt32 {
		return Unknown
	}
	ctx, cancel := context.WithTimeout(parent, observationTimeout)
	defer cancel()
	if herdr.IsRef(t.Ref) {
		family, generation, err := herdr.ParseRef(t.Ref)
		if err != nil || generation == 0 || t.LeaseID <= 0 || t.LaunchID <= 0 || t.Workspace == "" || len(t.Workspace) > 4096 || !utf8.ValidString(t.Workspace) || strings.ContainsRune(t.Workspace, 0) || d.herdr == nil {
			return Unknown
		}
		deadline, _ := ctx.Deadline()
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return Unknown
		}
		r, err := d.herdr(t.Ref, remaining)
		if err != nil || ctx.Err() != nil || r.Ref != family || r.Generation != generation || r.Target() != t.Ref || r.PID != t.PID || r.Workspace != t.Workspace || r.LeaseID != t.LeaseID || r.LaunchID != t.LaunchID {
			return Unknown
		}
		switch r.State {
		case "running":
			return Running
		case "ended":
			return Ended
		default:
			return Unknown
		}
	}
	if !observationPane(t.Ref) || d.tmux == nil {
		return Unknown
	}
	out, err := d.tmux(ctx, t.Ref)
	if err != nil || ctx.Err() != nil || len(out) > 1024 {
		return Unknown
	}
	fields := strings.Split(strings.TrimSpace(out), "\t")
	if len(fields) != 3 || fields[0] != t.Ref {
		return Unknown
	}
	pid, err := strconv.ParseInt(fields[1], 10, 32)
	if err != nil || pid <= 0 || strconv.FormatInt(pid, 10) != fields[1] || int(pid) != t.PID {
		return Unknown
	}
	switch fields[2] {
	case "0":
		return Running
	case "1":
		return Ended
	default:
		return Unknown
	}
}
func observationPane(ref string) bool {
	if len(ref) < 2 || len(ref) > 21 || ref[0] != '%' {
		return false
	}
	n, err := strconv.ParseUint(ref[1:], 10, 64)
	return err == nil && strconv.FormatUint(n, 10) == ref[1:]
}

type observationOutput struct {
	body     []byte
	overflow bool
}

func (b *observationOutput) Write(p []byte) (int, error) {
	if len(p) > 1024-len(b.body) {
		b.overflow = true
		return 0, errors.New("terminal observation output too large")
	}
	b.body = append(b.body, p...)
	return len(p), nil
}
func observeTmux(ctx context.Context, ref string) (string, error) {
	if !observationPane(ref) {
		return "", errors.New("invalid terminal observation")
	}
	cmd := exec.CommandContext(ctx, "tmux", "display-message", "-p", "-t", ref, "#{pane_id}\t#{pane_pid}\t#{pane_dead}")
	cmd.WaitDelay = observationTimeout
	out := &observationOutput{}
	cmd.Stdout = out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil || out.overflow {
		return "", errors.New("terminal observation unavailable")
	}
	return string(out.body), nil
}
