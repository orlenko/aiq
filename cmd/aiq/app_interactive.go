package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

// Interactive sessions: aiq records the Herdr terminal one started in
// (recordInteractiveTerminal) but runs nothing in it, so there is no runner
// record and no witness label. A recorded terminal is exported only when, now:
//   - the lease's process is the one that took the lease (it started no later),
//   - the terminal's recorded root process is its ancestor,
//   - the core still has that ID, a live terminal at that address, that root.
//
// A conversation is linked to an interactive lease only for Claude, through
// the registration Claude writes for its own pid (sessions/<pid>.json) on the
// lease's process or a descendant, written after that process started. Codex
// and Antigravity interactive sessions stay unlinked: interactive_unmapped.

const (
	appInteractiveUnmapped = "interactive_unmapped"
	// appLeaseTerminalRows bounds the recorded interactive terminals read.
	appLeaseTerminalRows = 4096
	appProcBytes         = 16 << 20
	appProcAncestry      = 64
	appProcDescendants   = 256
	appRegistrationBytes = 64 << 10
)

type appProc struct {
	ppid  int
	start time.Time
}
type appProcs map[int]appProc

// appProductionProcs is one listing of this machine's processes.
func appProductionProcs(ctx context.Context) (appProcs, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/ps", "-A", "-o", "pid=", "-o", "ppid=", "-o", "lstart=")
	cmd.Env = append(os.Environ(), "LC_ALL=C", "TZ=")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	procs, parseErr := appParseProcs(io.LimitReader(out, appProcBytes), time.UTC)
	io.Copy(io.Discard, out)
	if err := cmd.Wait(); err != nil {
		return nil, err
	}
	return procs, parseErr
}

// appParseProcs reads `pid ppid lstart` lines; lstart is in loc. A line that
// does not parse leaves that process out, which only ever unmaps it.
func appParseProcs(r io.Reader, loc *time.Location) (appProcs, error) {
	procs := appProcs{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 7 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		start, err3 := time.ParseInLocation("Mon Jan 2 15:04:05 2006", strings.Join(f[2:], " "), loc)
		if err1 != nil || err2 != nil || err3 != nil || pid <= 0 || ppid < 0 {
			continue
		}
		procs[pid] = appProc{ppid: ppid, start: start}
	}
	return procs, sc.Err()
}

// sameLeaseProcess says pid is still the process that took the lease at
// startedAt (unix seconds): a reused pid started later than that.
func (p appProcs) sameLeaseProcess(pid int, startedAt int64) bool {
	v, ok := p[pid]
	return ok && startedAt > 0 && !v.start.IsZero() && v.start.Unix() <= startedAt+1
}

// ancestor says root is pid or one of its ancestors.
func (p appProcs) ancestor(root, pid int) bool {
	for i := 0; i < appProcAncestry && pid > 1; i++ {
		if pid == root {
			return true
		}
		v, ok := p[pid]
		if !ok {
			return false
		}
		pid = v.ppid
	}
	return pid == root && root > 1
}

// selfAndDescendants is pid and its descendants, nearest first, bounded.
func (p appProcs) selfAndDescendants(pid int) []int {
	children := map[int][]int{}
	for child, v := range p {
		children[v.ppid] = append(children[v.ppid], child)
	}
	out := []int{pid}
	for i := 0; i < len(out) && len(out) < appProcDescendants; i++ {
		for _, c := range children[out[i]] {
			if len(out) == appProcDescendants {
				break
			}
			out = append(out, c)
		}
	}
	return out
}

func appProductionLeaseTerminals(ctx context.Context, host string) (map[int64]state.LeaseTerminal, error) {
	path := paths.StateDB()
	if _, err := os.Lstat(path); err != nil {
		return nil, err
	}
	st, err := state.OpenReadOnly(path)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	return st.ReadLeaseTerminals(ctx, host, appLeaseTerminalRows)
}

// appClaudeRegistration is the conversation Claude registered for pid in one
// of dirs, if exactly one regular file names that pid, that id, and was
// written no earlier than the process started.
func appClaudeRegistration(dirs []string, pid int, start time.Time) (string, bool) {
	found := ""
	for _, dir := range dirs {
		path := filepath.Join(dir, strconv.Itoa(pid)+".json")
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > appRegistrationBytes || info.ModTime().Before(start.Add(-time.Second)) {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil || len(data) > appRegistrationBytes {
			continue
		}
		var r struct {
			PID        int    `json:"pid"`
			SessionID  string `json:"sessionId"`
			Entrypoint string `json:"entrypoint"`
		}
		if json.NewDecoder(bytes.NewReader(data)).Decode(&r) != nil || r.PID != pid || !transcript.IsFullID("claude", r.SessionID) || strings.HasPrefix(r.Entrypoint, "sdk") {
			continue
		}
		if found != "" && found != r.SessionID {
			return "", false
		}
		found = r.SessionID
	}
	return found, found != ""
}

// appInteractive is what one bridge run knows about interactive leases.
type appInteractive struct {
	terminals map[int64]state.LeaseTerminal
	procs     appProcs
	links     map[int64]string // lease ID -> Claude conversation
	failed    bool
}

func (t *appTargets) interactive() *appInteractive {
	if t.inter != nil {
		return t.inter
	}
	in := &appInteractive{links: map[int64]string{}}
	t.inter = in
	if t.failed || t.env.leaseTerminals == nil || t.env.procs == nil {
		in.failed = true
		return in
	}
	terms, err := t.env.leaseTerminals(t.ctx, t.snapshot.HostIdentity())
	if err != nil {
		in.failed = true
		return in
	}
	in.terminals = terms
	if len(terms) == 0 {
		return in
	}
	if in.procs, err = t.env.procs(t.ctx); err != nil {
		in.failed = true
		return in
	}
	var dirs []string
	if t.env.input.roots != nil {
		if roots, err := t.env.input.roots(t.ctx); err == nil {
			seen := map[string]bool{}
			for _, p := range roots.ClaudeProjects {
				d := filepath.Join(filepath.Dir(p), "sessions")
				if !seen[d] {
					seen[d] = true
					dirs = append(dirs, d)
				}
			}
		}
	}
	for i := range t.snapshot.Leases {
		l := &t.snapshot.Leases[i]
		rec, ok := terms[l.ID]
		if !ok || l.Mode != state.ModeInteractive || appLeaseProvider(l) != "claude" || rec.PID != l.PID || !in.procs.sameLeaseProcess(int(l.PID), l.StartedAt) {
			continue
		}
		ids := map[string]bool{}
		for _, pid := range in.procs.selfAndDescendants(int(l.PID)) {
			if id, ok := appClaudeRegistration(dirs, pid, in.procs[pid].start); ok {
				ids[id] = true
			}
		}
		if len(ids) == 1 {
			for id := range ids {
				in.links[l.ID] = id
			}
		}
	}
	return in
}

func appLeaseProvider(l *state.NavigatorLease) string {
	if l.Provider != "" {
		return l.Provider
	}
	provider, _, err := state.ParseAccountID(l.AccountID)
	if err != nil {
		return ""
	}
	return provider
}

// checkInteractive verifies an interactive lease's recorded terminal now.
func (t *appTargets) checkInteractive(l *state.NavigatorLease) appTargetResult {
	in := t.interactive()
	if in.failed {
		return appTargetResult{reason: appUnknownTerm}
	}
	rec, ok := in.terminals[l.ID]
	if !ok || rec.PID != l.PID {
		return appTargetResult{reason: appInteractiveUnmapped}
	}
	if !in.procs.sameLeaseProcess(int(l.PID), l.StartedAt) {
		return appTargetResult{reason: appEnded}
	}
	if !in.procs.ancestor(int(rec.TerminalPID), int(l.PID)) {
		return appTargetResult{reason: appEnded}
	}
	if t.ctx.Err() != nil || t.env.verifyTerminal == nil {
		return appTargetResult{reason: appUnknownTerm}
	}
	target, reason := t.env.verifyTerminal(rec)
	if reason != "" {
		return appTargetResult{reason: reason}
	}
	return appTargetResult{terminal: &appTerminal{Address: target.Terminal, CoreID: target.CoreID, Witness: appWitness{TerminalPID: target.PID, Label: target.Label}}}
}

// forLease is the verified terminal of one lease in this run's snapshot,
// long or interactive, for rows built from leases (sessions).
func (t *appTargets) forLease(id int64) (*appTerminal, string) {
	if t.failed {
		return nil, appUnknownTerm
	}
	for i := range t.snapshot.Leases {
		if l := &t.snapshot.Leases[i]; l.ID == id {
			switch {
			case appLongTop(l):
				return t.resolve(l, "origin")
			case l.Mode == state.ModeInteractive:
				return t.resolve(l, "origin")
			}
			return nil, appNoLease
		}
	}
	return nil, appNoLease
}
