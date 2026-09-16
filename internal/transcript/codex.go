package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// codexFiles lists the rollouts whose session_meta names dir. Only the
// first line of each file is read here.
func codexFiles(roots []string, dir string) []found {
	var all []string
	for _, root := range roots {
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if !d.IsDir() && strings.HasPrefix(d.Name(), "rollout-") && strings.HasSuffix(d.Name(), ".jsonl") {
				all = append(all, p)
			}
			return nil
		})
	}
	match := make([]bool, len(all))
	parallel(len(all), func(i int) {
		meta, ok := codexMeta(all[i])
		match[i] = ok && canonical(meta.Cwd) == dir
	})
	var out []found
	for i, p := range all {
		if match[i] {
			out = append(out, found{"codex", p})
		}
	}
	return out
}

func codexMeta(path string) (codexPayload, bool) {
	f, err := os.Open(path)
	if err != nil {
		return codexPayload{}, false
	}
	defer f.Close()
	line, err := bufio.NewReaderSize(f, 1<<16).ReadBytes('\n')
	if err != nil && err != io.EOF {
		return codexPayload{}, false
	}
	if !bytes.Contains(line, []byte(`"session_meta"`)) {
		return codexPayload{}, false
	}
	var rec codexRecord
	if json.Unmarshal(line, &rec) != nil || rec.Type != "session_meta" {
		return codexPayload{}, false
	}
	var p codexPayload
	json.Unmarshal(rec.Payload, &p)
	return p, p.Cwd != ""
}

type codexRecord struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type codexPayload struct {
	Type string `json:"type"`

	// session_meta
	ID         string          `json:"id"`
	Cwd        string          `json:"cwd"`
	Source     json.RawMessage `json:"source"`
	Originator string          `json:"originator"`
	Timestamp  string          `json:"timestamp"`
	Git        *struct {
		Branch string `json:"branch"`
	} `json:"git"`

	// turn_context
	Model          string          `json:"model"`
	ApprovalPolicy string          `json:"approval_policy"`
	SandboxPolicy  json.RawMessage `json:"sandbox_policy"`

	// response_item message
	Role    string        `json:"role"`
	Phase   string        `json:"phase"`
	Content []contentPart `json:"content"`

	// event_msg
	Message          string  `json:"message"`
	LastAgentMessage *string `json:"last_agent_message"`
}

// ParseCodex reads one Codex rollout. A subagent's rollout returns nil: its
// work shows up in the parent session.
func ParseCodex(path string) (*Session, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// Older CLIs log each prompt twice: as a response item and as a
	// user_message event. The event is the typed text alone, so it wins
	// when present.
	userEvents := bytes.Contains(data, []byte(`"type":"user_message"`))

	s := &Session{Provider: "codex", Path: path}
	var cur *Turn
	final, commentary := false, ""
	flush := func() {
		if cur == nil {
			return
		}
		if cur.Reply == "" {
			cur.Reply = commentary
		}
		s.Turns = append(s.Turns, *cur)
		cur, final, commentary = nil, false, ""
	}
	start := func(prompt string, ts time.Time) {
		flush()
		cur = &Turn{Prompt: prompt, Started: ts, Ended: ts}
	}

	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec codexRecord
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		var p codexPayload
		json.Unmarshal(rec.Payload, &p)
		ts := parseTime(rec.Timestamp)
		s.touch(ts)
		switch rec.Type {
		case "session_meta":
			if s.ID != "" {
				continue // a forked rollout repeats its parent's meta
			}
			if isSubagent(p.Source) {
				return nil, nil
			}
			s.ID, s.Cwd = p.ID, p.Cwd
			s.touch(parseTime(p.Timestamp))
			if p.Git != nil {
				s.Branch = p.Git.Branch
			}
			var source string
			json.Unmarshal(p.Source, &source)
			s.Worker = source == "exec" || strings.HasSuffix(p.Originator, "_exec")
		case "turn_context":
			if p.Model != "" {
				s.Model = p.Model
			}
			if p.ApprovalPolicy != "" {
				s.Bypass = p.ApprovalPolicy == "never" && bytes.Contains(p.SandboxPolicy, []byte("danger-full-access"))
			}
		case "response_item":
			switch {
			case p.Type == "message" && p.Role == "user" && !userEvents:
				var texts []string
				for _, c := range p.Content {
					if c.Type == "input_text" && !injected(c.Text) {
						texts = append(texts, strings.TrimSpace(c.Text))
					}
				}
				if len(texts) > 0 {
					start(strings.Join(texts, "\n"), ts)
				}
			case p.Type == "message" && p.Role == "assistant" && cur != nil:
				var texts []string
				for _, c := range p.Content {
					if c.Type == "output_text" && strings.TrimSpace(c.Text) != "" {
						texts = append(texts, strings.TrimSpace(c.Text))
					}
				}
				if len(texts) == 0 {
					break
				}
				text := strings.Join(texts, "\n")
				if p.Phase == "commentary" {
					commentary = text
				} else if !final {
					cur.Reply = text
				}
			case strings.HasSuffix(p.Type, "_call") && cur != nil:
				cur.Tools++
			}
		case "event_msg":
			switch p.Type {
			case "user_message":
				if userEvents && !injected(p.Message) {
					start(strings.TrimSpace(p.Message), ts)
				}
			case "agent_message":
				if cur != nil && !final && strings.TrimSpace(p.Message) != "" {
					cur.Reply = strings.TrimSpace(p.Message)
				}
			case "task_complete":
				if cur != nil && p.LastAgentMessage != nil && strings.TrimSpace(*p.LastAgentMessage) != "" {
					cur.Reply = strings.TrimSpace(*p.LastAgentMessage)
					final = true
				}
			}
		}
		// Records of the turn's own work move its end; the next turn's
		// setup (task_started, turn_context) does not. A record that opened
		// a turn has already set that turn's times.
		if cur != nil && !ts.IsZero() && (rec.Type == "response_item" || rec.Type == "event_msg" && p.Type != "task_started") {
			cur.Ended = ts
		}
	}
	flush()
	if s.ID == "" {
		return nil, nil
	}
	return s, nil
}

func isSubagent(source json.RawMessage) bool {
	return len(source) > 0 && source[0] == '{' && bytes.Contains(source, []byte(`"subagent"`))
}

// codexNames maps thread ids to the names given with /rename. Later lines
// win.
func codexNames(files []string) map[string]string {
	names := map[string]string{}
	for _, file := range files {
		f, err := os.Open(file)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			var e struct {
				ID         string `json:"id"`
				ThreadName string `json:"thread_name"`
			}
			if json.Unmarshal(sc.Bytes(), &e) == nil && e.ID != "" && e.ThreadName != "" {
				names[e.ID] = e.ThreadName
			}
		}
		f.Close()
	}
	return names
}
