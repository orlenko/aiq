package transcript

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// The Antigravity CLI keeps one directory per conversation under
// <app data>/brain/<id>/, with the transcript at
// .system_generated/logs/transcript.jsonl: one step per line (USER_INPUT,
// PLANNER_RESPONSE, GENERIC tool output, SYSTEM_MESSAGE). The transcript
// does not name its working directory; history.jsonl in the app data dir
// records each typed prompt with its workspace and conversation id, and
// cache/last_conversations.json maps a workspace to its latest conversation.

// agyFiles lists the transcripts of conversations that ran in dir, until
// ctx ends.
func agyFiles(ctx context.Context, roots []string, dir string) []Candidate {
	var out []Candidate
	seen := map[string]bool{}
	for _, root := range roots {
		if ctx.Err() != nil {
			return out
		}
		for _, id := range agyConversations(ctx, root, dir) {
			p := AgyTranscript(root, id)
			if seen[p] {
				continue
			}
			if _, err := os.Stat(p); err != nil {
				continue
			}
			seen[p] = true
			out = append(out, Candidate{Provider: "agy", Path: p})
		}
	}
	return out
}

// AgyTranscript is the transcript path of a conversation under an app data dir.
func AgyTranscript(appData, id string) string {
	return filepath.Join(appData, "brain", id, ".system_generated", "logs", "transcript.jsonl")
}

// agyConversations reads the ids of the conversations recorded for dir.
func agyConversations(ctx context.Context, appData, dir string) []string {
	return agyWorkspaces(ctx, appData)[dir]
}

// agyWorkspaces maps each workspace recorded under an app data dir
// (canonical) to the ids of its conversations: those its typed prompts name,
// in order, then its latest conversation.
func agyWorkspaces(ctx context.Context, appData string) map[string][]string {
	return agyWorkspaceMap(ctx, appData, false)
}

func agyAbsoluteWorkspaces(ctx context.Context, appData string) map[string][]string {
	return agyWorkspaceMap(ctx, appData, true)
}

func agyWorkspaceMap(ctx context.Context, appData string, absoluteOnly bool) map[string][]string {
	ids := map[string][]string{}
	seen := map[string]bool{}
	canon := map[string]string{}
	// An id names a folder under brain/, so only a well-formed one counts: a
	// path or a pattern would name some other file.
	add := func(ws, id string) {
		if absoluteOnly && !filepath.IsAbs(ws) {
			return
		}
		dir, ok := canon[ws]
		if !ok {
			dir = canonical(ws)
			canon[ws] = dir
		}
		if ValidID(id) && !seen[dir+"\x00"+id] {
			seen[dir+"\x00"+id] = true
			ids[dir] = append(ids[dir], id)
		}
	}
	if f, err := OpenRegular(filepath.Join(appData, "history.jsonl")); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 4<<20)
		for ctx.Err() == nil && sc.Scan() {
			var e struct {
				ConversationID string `json:"conversationId"`
				Workspace      string `json:"workspace"`
			}
			if json.Unmarshal(sc.Bytes(), &e) == nil && e.Workspace != "" {
				add(e.Workspace, e.ConversationID)
			}
		}
		f.Close()
	}
	if data, err := ReadRegular(filepath.Join(appData, "cache", "last_conversations.json")); err == nil {
		var last map[string]string
		if json.Unmarshal(data, &last) == nil {
			for ws, id := range last {
				add(ws, id)
			}
		}
	}
	return ids
}

// agyByID is the transcript of conversation id under each app data dir
// that has one.
func agyByID(r Roots, id string) []Candidate {
	var out []Candidate
	for _, root := range r.AgyAppData {
		p := AgyTranscript(root, id)
		if _, err := os.Stat(p); err == nil {
			out = append(out, Candidate{Provider: "agy", Path: p, Root: root, Account: r.Labels[root]})
		}
	}
	return out
}

type agyStep struct {
	Type      string          `json:"type"`
	Source    string          `json:"source"`
	Status    string          `json:"status"`
	CreatedAt string          `json:"created_at"`
	Content   string          `json:"content"`
	ToolCalls json.RawMessage `json:"tool_calls"`
}

var userRequest = regexp.MustCompile(`(?s)<USER_REQUEST>\s*(.*?)\s*</USER_REQUEST>`)

// ParseAgy reads one Antigravity CLI transcript.
func ParseAgy(path string) (*Session, error) {
	f, err := OpenRegular(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseAgyReader(path, f, func() time.Time { return descriptorMtime(f.Stat) })
}

func parseAgyReader(path string, f io.Reader, fallback func() time.Time) (*Session, error) {
	// brain/<id>/.system_generated/logs/transcript.jsonl
	id := filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(path))))
	s := &Session{Provider: "agy", ID: id, Path: path}

	var cur *Turn
	flush := func() {
		if cur != nil {
			s.Turns = append(s.Turns, *cur)
			cur = nil
		}
	}
	r := bufio.NewReaderSize(f, 1<<16)
	var buf []byte
	for {
		line, over, err := readLine(r, &buf)
		switch {
		case over && ownString(line, "type") == "USER_INPUT":
			ts := parseTime(openingString(line, "created_at"))
			s.touch(ts)
			flush()
			t := skippedPrompt(ts)
			cur = &t
		case over:
			s.skipRecord(cur)
		case len(line) > 0:
			var step agyStep
			if json.Unmarshal(line, &step) == nil {
				ts := parseTime(step.CreatedAt)
				s.touch(ts)
				switch step.Type {
				case "USER_INPUT":
					prompt := step.Content
					if m := userRequest.FindStringSubmatch(prompt); m != nil {
						prompt = m[1]
					}
					prompt = strings.TrimSpace(prompt)
					if prompt == "" || injected(prompt) {
						break
					}
					flush()
					cur = &Turn{Prompt: prompt, Source: prefixSource(prompt), Started: ts, Ended: ts}
				case "PLANNER_RESPONSE":
					if cur == nil {
						break
					}
					var calls []json.RawMessage
					json.Unmarshal(step.ToolCalls, &calls)
					cur.settle(ts)
					cur.Tools += len(calls)
					if text := strings.TrimSpace(step.Content); text != "" {
						cur.Reply, cur.ReplyAt = text, ts
					}
					if !ts.IsZero() {
						cur.Ended = ts
					}
				case "GENERIC":
					if cur != nil && !ts.IsZero() {
						cur.settle(ts)
						cur.Ended = ts
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	flush()
	if s.Updated.IsZero() {
		s.Updated = fallback()
	}
	return s, nil
}
