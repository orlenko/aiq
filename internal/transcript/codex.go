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

	// response_item message metadata (CLI 0.149 and later)
	Meta *struct {
		TurnID string   `json:"turn_id"`
		Kinds  []string `json:"content_item_kinds"`
	} `json:"internal_chat_message_metadata_passthrough"`

	// event_msg
	Message          string  `json:"message"`
	LastAgentMessage *string `json:"last_agent_message"`
	TurnID           string  `json:"turn_id"`
}

// ParseCodex reads one Codex rollout. A subagent's rollout returns nil: its
// work shows up in the parent session.
//
// Each task (task_started … task_complete) normally answers one prompt.
// Input that joins a running task (same turn_id, or within the task's
// bounds when messages carry no turn_id) is a steer; a task that starts
// with no prompt of its own (a background wakeup) is a later stretch of
// the turn before it.
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
	// curTurn is the turn_id cur answers; running says a task is under
	// way, and fresh that it has not yet been given to a turn. ended says
	// cur's current stretch gave its final answer.
	curTurn, running, fresh, ended := "", false, false, false
	// work marks output of the turn; after a final answer it opens a new
	// stretch, and the answer so far moves to Earlier.
	work := func() {
		if fresh || ended {
			if cur.Reply != "" {
				cur.Earlier = append(cur.Earlier, cur.Reply)
			}
			cur.Reply, final, commentary = "", false, ""
			fresh, ended = false, false
		}
	}
	flush := func(last bool) {
		if cur == nil {
			return
		}
		// A task that has started but not yet been given its prompt, or
		// whose final answer is in, leaves the turn closed.
		cur.Open = last && running && !fresh && !ended
		switch {
		case cur.Open:
			cur.Reply = ""
		case cur.Reply == "" && commentary != "":
			cur.Reply = commentary
		case cur.Reply == "" && len(cur.Earlier) > 0:
			cur.Reply = cur.Earlier[len(cur.Earlier)-1]
			cur.Earlier = cur.Earlier[:len(cur.Earlier)-1]
		}
		s.Turns = append(s.Turns, *cur)
		cur, final, commentary = nil, false, ""
	}
	input := func(prompt, turnID string, ts time.Time) {
		src := prefixSource(prompt)
		joins := cur != nil && (turnID != "" && turnID == curTurn || turnID == "" && running && !fresh)
		if joins {
			if src == Human {
				cur.Steers = append(cur.Steers, prompt)
			}
			return
		}
		if src == Peer {
			prompt = peerBody(prompt)
		}
		flush(false)
		cur = &Turn{Prompt: prompt, Source: src, Started: ts, Ended: ts}
		curTurn, fresh, ended = turnID, false, false
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
				if text := codexUserText(p); text != "" {
					turnID := ""
					if p.Meta != nil {
						turnID = p.Meta.TurnID
					}
					input(text, turnID, ts)
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
				work()
				text := strings.Join(texts, "\n")
				if p.Phase == "commentary" {
					commentary = text
				} else if !final {
					cur.Reply = text
					ended = p.Phase == "final_answer"
				}
			case strings.HasSuffix(p.Type, "_call") && cur != nil:
				work()
				cur.Tools++
			}
		case "event_msg":
			switch p.Type {
			case "task_started":
				running, fresh = true, true
			case "task_complete", "turn_aborted":
				running = false
			}
			switch p.Type {
			case "user_message":
				if userEvents && !injected(p.Message) {
					input(strings.TrimSpace(p.Message), p.TurnID, ts)
				}
			case "agent_message":
				if cur != nil && !final && !ended && strings.TrimSpace(p.Message) != "" {
					work()
					cur.Reply = strings.TrimSpace(p.Message)
				}
			case "task_complete":
				if cur != nil && p.LastAgentMessage != nil && strings.TrimSpace(*p.LastAgentMessage) != "" {
					if fresh {
						work()
					}
					last := strings.TrimSpace(*p.LastAgentMessage)
					if n := len(cur.Earlier); cur.Reply == "" && n > 0 && cur.Earlier[n-1] == last {
						cur.Earlier = cur.Earlier[:n-1] // the stretch said nothing new
					}
					cur.Reply = last
					final, ended = true, true
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
	flush(true)
	if s.ID == "" {
		return nil, nil
	}
	return s, nil
}

// codexUserText is what the person wrote in a user message. CLIs that tag
// each content item with its kind say which items are the user's own;
// older ones are judged by how the text opens.
func codexUserText(p codexPayload) string {
	kinds := []string(nil)
	if p.Meta != nil && len(p.Meta.Kinds) == len(p.Content) {
		kinds = p.Meta.Kinds
	}
	var texts []string
	for i, c := range p.Content {
		if c.Type != "input_text" {
			continue
		}
		t := strings.TrimSpace(c.Text)
		if kinds != nil {
			// Image placeholders are tagged as user text too.
			if kinds[i] != "user.text" || t == "" || strings.HasPrefix(t, "<image") || strings.HasPrefix(t, "</image") {
				continue
			}
		} else if injected(c.Text) {
			continue
		}
		texts = append(texts, t)
	}
	return strings.Join(texts, "\n")
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
