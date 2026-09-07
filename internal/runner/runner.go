// Package runner runs a worker invocation as a child whose output is tee'd,
// so a usage-limit rejection can be recognised and rerouted.
package runner

import (
	"io"
	"os"
	"os/exec"
	"regexp"
	"sync"
	"time"

	"github.com/orlenko/aiq/internal/proc"
)

// ring keeps the last n bytes written to it.
type ring struct {
	mu    sync.Mutex
	buf   []byte
	max   int
	total int64
}

func (r *ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.total += int64(len(p))
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.max {
		r.buf = r.buf[len(r.buf)-r.max:]
	}
	return len(p), nil
}

func (r *ring) Bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.buf...)
}

// WorkerResult describes a finished worker run.
type WorkerResult struct {
	Code     int
	Elapsed  time.Duration
	LimitHit bool   // output matched a usage-limit pattern
	Match    string // the matching text
	Total    int64  // bytes of combined output
}

// RunWorker runs cmd with the terminal inherited, teeing its output into a
// small buffer that is matched against patterns on exit.
func RunWorker(cmd *exec.Cmd, patterns []*regexp.Regexp) (WorkerResult, error) {
	var res WorkerResult
	tail := &ring{max: 32 * 1024}
	cmd.Stdin = os.Stdin
	cmd.Stdout = io.MultiWriter(os.Stdout, tail)
	cmd.Stderr = io.MultiWriter(os.Stderr, tail)
	// A grandchild that inherits the pipes (a dev server, an MCP server)
	// must not keep the lease alive after the worker itself exited.
	cmd.WaitDelay = 5 * time.Second

	start := time.Now()
	code, err := proc.Wait(cmd)
	res.Elapsed = time.Since(start)
	if err != nil {
		return res, err
	}
	res.Code = code
	res.Total = tail.total
	out := tail.Bytes()
	for _, re := range patterns {
		if m := re.Find(out); m != nil {
			res.LimitHit = true
			res.Match = string(m)
			break
		}
	}
	return res, nil
}
