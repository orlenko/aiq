package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// RateLimits is the telemetry returned by account/rateLimits/read.
type RateLimits struct {
	PrimaryUsedPct     float64 // -1 unknown
	PrimaryReset       int64   // unix seconds, 0 unknown
	PrimaryWindowSecs  int64
	SecondaryUsedPct   float64
	SecondaryReset     int64
	SecondaryWindowSec int64
	ResetCredits       int
}

// ErrAuthRequired reports that the stored credential no longer authenticates.
var ErrAuthRequired = fmt.Errorf("codex account authentication required")

type rpcMsg struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// session drives one `codex app-server` process over stdio JSON-RPC.
type session struct {
	cmd     *exec.Cmd
	stdin   interface{ Write([]byte) (int, error) }
	scanner *bufio.Scanner
	ctx     context.Context
	cancel  context.CancelFunc
	nextID  int
}

func (p *Provider) open(home string, native bool, timeout time.Duration) (*session, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	cmd := p.Command([]string{"app-server"}, p.Env(home, native, false))
	go func() {
		<-ctx.Done()
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
	}()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	s := &session{cmd: cmd, stdin: stdin, ctx: ctx, cancel: cancel}
	s.scanner = bufio.NewScanner(stdout)
	s.scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	if _, err := s.call("initialize", map[string]any{"clientInfo": map[string]any{
		"name": "aiq", "title": "aiq", "version": "0.3.0",
	}}); err != nil {
		s.close()
		return nil, fmt.Errorf("initialize: %w", err)
	}
	s.notify("initialized", map[string]any{})
	return s, nil
}

func (s *session) close() {
	if c, ok := s.stdin.(interface{ Close() error }); ok {
		c.Close()
	}
	s.cmd.Process.Kill()
	s.cmd.Wait()
	s.cancel()
}

func (s *session) send(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.stdin.Write(append(data, '\n'))
	return err
}

func (s *session) notify(method string, params any) {
	s.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (s *session) call(method string, params any) (json.RawMessage, error) {
	s.nextID++
	id := strconv.Itoa(s.nextID)
	if err := s.send(map[string]any{"jsonrpc": "2.0", "id": s.nextID, "method": method, "params": params}); err != nil {
		return nil, err
	}
	for s.scanner.Scan() {
		var msg rpcMsg
		if json.Unmarshal(s.scanner.Bytes(), &msg) != nil {
			continue
		}
		if strings.TrimSpace(string(msg.ID)) != id {
			continue
		}
		if msg.Error != nil {
			if strings.Contains(strings.ToLower(msg.Error.Message), "auth") {
				return nil, fmt.Errorf("%w: %s", ErrAuthRequired, msg.Error.Message)
			}
			return nil, fmt.Errorf("%s: %s", method, msg.Error.Message)
		}
		return msg.Result, nil
	}
	if s.ctx.Err() != nil {
		return nil, fmt.Errorf("app-server timed out")
	}
	if err := s.scanner.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("app-server exited before responding")
}

// Probe reads current rate limits for a home through the official
// `codex app-server` JSON-RPC API.
func (p *Provider) Probe(home string, native bool) (RateLimits, error) {
	rl := RateLimits{PrimaryUsedPct: -1, SecondaryUsedPct: -1}
	s, err := p.open(home, native, 30*time.Second)
	if err != nil {
		return rl, err
	}
	defer s.close()
	result, err := s.call("account/rateLimits/read", map[string]any{})
	if err != nil {
		return rl, err
	}
	parsed, ok := ParseRateLimitsResult(result, time.Now())
	if !ok {
		return rl, fmt.Errorf("rateLimits/read returned no recognizable windows")
	}
	return parsed, nil
}

// ConsumeResetCredit redeems one earned rate-limit reset. outcome is one of
// reset, alreadyRedeemed, nothingToReset, noCredit.
func (p *Provider) ConsumeResetCredit(home string, native bool, idempotencyKey string) (outcome string, err error) {
	s, err := p.open(home, native, 30*time.Second)
	if err != nil {
		return "", err
	}
	defer s.close()
	result, err := s.call("account/rateLimitResetCredit/consume", map[string]any{"idempotencyKey": idempotencyKey})
	if err != nil {
		return "", err
	}
	var doc struct {
		Outcome string `json:"outcome"`
	}
	if err := json.Unmarshal(result, &doc); err != nil {
		return "", err
	}
	return doc.Outcome, nil
}

// ParseRateLimitsResult tolerantly extracts primary/secondary windows from an
// account/rateLimits/read result. Codex versions vary in key naming
// (usedPercent/used_percent, windowMinutes/windowDurationMins, absolute
// resetsAt vs relative resetsInSeconds), so it accepts all of them.
func ParseRateLimitsResult(result json.RawMessage, now time.Time) (RateLimits, bool) {
	rl := RateLimits{PrimaryUsedPct: -1, SecondaryUsedPct: -1}
	var doc map[string]json.RawMessage
	if json.Unmarshal(result, &doc) != nil {
		return rl, false
	}
	container := doc
	for _, key := range []string{"rateLimits", "rate_limits"} {
		if raw, ok := doc[key]; ok {
			var inner map[string]json.RawMessage
			if json.Unmarshal(raw, &inner) == nil {
				container = inner
			}
		}
	}
	ok := false
	if w, found := parseRLWindow(container["primary"], now); found {
		rl.PrimaryUsedPct, rl.PrimaryReset, rl.PrimaryWindowSecs = w.pct, w.reset, w.window
		ok = true
	}
	if w, found := parseRLWindow(container["secondary"], now); found {
		rl.SecondaryUsedPct, rl.SecondaryReset, rl.SecondaryWindowSec = w.pct, w.reset, w.window
		ok = true
	}
	for _, key := range []string{"rateLimitResetCredits", "rate_limit_reset_credits", "resetCredits"} {
		if raw, ok := doc[key]; ok {
			var credits struct {
				Available *int `json:"availableCount"`
				Snake     *int `json:"available_count"`
			}
			if json.Unmarshal(raw, &credits) == nil {
				if credits.Available != nil {
					rl.ResetCredits = *credits.Available
				} else if credits.Snake != nil {
					rl.ResetCredits = *credits.Snake
				}
			}
		}
	}
	return rl, ok
}

type rlWindow struct {
	pct    float64
	reset  int64
	window int64
}

func parseRLWindow(raw json.RawMessage, now time.Time) (rlWindow, bool) {
	w := rlWindow{pct: -1}
	if len(raw) == 0 || string(raw) == "null" {
		return w, false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return w, false
	}
	found := false
	for _, key := range []string{"usedPercent", "used_percent", "usedPercentage", "used_percentage"} {
		var v float64
		if raw, ok := m[key]; ok && json.Unmarshal(raw, &v) == nil {
			w.pct, found = v, true
			break
		}
	}
	if !found {
		return w, false
	}
	for _, key := range []string{"windowDurationMins", "windowMinutes", "window_minutes"} {
		var mins float64
		if raw, ok := m[key]; ok && json.Unmarshal(raw, &mins) == nil {
			w.window = int64(mins * 60)
		}
	}
	for _, key := range []string{"windowSeconds", "window_seconds", "limit_window_seconds"} {
		var secs float64
		if raw, ok := m[key]; ok && json.Unmarshal(raw, &secs) == nil {
			w.window = int64(secs)
		}
	}
	for _, key := range []string{"resetsAt", "resets_at", "reset_at"} {
		if raw, ok := m[key]; ok {
			if t := parseFlexibleTime(raw); t > 0 {
				w.reset = t
				return w, true
			}
		}
	}
	for _, key := range []string{"resetsInSeconds", "resets_in_seconds", "reset_after_seconds"} {
		var secs float64
		if raw, ok := m[key]; ok && json.Unmarshal(raw, &secs) == nil {
			w.reset = now.Unix() + int64(secs)
			return w, true
		}
	}
	return w, true
}

// parseFlexibleTime accepts unix seconds (number or numeric string) or RFC3339.
func parseFlexibleTime(raw json.RawMessage) int64 {
	var n float64
	if json.Unmarshal(raw, &n) == nil {
		return int64(n)
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return 0
	}
	if u, err := strconv.ParseInt(s, 10, 64); err == nil {
		return u
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Unix()
	}
	return 0
}
