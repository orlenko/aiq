//go:build darwin || linux

package herdr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"github.com/orlenko/aiq/internal/proc"
)

// Options starts in a verified, existing core terminal. Ready releases the
// caller's workspace startup lock after ownership and the first PID are saved.
type Options struct {
	Core                    Core
	Workspace, Session, Dir string
	Args                    []string
	Ready                   func()
	Hook                    func(pid int, generation uint64, h Hook) []byte
}

type incoming struct {
	method string
	q      request
	reply  chan response
}

type session struct {
	r          Record
	options    Options
	cmd        *exec.Cmd
	exited     chan error
	reaped     bool
	mode       *term.State
	foreground int
	stopping   atomic.Bool
	pid        atomic.Int64
	generation atomic.Uint64
}

// Run keeps the PTY while the provider child changes. A normal exit is final;
// unlike tmux's retained dead panes, it is never a reason to start fresh.
func Run(o Options) (int, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return 1, errors.New("native Herdr startup needs terminal stdin and stdout")
	}
	if err := o.Core.verify(); err != nil {
		return 1, err
	}
	ref, err := newRef(o.Workspace)
	if err != nil {
		return 1, err
	}
	dir, err := os.MkdirTemp("/tmp", "aiqh-")
	if err != nil {
		return 1, err
	}
	defer os.RemoveAll(dir)
	listener, err := net.Listen("unix", filepath.Join(dir, "control"))
	if err != nil {
		return 1, err
	}
	defer listener.Close()
	s := &session{options: o, r: Record{Ref: ref, Workspace: o.Workspace, Session: o.Session, Socket: filepath.Join(dir, "control"), RunnerPID: os.Getpid(), State: "starting", Core: o.Core}}
	s.mode, err = term.GetState(int(os.Stdin.Fd()))
	if err != nil {
		return 1, err
	}
	s.foreground, err = unix.IoctlGetInt(int(os.Stdin.Fd()), unix.TIOCGPGRP)
	if err != nil {
		return 1, err
	}
	if s.foreground != syscall.Getpgrp() {
		return 1, errors.New("native Herdr startup must run in the foreground of its terminal")
	}
	signal.Ignore(syscall.SIGTTOU)
	defer signal.Reset(syscall.SIGTTOU)
	defer s.restore()
	if err := save(s.r); err != nil {
		return 1, err
	}
	done := make(chan struct{})
	defer close(done)
	requests := make(chan incoming, 8)
	go s.serve(listener, requests, done)
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGHUP, syscall.SIGTERM, os.Interrupt, syscall.SIGQUIT)
	defer signal.Stop(signals)
	wakeStop := make(chan struct{}, 1)
	go func() {
		select {
		case <-signals:
			s.stopping.Store(true)
			wakeStop <- struct{}{}
		case <-done:
		}
	}()
	finish := func() {
		s.r.State = "ended"
		s.r.PID = 0
		s.pid.Store(0)
		if err := save(s.r); err != nil {
			fmt.Fprintln(os.Stderr, "aiq: could not record ended Herdr session; ownership remains uncertain")
		}
	}
	if err := s.start(o.Dir, o.Args); err != nil {
		if s.cmd == nil || s.reaped {
			finish()
		}
		return 1, err
	}
	if o.Ready != nil {
		o.Ready()
	}
	for {
		if s.stopping.Load() {
			_, err := s.stopChild()
			if err != nil {
				s.r.State = "lost"
				fmt.Fprintln(os.Stderr, "aiq: native Herdr exit observation failed:", err)
				save(s.r)
				return 1, err
			}
			finish()
			return 0, nil
		}
		select {
		case <-wakeStop:
			continue
		case err := <-s.exited:
			if err != nil {
				s.r.State = "lost"
				save(s.r)
				s.exited = nil
				continue
			}
			code, err := s.stopChild()
			if err != nil {
				s.r.State = "lost"
				fmt.Fprintln(os.Stderr, "aiq: native Herdr cleanup failed:", err)
				save(s.r)
				s.exited = nil
				continue
			}
			finish()
			return code, nil
		case in := <-requests:
			out := response{Record: s.r}
			q := in.q
			if q.Ref != s.r.Ref {
				out.Error = "Herdr runner identity mismatch"
				in.reply <- out
				continue
			}
			if in.method != "info" && (q.PID != s.r.PID || q.Generation != s.r.Generation) {
				out.Error = "stale Herdr child generation"
				in.reply <- out
				continue
			}
			switch in.method {
			case "info":
			case "register":
				if q.LeaseID <= 0 || q.LaunchID <= 0 || len(q.Fallback) > 100 || s.r.State != "starting" {
					out.Error = "invalid Herdr registration"
					break
				}
				s.r.LeaseID, s.r.LaunchID, s.r.Fallback, s.r.State = q.LeaseID, q.LaunchID, q.Fallback, "running"
				if err := save(s.r); err != nil {
					out.Error = "Herdr registration could not be recorded"
				} else {
					s.status("idle", "")
				}
			case "hook":
				if q.Hook == nil || s.r.State != "running" || s.options.Hook == nil {
					out.Error = "Herdr child has not registered"
					break
				}
				h := *q.Hook
				h.LaunchID = s.r.LaunchID
				h.Ref = s.r.Ref
				out.Output = s.options.Hook(s.r.PID, s.r.Generation, h)
				state := ""
				switch q.Hook.Event {
				case "sessionstart", "stop":
					state = "idle"
				case "userpromptsubmit":
					state = "working"
				}
				if q.Hook.Event == "stop" && len(out.Output) > 0 {
					state = "working"
				}
				if state != "" {
					s.status(state, q.Hook.Provider)
				}
			case "label":
				s.r.Label = cleanLabel(q.Label)
				if err := save(s.r); err != nil {
					out.Error = "Herdr label could not be recorded"
				}
				// Labels are presentation only; permission-sensitive lifecycle
				// status still comes from the provider and turn hooks.
				if err := s.r.Core.call("terminal.label", map[string]any{"terminal": s.r.Core.Terminal, "labels": map[string]string{"aiq.status": s.r.Label}}, nil); err != nil {
					out.Error = err.Error()
				}
			case "replace":
				if s.r.State != "running" {
					out.Error = "Herdr child is not running; explicit resume is required"
					break
				}
				if err := s.validCommand(q.Dir, q.Args); err != nil {
					out.Error = err.Error()
					break
				}
				s.r.State = "replacing"
				s.r.PreviousPID = s.r.PID
				if err := save(s.r); err != nil {
					out.Error = "cannot record Herdr replacement"
					break
				}
				if _, err := s.stopChild(); err != nil {
					s.r.State = "lost"
					save(s.r)
					out.Error = err.Error()
					break
				}
				if s.stopping.Load() {
					finish()
					out.Record = s.r
					in.reply <- out
					return 0, nil
				}
				if err := s.start(q.Dir, q.Args); err != nil {
					out.Error = err.Error()
					out.Record = s.r
					in.reply <- out
					if s.cmd == nil || s.reaped {
						finish()
					}
					fmt.Fprintln(os.Stderr, "aiq: native Herdr successor could not start; resume explicitly")
					return 1, err
				}
			case "stop":
				s.stopping.Store(true)
				if _, err := s.stopChild(); err != nil {
					s.r.State = "lost"
					save(s.r)
					out.Error = err.Error()
					break
				}
				finish()
				out.Record = s.r
				in.reply <- out
				return 0, nil
			default:
				out.Error = "unknown Herdr control operation"
			}
			out.Record = s.r
			in.reply <- out
		}
	}
}

func (s *session) validCommand(dir string, args []string) error {
	if !filepath.IsAbs(dir) || len(args) == 0 || len(args) > 4096 || args[0] != s.options.Args[0] || !filepath.IsAbs(args[0]) {
		return errors.New("invalid Herdr successor command")
	}
	for _, a := range args {
		if strings.ContainsRune(a, 0) {
			return errors.New("invalid Herdr successor argument")
		}
	}
	return nil
}

func (s *session) start(dir string, args []string) error {
	if err := s.validCommand(dir, args); err != nil {
		return err
	}
	s.r.Generation++
	s.r.PID = 0
	s.r.LeaseID = 0
	s.r.LaunchID = 0
	s.r.Fallback = ""
	s.r.Label = ""
	s.r.State = "starting"
	s.pid.Store(0)
	s.generation.Store(s.r.Generation)
	if err := save(s.r); err != nil {
		return errors.New("cannot record Herdr startup")
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = ChildEnv(os.Environ(), s.r.Ref, s.r.Generation)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Foreground: true, Ctty: int(os.Stdin.Fd())}
	if err := cmd.Start(); err != nil {
		return errors.New("could not start native Herdr agent process")
	}
	s.cmd = cmd
	s.reaped = false
	s.exited = make(chan error, 1)
	go func(ch chan error) { ch <- waitExited(cmd.Process.Pid) }(s.exited)
	s.r.PID = cmd.Process.Pid
	s.pid.Store(int64(s.r.PID))
	if err := save(s.r); err != nil {
		_, cleanup := s.stopChild()
		if cleanup != nil {
			return cleanup
		}
		return errors.New("could not record native Herdr child; no provider was started")
	}
	return nil
}

func (s *session) restore() {
	unix.IoctlSetPointerInt(int(os.Stdin.Fd()), unix.TIOCSPGRP, s.foreground)
	if s.mode != nil {
		term.Restore(int(os.Stdin.Fd()), s.mode)
	}
}

// The leader stays unreaped until the group contains no executing members,
// reserving the PID/PGID across TERM, KILL and the positive cleanup barrier.
func (s *session) stopChild() (int, error) {
	if s.cmd == nil || s.reaped {
		return 0, nil
	}
	pid := s.cmd.Process.Pid
	active, err := groupActive(pid)
	if err != nil {
		return 1, err
	}
	if !active {
		return s.reapChild()
	}
	for _, step := range []struct {
		sig  syscall.Signal
		wait time.Duration
	}{{syscall.SIGTERM, 600 * time.Millisecond}, {syscall.SIGKILL, time.Second}} {
		if err := syscall.Kill(-pid, step.sig); err != nil && err != syscall.ESRCH {
			active, queryErr := groupActive(pid)
			if queryErr == nil && !active {
				return s.reapChild()
			}
			return 1, fmt.Errorf("cannot signal owned Herdr child group: %v", err)
		}
		until := time.Now().Add(step.wait)
		for {
			active, err := groupActive(pid)
			if err != nil {
				return 1, err
			}
			if !active {
				return s.reapChild()
			}
			if time.Now().After(until) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	return 1, errors.New("Herdr child cleanup is incomplete; workspace ownership retained")
}

func (s *session) reapChild() (int, error) {
	err := s.cmd.Wait()
	s.reaped = true
	s.restore()
	if err == nil {
		return 0, nil
	}
	var e *exec.ExitError
	if errors.As(err, &e) {
		code := e.ExitCode()
		if code < 0 {
			code = 1
		}
		return code, nil
	}
	return 1, errors.New("could not reap owned Herdr child")
}

type cappedBuffer struct{ bytes.Buffer }

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > maxMessage {
		return 0, errors.New("process snapshot too large")
	}
	return b.Buffer.Write(p)
}

func groupActive(pgid int) (bool, error) {
	// Only IDs and state are read; command lines/environments are not inspected.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/ps", "-axo", "pid=,pgid=,stat=")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}
	var out cappedBuffer
	cmd.Stdout = &out
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		return false, errors.New("cannot establish Herdr child group cleanup")
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 3 {
			return false, errors.New("invalid process group snapshot")
		}
		g, err := strconv.Atoi(f[1])
		if err != nil {
			return false, errors.New("invalid process group snapshot")
		}
		if g == pgid && !strings.HasPrefix(f[2], "Z") {
			return true, nil
		}
	}
	return false, nil
}

// Reclaim only after positive absence evidence, under the workspace startup
// lock. Transitional crash windows retain unknown ownership even with PID 0.
func Reclaim(r Record) (bool, error) {
	if proc.Alive(r.RunnerPID) || r.State == "starting" || r.State == "replacing" || r.PID <= 0 {
		return false, nil
	}
	if proc.Alive(r.PID) {
		return false, nil
	}
	active, err := groupActive(r.PID)
	if err != nil || active {
		return false, err
	}
	cur, err := load(r.Ref)
	if err != nil {
		return false, err
	}
	if cur.RunnerPID != r.RunnerPID || cur.PID != r.PID || cur.Generation != r.Generation {
		return false, errors.New("Herdr ownership changed")
	}
	cur.State = "ended"
	cur.PID = 0
	return true, save(cur)
}

func (s *session) status(state, provider string) {
	switch provider {
	case "claude", "codex", "agy":
	default:
		provider = ""
	}
	if provider == "" {
		fmt.Fprintf(os.Stdout, "\x1b]7501;state=%s\x1b\\", state)
	} else {
		fmt.Fprintf(os.Stdout, "\x1b]7501;state=%s:app=%s\x1b\\", state, provider)
	}
}
func cleanLabel(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 32 || (r >= 127 && r <= 159) {
			return ' '
		}
		return r
	}, s)
	if len(s) > 160 {
		s = string([]rune(s)[:min(80, len([]rune(s)))])
	}
	return s
}

func (s *session) serve(l net.Listener, requests chan<- incoming, done <-chan struct{}) {
	slots := make(chan struct{}, 8)
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		select {
		case slots <- struct{}{}:
		default:
			c.Close()
			continue
		}
		go func() {
			defer c.Close()
			defer func() { <-slots }()
			c.SetDeadline(time.Now().Add(callTimeout))
			var envelope struct {
				Version string  `json:"jsonrpc"`
				ID      int     `json:"id"`
				Method  string  `json:"method"`
				Params  request `json:"params"`
			}
			dec := json.NewDecoder(io.LimitReader(c, maxMessage+1))
			dec.DisallowUnknownFields()
			if dec.Decode(&envelope) != nil || envelope.ID != 1 || envelope.Version != "2.0" {
				return
			}
			if envelope.Method == "hook" {
				c.SetDeadline(time.Now().Add(8 * time.Second))
			}
			q := envelope.Params
			if envelope.Method == "stop" && q.Ref == s.r.Ref && q.PID == int(s.pid.Load()) && q.Generation == s.generation.Load() {
				s.stopping.Store(true)
			}
			in := incoming{envelope.Method, q, make(chan response, 1)}
			select {
			case requests <- in:
			case <-done:
				return
			}
			select {
			case out := <-in.reply:
				json.NewEncoder(c).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": out})
			case <-done:
			}
		}()
	}
}
