// Package herdr supervises a replaceable agent in a herdr-core terminal.
// It uses core's local JSON-RPC API; the older Herdr TUI is a different API.
package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const maxMessage = 2 << 20
const callTimeout = 4 * time.Second
const coreTimeout = time.Second

// Core pins a terminal to one endpoint, persistent core identity and root
// process. No operation uses core's focused-terminal default.
type Core struct {
	Socket   string
	ID       string
	Terminal string
	PID      int
}

type coreTerminal struct {
	Address string `json:"address"`
	State   string `json:"state"`
	PID     int    `json:"pid"`
}

type rpcError struct {
	Code int `json:"code"`
	Data struct {
		Kind string `json:"kind"`
	} `json:"data"`
}

// exchange never starts a core and never includes request contents in errors.
func exchange(socket, method string, params any, result any, timeout time.Duration) error {
	c, err := net.DialTimeout("unix", socket, timeout)
	if err != nil {
		return fmt.Errorf("Herdr %s: endpoint unavailable", method)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(timeout))
	request := struct {
		Version string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
	}{"2.0", 1, method, params}
	b, err := json.Marshal(request)
	if err != nil || len(b) > maxMessage {
		return errors.New("Herdr request is too large or invalid")
	}
	if _, err := c.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("Herdr %s: write failed; outcome unknown", method)
	}
	line, err := bufio.NewReader(io.LimitReader(c, maxMessage+1)).ReadBytes('\n')
	if err != nil || len(line) > maxMessage {
		return fmt.Errorf("Herdr %s: response unavailable; outcome unknown", method)
	}
	var response struct {
		ID     int             `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if json.Unmarshal(line, &response) != nil || response.ID != 1 {
		return errors.New("invalid Herdr response")
	}
	if response.Error != nil {
		return fmt.Errorf("Herdr %s: %s", method, response.Error.Data.Kind)
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(response.Result, result); err != nil {
		return errors.New("invalid Herdr result")
	}
	return nil
}

func (c Core) verify() error {
	var hello struct {
		Generation int `json:"generation"`
		Core       struct {
			ID string `json:"id"`
		} `json:"core"`
	}
	if err := exchange(c.Socket, "hello", map[string]any{}, &hello, coreTimeout); err != nil {
		return err
	}
	if hello.Generation != 1 || hello.Core.ID == "" || (c.ID != "" && c.ID != hello.Core.ID) {
		return errors.New("Herdr core identity or protocol changed")
	}
	var t coreTerminal
	if err := exchange(c.Socket, "terminal.get", map[string]any{"terminal": c.Terminal}, &t, coreTimeout); err != nil {
		return err
	}
	if t.State != "live" || t.Address != c.Terminal || t.PID != c.PID {
		return errors.New("Herdr terminal identity changed or ended")
	}
	return nil
}

var terminalID = regexp.MustCompile(`^t[1-9][0-9bcdfghjkmnpqrstvwxyz]*$`)

// currentSocket selects this terminal's injected owner. HC_SOCKET is a client
// selector and may point elsewhere; only an absent owner permits fallback.
func currentSocket() string {
	if socket, present := os.LookupEnv("HC_TERMINAL_SOCKET"); present {
		return socket
	}
	return os.Getenv("HC_SOCKET")
}

// CurrentCore verifies direct terminal ownership, including the controlling
// TTY. An inherited HC_TERMINAL_ID inside tmux/screen is not sufficient.
func CurrentCore() (Core, error) {
	c := Core{Socket: currentSocket(), Terminal: os.Getenv("HC_TERMINAL_ID")}
	if !filepath.IsAbs(c.Socket) || !terminalID.MatchString(c.Terminal) || os.Getenv("TMUX") != "" || os.Getenv("TMUX_PANE") != "" || os.Getenv("STY") != "" {
		return c, errors.New("native Herdr sessions need a direct Herdr-core terminal; open one, or use hc run --cwd <workspace> -- aiq long --backend herdr <provider>")
	}
	var hello struct {
		Generation int `json:"generation"`
		Core       struct {
			ID string `json:"id"`
		} `json:"core"`
	}
	if err := exchange(c.Socket, "hello", map[string]any{}, &hello, coreTimeout); err != nil {
		return c, err
	}
	if hello.Generation != 1 || hello.Core.ID == "" {
		return c, errors.New("unsupported Herdr core protocol (tested with hc 0.1.20)")
	}
	c.ID = hello.Core.ID
	var t coreTerminal
	if err := exchange(c.Socket, "terminal.get", map[string]any{"terminal": c.Terminal}, &t, coreTimeout); err != nil {
		return c, err
	}
	if t.State != "live" || t.Address != c.Terminal || t.PID <= 0 {
		return c, errors.New("Herdr terminal is not live")
	}
	c.PID = t.PID
	ours, err := processTTY(os.Getpid())
	if err != nil {
		return c, err
	}
	theirs, err := processTTY(c.PID)
	if err != nil || ours == "" || ours == "??" || ours == "?" || ours != theirs {
		return c, errors.New("this TTY is not the direct Herdr terminal; nested multiplexers are unsupported")
	}
	return c, nil
}

func processTTY(pid int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/ps", "-o", "tty=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", errors.New("cannot verify Herdr terminal ownership")
	}
	return strings.TrimSpace(string(out)), nil
}

func (c Core) call(method string, params any, result any) error {
	if err := c.verify(); err != nil {
		return err
	}
	return exchange(c.Socket, method, params, result, coreTimeout)
}

func (c Core) capture() (string, error) {
	var r struct {
		Text string `json:"text"`
	}
	err := c.call("terminal.read", map[string]any{"terminal": c.Terminal, "lines": 0}, &r)
	return r.Text, err
}

func (c Core) keys(keys []string) error {
	if err := c.verify(); err != nil {
		return err
	}
	return exchange(c.Socket, "terminal.keys", map[string]any{"terminal": c.Terminal, "keys": keys}, nil, 12*time.Second)
}

func (c Core) attach() error { return c.attachWithDiagnostics(os.Stderr) }

// Exact-lease callers suppress control diagnostics; legacy viewers retain
// their existing stderr and all viewers keep interactive stdin/stdout.
func (c Core) attachWithDiagnostics(diagnostics io.Writer) error {
	if err := c.verify(); err != nil {
		return err
	}
	if os.Getenv("HC_TERMINAL_ID") == c.Terminal {
		current, err := CurrentCore()
		if err != nil {
			return fmt.Errorf("cannot verify the current Herdr terminal for attachment: %w", err)
		}
		// Both endpoints have proved the same terminal identity. Socket
		// spelling may differ for an existing absolute path or symlink alias.
		if current.ID == c.ID && current.Terminal == c.Terminal && current.PID == c.PID {
			return nil
		}
		// A verified different core may use the same local ID. External attach
		// below remains pinned to the saved target rather than caller selectors.
	}
	cmd := exec.Command("hc", "attach", c.Terminal)
	cmd.Env = append(without(os.Environ(), func(k string) bool { return strings.HasPrefix(k, "HC_") }), "HC_SOCKET="+c.Socket)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, diagnostics
	return cmd.Run()
}

func without(env []string, drop func(string) bool) []string {
	var out []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if !drop(k) {
			out = append(out, kv)
		}
	}
	return out
}
