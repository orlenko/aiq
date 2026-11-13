package pool

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"
)

var ErrIdentityUnavailable = errors.New("machine identity unavailable")

type identitySources struct {
	machine  func() (io.ReadCloser, error)
	probe    func(context.Context) ([]byte, error)
	hostname func() (string, error)
}

// HostnameContext obtains a fresh identity without consulting or changing the
// legacy cache. Stable sources produce the same value as Hostname. Filesystem
// and hostname calls are checked cooperatively; the owned query is cancelable.
func HostnameContext(parent context.Context) (string, error) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return "", ErrIdentityUnavailable
	}
	return readIdentity(parent, identitySources{
		machine: func() (io.ReadCloser, error) { return os.Open("/etc/machine-id") },
		probe: func(ctx context.Context) ([]byte, error) {
			return runIdentityProbe(ctx, func(child context.Context) *exec.Cmd {
				return exec.CommandContext(child, "ioreg", "-rd1", "-c", "IOPlatformExpertDevice")
			})
		}, hostname: os.Hostname,
	})
}

func readIdentity(parent context.Context, sources identitySources) (string, error) {
	if parent == nil || sources.machine == nil || sources.probe == nil || sources.hostname == nil {
		return "", ErrIdentityUnavailable
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	if ctx.Err() != nil {
		return "", ErrIdentityUnavailable
	}
	f, err := sources.machine()
	if err == nil {
		if f == nil {
			return "", ErrIdentityUnavailable
		}
		raw, readErr := io.ReadAll(io.LimitReader(f, 4097))
		closeErr := f.Close()
		if ctx.Err() != nil || closeErr != nil || len(raw) > 4096 || !utf8.Valid(raw) {
			return "", ErrIdentityUnavailable
		}
		if readErr == nil {
			if id := strings.TrimSpace(string(raw)); id != "" {
				return id, nil
			}
		}
	}
	if ctx.Err() != nil {
		return "", ErrIdentityUnavailable
	}
	out, err := sources.probe(ctx)
	if ctx.Err() != nil || errors.Is(err, ErrIdentityUnavailable) || len(out) > 65536 {
		return "", ErrIdentityUnavailable
	}
	if err == nil {
		if !utf8.Valid(out) {
			return "", ErrIdentityUnavailable
		}
		if match := platformUUIDRe.FindSubmatch(out); match != nil {
			return string(match[1]), nil
		}
	}
	if ctx.Err() != nil {
		return "", ErrIdentityUnavailable
	}
	id, err := sources.hostname()
	if err != nil || ctx.Err() != nil || id == "" || len(id) > 4096 || !utf8.ValidString(id) {
		return "", ErrIdentityUnavailable
	}
	return id, nil
}

type identityOutput struct {
	bytes.Buffer
	cancel   context.CancelFunc
	overflow bool
}

func (w *identityOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := 65536 - w.Len()
	if len(p) > remaining {
		p = p[:remaining]
		w.overflow = true
		w.cancel()
	}
	_, _ = w.Buffer.Write(p)
	return n, nil
}

// WaitDelay closes owned copying pipes inside the original allowance. This
// cancellation applies to a native identity query, never a browser opener.
func runIdentityProbe(ctx context.Context, command func(context.Context) *exec.Cmd) ([]byte, error) {
	if ctx == nil || ctx.Err() != nil || command == nil {
		return nil, ErrIdentityUnavailable
	}
	deadline, ok := ctx.Deadline()
	const reserve = 250 * time.Millisecond
	if !ok || time.Until(deadline) <= reserve {
		return nil, ErrIdentityUnavailable
	}
	child, cancel := context.WithDeadline(ctx, deadline.Add(-reserve))
	defer cancel()
	out := &identityOutput{cancel: cancel}
	cmd := command(child)
	if cmd == nil {
		return nil, ErrIdentityUnavailable
	}
	cmd.Stdout = out // nil stdin/stderr are direct null descriptors, not pipes.
	cmd.Stdin = nil
	cmd.Stderr = nil
	cmd.WaitDelay = reserve
	err := cmd.Run() // Joins the owned process and its copying goroutine.
	if ctx.Err() != nil || child.Err() != nil || out.overflow || errors.Is(err, exec.ErrWaitDelay) {
		return nil, ErrIdentityUnavailable
	}
	if err != nil {
		return nil, err
	} // Ordinary probe failure permits hostname fallback.
	return out.Bytes(), nil
}
