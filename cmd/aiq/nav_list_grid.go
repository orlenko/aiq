package main

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/terminal"
	"github.com/orlenko/aiq/internal/transcript"
)

// The Sessions grid shows conversations and managed workspaces in one list.
// Everything here comes from what one Sessions request already admits: the
// selected transcript rows, the launch metadata the activity pass read, and a
// metadata-only count of open Asks. No git probe runs, and no workspace root
// is derived from a directory beyond exact (symlink-resolved) path equality.

// navGridAskWait bounds how long a Sessions request waits for the shared Ask
// slot. A busy slot leaves the counts unknown; it never delays or fails the list.
const navGridAskWait = 250 * time.Millisecond

type navGridWorkspace struct {
	path  string
	known bool
}
type navGridAsks struct {
	count int
	known bool
}

// navListExtras is the grid's part of one Sessions response: per selected row
// its workspace and open-Ask count, the workspace-only rows to append, and
// whether those rows are incomplete.
type navListExtras struct {
	activity   []navSessionActivity
	summary    navActivitySummary
	workspaces []navGridWorkspace
	asks       []navGridAsks
	rows       []navSessionRow
	partial    bool
}

type navGridCandidate struct {
	path, canon     string
	known           bool
	provider        string
	firstMS, lastMS int64
	sessions        map[string]bool
	leases          []*state.NavigatorLease
}

func navGridMillis(ms int64) string {
	if ms <= 0 || ms > navConversationSafeInteger {
		return time.Time{}.UTC().Format(time.RFC3339Nano)
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339Nano)
}

func (f *navFactory) listExtras(ctx context.Context, roots transcript.Roots, q navListQuery, rows []transcript.Summary, run *navListRun) (navListExtras, error) {
	activity, summary, meta, err := f.listActivityWith(ctx, roots, q, rows, run)
	if err != nil {
		return navListExtras{}, err
	}
	out := navListExtras{activity: activity, summary: summary, partial: meta.failed}
	canonical := map[string]string{}
	canon := func(path string) string {
		if path == "" || !filepath.IsAbs(path) {
			return ""
		}
		if value, ok := canonical[path]; ok {
			return value
		}
		value := canonicalDir(path)
		canonical[path] = value
		return value
	}
	candidates, byCanon := navGridCandidates(meta.snapshot, canon)
	if ctx.Err() != nil {
		return navListExtras{}, ctx.Err()
	}

	// Each selected conversation's workspace: the lease mapping when the
	// activity pass made one, else a recorded long workspace at exactly the
	// conversation's directory, else unknown.
	out.workspaces = make([]navGridWorkspace, len(rows))
	listed := map[string]bool{}
	covered := map[*navGridCandidate]bool{}
	for i, row := range rows {
		listed[row.Provider+"/"+row.ID] = true
		cwd := canon(row.Cwd)
		if w := activity[i].Workspace; w != "" {
			out.workspaces[i] = navGridWorkspace{w, true}
		} else if c := byCanon[cwd]; cwd != "" && c != nil && c.known {
			out.workspaces[i] = navGridWorkspace{c.path, true}
		}
		if w := out.workspaces[i]; w.known {
			if c := byCanon[canon(w.path)]; c != nil {
				covered[c] = true
			}
		}
		if c := byCanon[cwd]; cwd != "" && c != nil {
			covered[c] = true
		}
	}
	selected := ""
	if !q.all {
		selected = canon(q.dir)
	}
	shown := make([]*navGridCandidate, 0)
	for _, c := range candidates {
		if covered[c] || !q.all && c.canon != selected {
			continue
		}
		recorded := false
		for key := range c.sessions {
			if listed[key] {
				recorded = true
				break
			}
		}
		if !recorded {
			shown = append(shown, c)
		}
	}
	sort.Slice(shown, func(i, j int) bool {
		if shown[i].lastMS != shown[j].lastMS {
			return shown[i].lastMS > shown[j].lastMS
		}
		return shown[i].path < shown[j].path
	})
	if len(shown) > transcript.ListRows {
		clear(shown[transcript.ListRows:])
		shown = shown[:transcript.ListRows]
		out.partial = true
	}

	counts, asksKnown := f.listAskCounts(ctx, run)
	out.asks = make([]navGridAsks, len(rows))
	bySession := map[string]int{}
	byWorkspace := map[string]int{}
	if asksKnown {
		for _, v := range counts {
			key := v.Provider + "/" + v.SessionID
			bySession[key] += int(v.Count)
			if !listed[key] {
				if w := canon(v.Workspace); w != "" {
					byWorkspace[w] += int(v.Count)
				}
			}
		}
		for i, row := range rows {
			out.asks[i] = navGridAsks{bySession[row.Provider+"/"+row.ID], true}
		}
	}

	out.rows = make([]navSessionRow, 0, len(shown))
	for _, c := range shown {
		if err := ctx.Err(); err != nil {
			return navListExtras{}, err
		}
		row := navSessionRow{Kind: "workspace", Provider: c.provider, Cwd: c.path, Started: navGridMillis(c.firstMS), Updated: navGridMillis(c.lastMS),
			Commands: []navListCommand{}, Workspace: c.path, WorkspaceKnown: c.known, Activity: navSessionActivity{Live: "unknown"}}
		if asksKnown {
			row.PendingAsks, row.PendingAsksKnown = byWorkspace[c.canon], true
		}
		if len(c.leases) == 1 {
			lease := c.leases[0]
			a := &row.Activity
			switch meta.observed[lease.ID] {
			case terminal.Running:
				a.Live = "live"
			case terminal.Ended:
				a.Live = "ended"
			}
			a.Managed, a.Workspace, a.Account, a.Launcher, a.Pane = true, c.path, lease.AccountID, lease.Launcher, lease.Pane
			a.LeaseID, a.Started, a.Drain, a.DrainManual = strconv.FormatInt(lease.ID, 10), strconv.FormatInt(lease.StartedAt, 10), lease.Drain, lease.DrainManual
		}
		a := row.Activity
		if !run.reserve(1024, row.Provider, row.Cwd, row.Workspace, row.Started, row.Updated, a.Live, a.Workspace, a.Account, a.Launcher, a.Pane, a.LeaseID, a.Started, a.Drain) {
			return navListExtras{}, errNavAuxTooLarge
		}
		out.rows = append(out.rows, row)
	}
	return out, ctx.Err()
}

// navGridCandidates lists the long workspaces recorded in launch metadata:
// each top-level long lease's workspace (a known root), and each long launch
// directory not already one of those (not known to be a root). Order is
// deterministic; byCanon finds a candidate by its symlink-resolved path.
func navGridCandidates(snapshot *state.NavigatorSnapshot, canon func(string) string) ([]*navGridCandidate, map[string]*navGridCandidate) {
	byCanon := map[string]*navGridCandidate{}
	out := []*navGridCandidate{}
	if snapshot == nil {
		return out, byCanon
	}
	add := func(path string, known bool) *navGridCandidate {
		c := canon(path)
		if c == "" {
			return nil
		}
		if value := byCanon[c]; value != nil {
			return value
		}
		value := &navGridCandidate{path: path, canon: c, known: known, sessions: map[string]bool{}}
		byCanon[c] = value
		out = append(out, value)
		return value
	}
	note := func(c *navGridCandidate, provider, id string, ms int64) {
		if transcript.IsFullID(provider, id) {
			c.sessions[provider+"/"+id] = true
		}
		if ms > 0 && (c.firstMS == 0 || ms < c.firstMS) {
			c.firstMS = ms
		}
		if ms > c.lastMS {
			c.lastMS, c.provider = ms, provider
		}
		if c.provider == "" {
			c.provider = provider
		}
	}
	for i := range snapshot.Leases {
		lease := &snapshot.Leases[i]
		if lease.Mode != state.ModeLong || lease.Depth != 0 || !navCatalogPath(lease.Workspace) {
			continue
		}
		if c := add(lease.Workspace, true); c != nil {
			c.leases = append(c.leases, lease)
			ms := int64(0)
			if lease.StartedAt > 0 && lease.StartedAt <= navConversationSafeInteger/1000 {
				ms = lease.StartedAt * 1000
			}
			note(c, lease.Provider, lease.SessionID, ms)
		}
	}
	for _, launch := range snapshot.Launches {
		if launch.Mode != state.ModeLong || !navCatalogPath(launch.Cwd) {
			continue
		}
		if c := add(launch.Cwd, false); c != nil {
			note(c, launch.Provider, launch.SessionID, launch.StartedMS)
		}
	}
	return out, byCanon
}

// listAskCounts reads the open-Ask counts under the shared Ask slot. Any
// failure, a busy slot, or counts that would not fit the list's remaining
// allowance leave them unknown; none of that fails the Sessions list. A
// missing Asks database means Asks are not set up: nothing is open.
func (f *navFactory) listAskCounts(ctx context.Context, run *navListRun) ([]state.AskCount, bool) {
	if f.input.asksPath == "" || ctx.Err() != nil {
		return nil, false
	}
	wait := time.NewTimer(navGridAskWait)
	defer wait.Stop()
	select {
	case f.askSlots <- struct{}{}:
		defer func() { <-f.askSlots }()
	case <-wait.C:
		return nil, false
	case <-ctx.Done():
		return nil, false
	}
	if _, err := os.Lstat(f.input.asksPath); os.IsNotExist(err) {
		return []state.AskCount{}, true
	}
	counts, err := func() (counts []state.AskCount, err error) {
		store, err := state.OpenReadOnly(f.input.asksPath)
		if err != nil {
			return nil, err
		}
		defer func() {
			if closeErr := store.Close(); closeErr != nil {
				counts, err = nil, closeErr
			}
		}()
		return store.ReadOpenAskCounts(ctx, f.policy.asks.reader)
	}()
	if err != nil || ctx.Err() != nil {
		return nil, false
	}
	need := int64(0)
	for _, v := range counts {
		need += 64 + int64(len(v.Provider)+len(v.SessionID)+len(v.Workspace))
	}
	if !run.tryReserve(need) {
		return nil, false
	}
	return counts, true
}
