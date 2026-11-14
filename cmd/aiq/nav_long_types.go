package main

import (
	"sort"
	"strconv"
	"strings"

	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

type navLongScope struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
}
type navLongLease struct {
	ID           string      `json:"id"`
	Started      string      `json:"started"`
	Provider     string      `json:"provider"`
	Account      string      `json:"account"`
	Launcher     string      `json:"launcher"`
	Pane         string      `json:"pane"`
	Live         string      `json:"live"`
	Drain        string      `json:"drain"`
	WorkingKnown bool        `json:"working_known"`
	Working      bool        `json:"working"`
	TurnStarted  navWireTime `json:"turn_started"`
}
type navLongSpan struct {
	Account  string      `json:"account"`
	Launcher string      `json:"launcher"`
	From     navWireTime `json:"from"`
	Until    navWireTime `json:"until"`
}
type navLongSession struct {
	Key                 string        `json:"key"`
	Provider            string        `json:"provider"`
	SessionID           string        `json:"session_id"`
	TranscriptAvailable bool          `json:"transcript_available"`
	WorkspaceKnown      bool          `json:"workspace_known"`
	Last                navWireTime   `json:"last"`
	Spans               []navLongSpan `json:"spans"`
}
type navLongWorkspace struct {
	Workspace string `json:"workspace"`
	Recorded  int    `json:"recorded_leases"`
	Sessions  int    `json:"sessions"`
}
type navLongBody struct {
	Build            string             `json:"build"`
	Kind             string             `json:"kind"`
	RequestedDir     string             `json:"requested_dir"`
	Workspace        string             `json:"workspace"`
	WorkspaceKnown   bool               `json:"workspace_known"`
	State            string             `json:"state"`
	Partial          bool               `json:"partial"`
	UnmappedLaunches int                `json:"unmapped_launches"`
	HistoryPresent   bool               `json:"history_present"`
	Observed         navWireTime        `json:"observed"`
	Lease            *navLongLease      `json:"lease"`
	Current          *navLongScope      `json:"current"`
	CurrentSpans     []navLongSpan      `json:"current_spans"`
	Earlier          []navLongSession   `json:"earlier"`
	Workspaces       []navLongWorkspace `json:"workspaces"`
	Note             navLongNote        `json:"note"`
}

type navLongLaunch struct {
	state.NavigatorLaunch
	available, workspaceKnown bool
}

func navLongTime(ms int64) navWireTime {
	if ms == 0 || ms < -navConversationSafeInteger || ms > navConversationSafeInteger {
		return navWireTime{}
	}
	return navWireTime{MS: ms, Known: true}
}
func navLongSeconds(seconds int64) navWireTime {
	if seconds <= 0 || seconds > navConversationSafeInteger/1000 {
		return navWireTime{}
	}
	return navLongTime(seconds * 1000)
}

// Group session identities across intervening launches, while merging account
// spans only across adjacent records of that same session. Missing IDs never
// collapse into one artificial session.
func navLongGroups(workspace string, records []navLongLaunch) []navLongSession {
	sort.Slice(records, func(i, j int) bool {
		a, b := records[i], records[j]
		if a.StartedMS != b.StartedMS {
			return a.StartedMS < b.StartedMS
		}
		return a.ID < b.ID
	})
	groups := map[string]*navLongSession{}
	previous := ""
	for i, row := range records {
		id := row.SessionID
		key := row.Provider + "/" + id
		if !transcript.IsFullID(row.Provider, id) {
			id = ""
			key = "unknown:" + strconv.FormatInt(row.ID, 10)
		}
		group := groups[key]
		if group == nil {
			group = &navLongSession{Key: navConversationKey("nav-long-session-v1", workspace, key), Provider: strings.Clone(row.Provider), SessionID: strings.Clone(id), WorkspaceKnown: true, Spans: []navLongSpan{}}
			groups[key] = group
		}
		group.WorkspaceKnown = group.WorkspaceKnown && row.workspaceKnown
		group.TranscriptAvailable = group.TranscriptAvailable || row.available
		group.Last = navLongTime(row.StartedMS)
		span := navLongSpan{Account: strings.Clone(row.AccountID), Launcher: strings.Clone(row.Launcher), From: navLongTime(row.StartedMS)}
		if i+1 < len(records) && records[i+1].StartedMS >= row.StartedMS {
			span.Until = navLongTime(records[i+1].StartedMS)
		}
		n := len(group.Spans)
		if previous == key && n > 0 && group.Spans[n-1].Account == span.Account && group.Spans[n-1].Launcher == span.Launcher &&
			group.Spans[n-1].Until.Known && span.From.Known && group.Spans[n-1].Until.MS == span.From.MS {
			group.Spans[n-1].Until = span.Until
		} else {
			group.Spans = append(group.Spans, span)
		}
		previous = key
	}
	out := make([]navLongSession, 0, len(groups))
	for _, group := range groups {
		out = append(out, *group)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Last.Known != b.Last.Known {
			return a.Last.Known
		}
		if a.Last.MS != b.Last.MS {
			return a.Last.MS > b.Last.MS
		}
		return a.Key < b.Key
	})
	return out
}
