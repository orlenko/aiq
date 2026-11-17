package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"time"

	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

// Saved names are optional inbox metadata. Read only recent metadata records,
// once per conversation, with a shared byte/time allowance. Conversation bodies
// are neither decoded nor returned, and a missing name never suppresses an ask.
func (env appEnv) askNames(ctx context.Context, asks []state.Ask) map[string]appSessionName {
	out := map[string]appSessionName{}
	if env.input.roots == nil {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	roots, err := env.input.roots(ctx)
	if err != nil {
		return out
	}
	const maxNames = 128
	wanted := map[string]bool{}
	for _, a := range asks {
		if (a.Provider == "claude" || a.Provider == "codex") && transcript.IsFullID(a.Provider, a.SessionID) {
			key := a.Provider + "/" + a.SessionID
			if len(wanted) < maxNames || wanted[key] {
				wanted[key] = true
			}
		}
	}
	remaining := int64(64 << 20)
	// Codex's small name indexes avoid opening individual rollouts.
	codex := map[string]bool{}
	for key := range wanted {
		if len(key) > 6 && key[:6] == "codex/" {
			codex[key[6:]] = true
		}
	}
	if len(codex) > 0 {
		for _, path := range roots.CodexIndex {
			seen := map[string]bool{}
			appNameRecords(ctx, path, &remaining, func(line []byte) bool {
				var r struct {
					ID   string `json:"id"`
					Name string `json:"thread_name"`
				}
				if json.Unmarshal(line, &r) == nil && codex[r.ID] && !seen[r.ID] && r.Name != "" {
					seen[r.ID] = true
					out["codex/"+r.ID] = appSessionName{Name: appHistoryLabel(r.Name), Source: "saved"}
				}
				return false
			})
		}
	}
	src := transcript.DirectWalk(roots)
	seen := map[string]bool{}
	for _, a := range asks {
		key := a.Provider + "/" + a.SessionID
		if a.Provider != "claude" || !wanted[key] || seen[key] || ctx.Err() != nil || remaining <= 0 {
			continue
		}
		seen[key] = true
		candidates, err := src.ByID(ctx, "claude", a.SessionID)
		if err != nil {
			continue
		}
		var best string
		var info os.FileInfo
		ambiguous := false
		for _, c := range candidates {
			if ctx.Err() != nil {
				break
			}
			f, err := transcript.OpenRegular(c.Path)
			if err != nil {
				continue
			}
			next, err := f.Stat()
			f.Close()
			if err != nil {
				continue
			}
			if info == nil || next.ModTime().After(info.ModTime()) {
				best, info, ambiguous = c.Path, next, false
			} else if next.ModTime().Equal(info.ModTime()) && !os.SameFile(info, next) {
				ambiguous = true
			}
		}
		if best == "" || ambiguous {
			continue
		}
		appNameRecords(ctx, best, &remaining, func(line []byte) bool {
			if !bytes.Contains(line, []byte(`"custom-title"`)) {
				return false
			}
			var r struct {
				Type    string `json:"type"`
				Title   string `json:"customTitle"`
				Session string `json:"sessionId"`
			}
			if json.Unmarshal(line, &r) != nil || r.Type != "custom-title" || r.Session != "" && r.Session != a.SessionID {
				return false
			}
			out[key] = appSessionName{Name: appHistoryLabel(r.Title), Source: "saved"}
			return true // an explicit empty saved title also supersedes an older name
		})
	}
	return out
}

// Scan complete lines backwards through a bounded suffix. A record crossing
// the suffix boundary is ignored, as are oversized metadata records. This
// deliberately does not turn a large transcript into a full parse on each poll.
func appNameRecords(ctx context.Context, path string, remaining *int64, visit func([]byte) bool) {
	if ctx.Err() != nil || *remaining <= 0 {
		return
	}
	f, err := transcript.OpenRegular(path)
	if err != nil {
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return
	}
	n := min(info.Size(), 2<<20, *remaining)
	if n <= 0 {
		return
	}
	*remaining -= n
	offset := info.Size() - n
	data := make([]byte, int(n))
	read, err := f.ReadAt(data, offset)
	if err != nil && err != io.EOF {
		return
	}
	data = data[:read]
	if offset > 0 {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		} else {
			return
		}
	}
	for len(data) > 0 && ctx.Err() == nil {
		i := bytes.LastIndexByte(data, '\n')
		line := data[i+1:]
		if len(line) > 0 && len(line) <= 65536 && visit(line) {
			return
		}
		if i < 0 {
			return
		}
		data = data[:i]
	}
}
