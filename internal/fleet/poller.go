package fleet

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"sync"
	"time"
)

// Runner runs `aiq leases --json` on host and returns its stdout. It must
// return once ctx ends.
type Runner func(ctx context.Context, host string) ([]byte, error)

// Host status values.
const (
	Pending = "pending" // not answered yet
	OK      = "ok"
	Failed  = "error"
)

// Host is what the poller knows about one machine.
type Host struct {
	Name   string  // as written in [fleet] hosts
	Status string  // Pending, OK or Failed
	SeenAt int64   // unix time of the last good reply; 0 if none
	Error  string  // the last failure; "" after a good reply
	Leases []Lease // from the last good reply, kept through later failures
}

// Poller asks each host for its sessions every interval, but only while the
// Quotas page is being read: it starts on the first read and stops once no
// read has come for idle. Hosts are asked in parallel, each bounded by
// timeout, and one slow host never holds up another's result.
type Poller struct {
	hosts    []string
	run      Runner
	interval time.Duration
	timeout  time.Duration
	idle     time.Duration
	now      func() time.Time

	mu       sync.Mutex
	state    []Host
	running  bool
	lastRead time.Time
	first    chan struct{} // closed when the current run's first round ends
}

// NewPoller returns a stopped poller over hosts. A non-positive interval is
// one minute.
func NewPoller(hosts []string, run Runner, interval time.Duration) *Poller {
	if interval <= 0 {
		interval = time.Minute
	}
	p := &Poller{hosts: append([]string(nil), hosts...), run: run, interval: interval,
		timeout: 20 * time.Second, idle: 10 * time.Minute, now: time.Now}
	for _, h := range p.hosts {
		p.state = append(p.state, Host{Name: h, Status: Pending, Leases: []Lease{}})
	}
	return p
}

// Hosts returns a copy of every host's state, in [fleet] order, and keeps the
// poller running. When this read starts the poller, it waits up to wait (or
// until ctx ends) for the first round, so the page opens with answers.
func (p *Poller) Hosts(ctx context.Context, wait time.Duration) []Host {
	p.mu.Lock()
	p.lastRead = p.now()
	first := p.first
	if !p.running {
		p.running = true
		first = make(chan struct{})
		p.first = first
		go p.loop(first)
	}
	p.mu.Unlock()
	if wait > 0 {
		t := time.NewTimer(wait)
		select {
		case <-first:
		case <-t.C:
		case <-ctx.Done():
		}
		t.Stop()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Host, len(p.state))
	for i, h := range p.state {
		h.Leases = append([]Lease{}, h.Leases...)
		out[i] = h
	}
	return out
}

func (p *Poller) loop(first chan struct{}) {
	opened := false
	defer func() {
		// A failure here stops polling; it never stops the daemon. The next
		// read starts a fresh loop.
		_ = recover()
		if !opened {
			close(first)
		}
		p.mu.Lock()
		p.running = false
		p.mu.Unlock()
	}()
	p.round()
	opened = true
	close(first)
	for {
		t := time.NewTimer(p.interval)
		<-t.C
		p.mu.Lock()
		idle := p.now().Sub(p.lastRead) > p.idle
		p.mu.Unlock()
		if idle {
			return
		}
		p.round()
	}
}

func (p *Poller) round() {
	var wg sync.WaitGroup
	for i, host := range p.hosts {
		wg.Add(1)
		go func(i int, host string) {
			defer wg.Done()
			doc, err := p.ask(host)
			p.mu.Lock()
			defer p.mu.Unlock()
			h := &p.state[i]
			if err != nil {
				h.Status, h.Error = Failed, err.Error()
				return
			}
			h.Status, h.Error, h.SeenAt, h.Leases = OK, "", p.now().Unix(), doc.Leases
		}(i, host)
	}
	wg.Wait()
}

func (p *Poller) ask(host string) (doc Doc, err error) {
	defer func() {
		if v := recover(); v != nil {
			doc, err = Doc{}, errors.New("internal error reading the reply")
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()
	out, err := p.run(ctx, host)
	if err != nil {
		return Doc{}, err
	}
	return Decode(out)
}

// SSH is the production Runner: `ssh <host> <aiq> leases --json` without a
// terminal, stdin, agent or port forwarding, and without prompts, so a host
// that needs a password or an unknown host key fails instead of waiting.
// host and aiq are checked by config before they get here.
func SSH(aiq string) Runner {
	return func(ctx context.Context, host string) ([]byte, error) {
		bin, err := exec.LookPath("ssh")
		if err != nil {
			return nil, errors.New("ssh is not installed on this machine")
		}
		cmd := exec.CommandContext(ctx, bin, SSHArgs(host, aiq)...)
		stdout, stderr := &capped{max: MaxReplyBytes}, &capped{max: 4096}
		cmd.Stdout, cmd.Stderr = stdout, stderr
		cmd.WaitDelay = 2 * time.Second
		err = cmd.Run()
		if ctx.Err() != nil {
			return nil, errors.New("no answer in time")
		}
		if err != nil {
			if line := errorLine(stderr.buf.Bytes()); line != "" {
				return nil, errors.New(line)
			}
			return nil, errors.New("ssh " + err.Error())
		}
		if stdout.over {
			return nil, errors.New("reply too large")
		}
		return stdout.buf.Bytes(), nil
	}
}

// SSHArgs are ssh's arguments for one ask.
func SSHArgs(host, aiq string) []string {
	return []string{"-n", "-T", "-x", "-a",
		"-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "ClearAllForwardings=yes",
		"-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=2",
		"--", host, aiq + " leases --json"}
}

// capped keeps the first max bytes written and drops the rest, so a chatty
// command can neither fill memory nor block on a full pipe.
type capped struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (c *capped) Write(b []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		if len(b) > room {
			c.buf.Write(b[:room])
			c.over = true
		} else {
			c.buf.Write(b)
		}
	} else if len(b) > 0 {
		c.over = true
	}
	return len(b), nil
}
