// Package fleet reads the long and interactive sessions running on the owner's
// other machines, so one machine's Quotas page can show every session on each
// shared subscription.
//
// Each machine answers `aiq leases --json` (Collect builds the reply). The
// viewing daemon runs that command over SSH on the hosts listed in [fleet],
// with the owner's own keys, and keeps the last good reply per host. Nothing
// listens on any machine, and the reply carries no launch arguments,
// transcripts, credentials or account homes.
package fleet

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/orlenko/aiq/internal/state"
)

// Schema is the version of the `aiq leases --json` document.
const Schema = 1

// Limits on one reply: a reply over them is refused whole.
const (
	MaxReplyBytes = 1 << 20
	MaxLeases     = 500
	MaxTextBytes  = 4096
)

// Doc is the `aiq leases --json` document.
type Doc struct {
	Schema      int     `json:"schema"`
	GeneratedAt int64   `json:"generated_at"`
	Leases      []Lease `json:"leases"`
}

// Lease is one live long or interactive session on the answering machine.
// Account, Provider and Identity let the viewer find the same subscription
// among its own accounts.
type Lease struct {
	Account   string `json:"account"`
	Provider  string `json:"provider"`
	Identity  string `json:"identity"`
	Mode      string `json:"mode"` // long or interactive
	Drain     string `json:"drain"`
	Depth     int    `json:"depth"`
	Pane      string `json:"pane"`
	PID       int    `json:"pid"`
	Workspace string `json:"workspace"`
	Cwd       string `json:"cwd"`
	Why       string `json:"why"`
	StartedAt int64  `json:"started_at"`
}

// Collect builds this machine's reply from a quota snapshot: leases recorded
// by this machine (host is its lease identity) whose process is alive, in long
// or interactive mode. Workers are short and numerous, so they stay local.
func Collect(snap *state.QuotaSnapshot, host string, alive func(int) bool, now int64) Doc {
	doc := Doc{Schema: Schema, GeneratedAt: now, Leases: []Lease{}}
	if snap == nil {
		return doc
	}
	accounts := make(map[string]state.Account, len(snap.Accounts))
	for _, a := range snap.Accounts {
		accounts[a.ID] = a
	}
	for _, l := range snap.Leases {
		if l.Hostname != host || (l.Mode != state.ModeLong && l.Mode != state.ModeInteractive) || !alive(l.PID) {
			continue
		}
		if len(doc.Leases) == MaxLeases {
			break
		}
		a := accounts[l.AccountID]
		provider := a.Provider
		if provider == "" {
			provider = l.Provider
		}
		doc.Leases = append(doc.Leases, Lease{Account: l.AccountID, Provider: provider, Identity: a.Identity,
			Mode: l.Mode, Drain: l.Drain, Depth: l.Depth, Pane: l.Pane, PID: l.PID, Workspace: l.Workspace, Cwd: l.Cwd,
			Why: snap.Whys[state.LeaseRef{ID: l.ID, StartedAt: l.StartedAt}], StartedAt: l.StartedAt})
	}
	return doc
}

// Decode admits one reply. Anything outside the limits, an unknown schema, a
// mode other than long or interactive, or a text field with control
// characters (other than a why's line breaks) refuses the whole reply.
func Decode(data []byte) (Doc, error) {
	var doc Doc
	if len(data) > MaxReplyBytes {
		return doc, errors.New("reply too large")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&doc); err != nil {
		return Doc{}, errors.New("reply is not a leases document")
	}
	if dec.More() {
		return Doc{}, errors.New("reply has trailing data")
	}
	if doc.Schema != Schema {
		return Doc{}, fmt.Errorf("reply schema %d, expected %d: update aiq on that machine", doc.Schema, Schema)
	}
	if len(doc.Leases) > MaxLeases {
		return Doc{}, errors.New("reply lists too many sessions")
	}
	if doc.Leases == nil {
		doc.Leases = []Lease{}
	}
	for _, l := range doc.Leases {
		if l.Mode != state.ModeLong && l.Mode != state.ModeInteractive {
			return Doc{}, errors.New("reply has an unknown session mode")
		}
		if l.Account == "" || l.Provider == "" || l.PID < 0 || l.Depth < 0 || l.StartedAt < 0 {
			return Doc{}, errors.New("reply has an incomplete session")
		}
		for _, s := range []string{l.Account, l.Provider, l.Identity, l.Drain, l.Pane, l.Workspace, l.Cwd} {
			if !text(s, false) {
				return Doc{}, errors.New("reply has unreadable text")
			}
		}
		if !text(l.Why, true) {
			return Doc{}, errors.New("reply has unreadable text")
		}
	}
	return doc, nil
}

func text(s string, lines bool) bool {
	if len(s) > MaxTextBytes || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if lines && r == '\n' {
			continue
		}
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return false
		}
	}
	return true
}

// errorLine is the first non-empty line of a command's stderr, without
// control characters, at most 200 runes: what a person can act on, such as
// ssh's "Permission denied (publickey)".
func errorLine(stderr []byte) string {
	for _, line := range strings.Split(string(stderr), "\n") {
		line = strings.Map(func(r rune) rune {
			if unicode.IsControl(r) || r == utf8.RuneError {
				return -1
			}
			return r
		}, line)
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if utf8.RuneCountInString(line) > 200 {
			line = string([]rune(line)[:200]) + "…"
		}
		return line
	}
	return ""
}
