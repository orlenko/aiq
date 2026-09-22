package agy

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/orlenko/aiq/internal/state"
)

// Usage is what one poll of an account yields.
type Usage struct {
	Windows  []state.Window
	Identity string
	Plan     string
}

// ErrNotLoggedIn is the poll's failure when the CLI has no login and would
// wait for a browser sign-in.
var ErrNotLoggedIn = errors.New("agy is not logged in — run: aiq account login")

// loginPrompt is what the CLI prints in that case.
const loginPrompt = "Authentication required"

// ProbeEnv names the file the status-line multiplexer writes the payload to
// when it runs inside a poll. The variable is set on the probe's environment
// only, so ordinary sessions never see it.
const ProbeEnv = "AIQ_AGY_PROBE"

// Poll reads the account's quota without spending any of it. The CLI has no
// usage command, but its status line carries the quota buckets, and it runs
// the statusLine command during a headless run's initialization, before the
// model is called. The poll starts `agy -p` with the multiplexer told to
// drop the payload into a file, waits for the file, and stops the run.
func (p *Provider) Poll(home string, native bool, dir string, now time.Time, timeout time.Duration) (*Usage, error) {
	geminiDir := GeminiDir(home, native)
	if _, installed, err := InstalledStatusline(geminiDir); err != nil {
		return nil, err
	} else if !installed {
		return nil, errors.New("agy statusline multiplexer not installed — run: aiq statusline install agy")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	probe, err := os.CreateTemp(dir, "probe-*.json")
	if err != nil {
		return nil, err
	}
	path := probe.Name()
	probe.Close()
	os.Remove(path)
	defer os.Remove(path)

	env := append(p.Env(home, native, false), ProbeEnv+"="+path)
	cmd := p.Command([]string{"-p", "aiq quota probe", "--output-format", "json", "--print-timeout", "30s"}, env)
	cmd.Dir = dir
	var output lockedBuffer
	cmd.Stdout, cmd.Stderr = &output, &output
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start agy: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	stop := func() {
		// TERM the whole group: the CLI starts a language server of its own.
		syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
	}
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			st, ok := ParseStatus(data)
			if ok {
				stop()
				return &Usage{Windows: st.Windows(now), Identity: st.Email, Plan: st.Plan}, nil
			}
		}
		select {
		case err := <-done:
			out := strings.TrimSpace(output.String())
			if len(out) > 300 {
				out = "…" + out[len(out)-300:]
			}
			if strings.Contains(out, loginPrompt) || strings.Contains(out, "authentication failed") {
				return nil, ErrNotLoggedIn
			}
			if err != nil {
				return nil, fmt.Errorf("agy exited before reporting quota: %v: %s", err, out)
			}
			return nil, fmt.Errorf("agy finished without reporting quota (is %s the aiq multiplexer?): %s", SettingsPath(geminiDir), out)
		case <-tick.C:
			// Logged out, the CLI prints a sign-in URL and waits a minute for
			// the browser; there is nothing to wait for.
			if strings.Contains(output.String(), loginPrompt) {
				stop()
				return nil, ErrNotLoggedIn
			}
			if time.Now().After(deadline) {
				stop()
				return nil, fmt.Errorf("agy reported no quota within %s", timeout)
			}
		}
	}
}

// lockedBuffer is a bytes.Buffer the poll may read while the CLI writes.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// ProbeDir is where polls run: a directory of aiq's own, so the CLI never
// records a real project as the probe's workspace.
func ProbeDir(dataDir string) string { return filepath.Join(dataDir, "agy-probe") }
