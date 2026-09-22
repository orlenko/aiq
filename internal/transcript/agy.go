package transcript

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The Antigravity CLI keeps one directory per conversation under
// <app data>/brain/<id>/, with the transcript at
// .system_generated/logs/transcript.jsonl: one step per line (USER_INPUT,
// PLANNER_RESPONSE, GENERIC tool output, SYSTEM_MESSAGE). The transcript
// does not name its working directory; history.jsonl in the app data dir
// records each typed prompt with its workspace and conversation id, and
// cache/last_conversations.json maps a workspace to its latest conversation.

// agyFiles lists the transcripts of conversations that ran in dir.
func agyFiles(roots []string, dir string) []found {
	var out []found
	seen := map[string]bool{}
	for _, root := range roots {
		for _, id := range agyConversations(root, dir) {
			p := AgyTranscript(root, id)
			if seen[p] {
				continue
			}
			if _, err := os.Stat(p); err != nil {
				continue
			}
			seen[p] = true
			out = append(out, found{"agy", p})
		}
	}
	return out
}

// AgyTranscript is the transcript path of a conversation under an app data dir.
func AgyTranscript(appData, id string) string {
	return filepath.Join(appData, "brain", id, ".system_generated", "logs", "transcript.jsonl")
}

// agyConversations reads the ids of the conversations recorded for dir.
func agyConversations(appData, dir string) []string {
	var ids []string
	seen := map[string]bool{}
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if f, err := os.Open(filepath.Join(appData, "history.jsonl")); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 4<<20)
		for sc.Scan() {
			var e struct {
				ConversationID string `json:"conversationId"`
				Workspace      string `json:"workspace"`
			}
			if json.Unmarshal(sc.Bytes(), &e) == nil && e.Workspace != "" && canonical(e.Workspace) == dir {
				add(e.ConversationID)
			}
		}
		f.Close()
	}
	if data, err := os.ReadFile(filepath.Join(appData, "cache", "last_conversations.json")); err == nil {
		var last map[string]string
		if json.Unmarshal(data, &last) == nil {
			for ws, id := range last {
				if canonical(ws) == dir {
					add(id)
				}
			}
		}
	}
	return ids
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
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
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
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
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
					cur = &Turn{Prompt: prompt, Started: ts, Ended: ts}
				case "PLANNER_RESPONSE":
					if cur == nil {
						break
					}
					var calls []json.RawMessage
					json.Unmarshal(step.ToolCalls, &calls)
					cur.Tools += len(calls)
					if text := strings.TrimSpace(step.Content); text != "" {
						cur.Reply = text
					}
					if !ts.IsZero() {
						cur.Ended = ts
					}
				case "GENERIC":
					if cur != nil && !ts.IsZero() {
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
		if info, err := f.Stat(); err == nil {
			s.Updated = info.ModTime()
		}
	}
	return s, nil
}
