package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/proc"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/terminal"
	"github.com/orlenko/aiq/internal/transcript"
)

type navSessionActivity struct {
	Live        string `json:"live"`
	PID         int    `json:"pid"`
	Managed     bool   `json:"managed"`
	Workspace   string `json:"workspace"`
	Account     string `json:"account"`
	Launcher    string `json:"launcher"`
	Pane        string `json:"pane"`
	LeaseID     string `json:"lease_id"`
	Started     string `json:"started"`
	Drain       string `json:"drain"`
	DrainManual bool   `json:"drain_manual"`
	Continued   bool   `json:"continued"`
}
type navActivitySummary struct {
	Known           bool        `json:"known"`
	Live            int         `json:"live"`
	Elsewhere       int         `json:"elsewhere"`
	ClaudeOpenKnown bool        `json:"claude_open_known"`
	Observed        navWireTime `json:"observed"`
}
type navActivityOpen struct {
	pid       int
	duplicate bool
}

// Scan only the small live-registration files, not transcripts. Every directory
// entry and admitted file, even an unusable one, consumes the list's allowances.
// A surviving PID is an observation about a registration, never session proof.
func navClaudeRegistrations(ctx context.Context, roots []string, run *navListRun, entries *int64, maxEntries int64, alive func(int) bool) (map[string]navActivityOpen, bool, error) {
	out := map[string]navActivityOpen{}
	known := true
	seen := map[string]bool{}
	for _, root := range roots {
		dir := filepath.Join(filepath.Dir(root), "sessions")
		if seen[dir] {
			continue
		}
		seen[dir] = true
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		fd, err := os.Open(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			known = false
			continue
		}
		for {
			batch, readErr := fd.ReadDir(64)
			for _, entry := range batch {
				if err := ctx.Err(); err != nil {
					fd.Close()
					return nil, false, err
				}
				*entries++
				if *entries > maxEntries || !run.reserve(128, entry.Name()) {
					fd.Close()
					return nil, false, errNavAuxTooLarge
				}
				if !strings.HasSuffix(entry.Name(), ".json") || entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
					continue
				}
				path := filepath.Join(dir, entry.Name())
				if !run.reserve(128, path) {
					fd.Close()
					return nil, false, errNavAuxTooLarge
				}
				file, err := transcript.OpenRegular(path)
				if err != nil {
					known = false
					continue
				}
				info, err := file.Stat()
				if err != nil {
					file.Close()
					known = false
					continue
				}
				if info.Size() < 0 || info.Size() == math.MaxInt64 || !run.reserve(info.Size()+1) {
					file.Close()
					fd.Close()
					return nil, false, errNavAuxTooLarge
				}
				data, err := io.ReadAll(io.LimitReader(file, info.Size()+1))
				closeErr := file.Close()
				if int64(len(data)) > info.Size() {
					fd.Close()
					return nil, false, errNavAuxTooLarge
				}
				if err != nil || closeErr != nil {
					known = false
					continue
				}
				var record struct {
					PID int    `json:"pid"`
					ID  string `json:"sessionId"`
				}
				if json.Unmarshal(data, &record) != nil || record.PID <= 0 || record.PID > math.MaxInt32 || !transcript.IsFullID("claude", record.ID) || entry.Name() != strconv.Itoa(record.PID)+".json" {
					known = false
					continue
				}
				previous, exists := out[record.ID]
				if exists {
					known = false
					previous.duplicate = true
					out[record.ID] = previous
					continue
				}
				out[strings.Clone(record.ID)] = navActivityOpen{pid: record.PID}
			}
			if readErr != nil {
				if readErr != io.EOF {
					known = false
				}
				break
			}
		}
		if err := fd.Close(); err != nil {
			known = false
		}
	}
	for id, value := range out {
		if value.duplicate || !alive(value.pid) {
			value.pid = 0
			out[id] = value
		}
	}
	return out, known, nil
}

func (f *navFactory) listActivity(ctx context.Context, roots transcript.Roots, q navListQuery, rows []transcript.Summary, run *navListRun) ([]navSessionActivity, navActivitySummary, error) {
	out := make([]navSessionActivity, len(rows))
	totals := navActivitySummary{}
	at := f.input.now()
	observed, err := navConversationTime(at)
	if err != nil {
		return nil, totals, err
	}
	totals.Observed = observed
	for i, row := range rows {
		out[i].Live = "unknown"
		age := at.Sub(row.Updated)
		if (row.Provider == "codex" || row.Provider == "agy") && !row.Updated.IsZero() && age >= 0 && age < 30*time.Minute {
			out[i].Live = "recent"
		}
	}
	entries := int64(len(rows) + len(roots.ClaudeProjects) + len(roots.CodexSessions) + len(roots.AgyAppData))
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
	selectedDir := canon(q.dir)
	var snapshot *state.NavigatorSnapshot
	if f.input.metadata != nil {
		snapshot, err = f.input.metadata(ctx, state.NavigatorSnapshotLimits{Rows: f.policy.sessions.arrayEntries, TextBytes: f.policy.sessions.rowBytes})
	}
	if ctx.Err() != nil {
		return nil, totals, ctx.Err()
	}
	if errors.Is(err, state.ErrNavigatorSnapshotTooLarge) {
		return nil, totals, errNavAuxTooLarge
	}
	if err == nil && snapshot != nil {
		entries += int64(len(snapshot.Launches) + len(snapshot.Leases))
		if entries > f.policy.sessions.arrayEntries {
			return nil, totals, errNavAuxTooLarge
		}
		for _, v := range snapshot.Launches {
			if !run.reserve(256, v.Provider, v.AccountID, v.Launcher, v.Cwd, v.Mode, v.SessionID) {
				return nil, totals, errNavAuxTooLarge
			}
		}
		for _, v := range snapshot.Leases {
			if !run.reserve(256, v.Provider, v.AccountID, v.Launcher, v.Cwd, v.Mode, v.Workspace, v.Pane, v.SessionID, v.Drain) {
				return nil, totals, errNavAuxTooLarge
			}
		}
		totals.Known = true
		launches := map[int64][]state.NavigatorLaunch{}
		for _, v := range snapshot.Launches {
			launches[v.LeaseID] = append(launches[v.LeaseID], v)
		}
		type managed struct {
			lease          *state.NavigatorLease
			live           terminal.Observation
			cwd, workspace string
		}
		bySession := map[string][]managed{}
		byDirectory := map[string][]managed{}
		for i := range snapshot.Leases {
			if err := ctx.Err(); err != nil {
				return nil, totals, err
			}
			lease := &snapshot.Leases[i]
			if lease.Mode != state.ModeLong || lease.Depth != 0 {
				continue
			}
			cwd, workspace := canon(lease.Cwd), canon(lease.Workspace)
			if cwd == "" || workspace == "" || !transcript.IsFullID(lease.Provider, lease.SessionID) {
				totals.Known = false
				continue
			}
			launchID := navLeaseLaunchID(ctx, &state.NavigatorSnapshot{Launches: launches[lease.ID]}, lease, false)
			live, e := f.observeNavLease(ctx, snapshot, lease, launchID)
			if e != nil {
				return nil, totals, e
			}
			value := managed{lease, live, cwd, workspace}
			key := lease.Provider + "/" + lease.SessionID
			bySession[key] = append(bySession[key], value)
			byDirectory[cwd] = append(byDirectory[cwd], value)
		}
		for _, values := range bySession {
			if len(values) != 1 {
				totals.Known = false
				continue
			}
			value := values[0]
			if value.live == terminal.Unknown {
				totals.Known = false
				continue
			}
			if value.live == terminal.Running {
				totals.Live++
				if !q.all && selectedDir != "" && value.cwd != selectedDir {
					totals.Elsewhere++
				}
			}
		}
		for i, row := range rows {
			if row.Worker {
				out[i].Live = "unknown"
				continue
			}
			cwd := canon(row.Cwd)
			values := bySession[row.Provider+"/"+row.ID]
			if len(values) == 1 && cwd != "" && values[0].cwd == cwd {
				value := values[0]
				lease := value.lease
				activity := &out[i]
				activity.Live = "unknown"
				if value.live == terminal.Running {
					activity.Live = "live"
				} else if value.live == terminal.Ended {
					activity.Live = "ended"
				}
				activity.Managed = true
				activity.Workspace = value.workspace
				activity.Account = lease.AccountID
				activity.Launcher = lease.Launcher
				activity.Pane = lease.Pane
				activity.LeaseID = strconv.FormatInt(lease.ID, 10)
				activity.Started = strconv.FormatInt(lease.StartedAt, 10)
				activity.Drain = lease.Drain
				activity.DrainManual = lease.DrainManual
			} else if len(values) > 0 {
				out[i].Live = "unknown"
			}
			if !out[i].Managed && cwd != "" {
				candidates := byDirectory[cwd]
				if len(candidates) == 1 && candidates[0].live == terminal.Running {
					out[i].Workspace = candidates[0].workspace
					out[i].Continued = candidates[0].lease.Provider != row.Provider || candidates[0].lease.SessionID != row.ID
				}
			}
		}
	}
	open, known, err := navClaudeRegistrations(ctx, roots.ClaudeProjects, run, &entries, f.policy.sessions.arrayEntries, proc.Alive)
	if err != nil {
		return nil, totals, err
	}
	totals.ClaudeOpenKnown = known
	for i, row := range rows {
		if !row.Worker && !out[i].Managed && row.Provider == "claude" && canon(row.Cwd) != "" {
			if value, ok := open[row.ID]; ok && value.pid > 0 && !value.duplicate {
				out[i].Live = "open"
				out[i].PID = value.pid
			}
		}
		v := out[i]
		if !run.reserve(256, v.Live, v.Workspace, v.Account, v.Launcher, v.Pane, v.LeaseID, v.Started, v.Drain) {
			return nil, totals, errNavAuxTooLarge
		}
	}
	return out, totals, ctx.Err()
}
