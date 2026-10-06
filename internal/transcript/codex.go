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
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// Older CLIs log each prompt twice: as a response item and as a
	// user_message event. The event is the typed text alone, so it wins
	// when present.
	userEvents, err := contains(f, []byte(`"type":"user_message"`))
	if err != nil {
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	s := &Session{Provider: "codex", Path: path}
	var cur *Turn
	final, commentary := false, ""
	commentaryAt, replyAt := time.Time{}, time.Time{}
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
				// An answer that came only as an agent_message or in
				// task_complete has no item yet: it gets one here, before
				// the stretch that follows it.
				if !cur.answered(cur.Reply) {
					cur.addAnswer(cur.Reply, replyAt)
				}
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
			cur.Reply = commentary // an aborted turn: its last commentary stands
			cur.addAnswer(commentary, commentaryAt)
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
				cur.Items = append(cur.Items, TurnItem{Kind: ItemSteer, Text: prompt, At: ts})
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
	r := bufio.NewReaderSize(f, 1<<16)
	var buf []byte
	for {
		line, h, err := codexLine(r, &buf)
		if err != nil && err != io.EOF {
			return nil, err
		}
		if h.skip {
			// Only its time matters; see codexLine.
			ts := parseTime(h.ts)
			s.touch(ts)
			if cur != nil && !ts.IsZero() && (h.typ == "response_item" || h.typ == "event_msg") {
				cur.Ended = ts
			}
			if cur != nil && h.typ == "response_item" && h.payload == "reasoning" {
				work() // a task woken without a prompt is at work once it reasons
			}
		}
		if err == io.EOF && len(line) == 0 {
			break
		}
		if h.skip || len(bytes.TrimSpace(line)) == 0 {
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
		// codexHeadOf skips lines before they get here; a case added
		// below must also be kept there.
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
					commentary, commentaryAt = text, ts
					cur.Items = append(cur.Items, TurnItem{Kind: ItemSaid, Text: text, At: ts})
				} else if !final {
					cur.Reply, replyAt = text, ts
					ended = p.Phase == "final_answer"
					if ended {
						cur.addAnswer(text, ts)
					}
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
					cur.Reply, replyAt = strings.TrimSpace(p.Message), ts
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
					// A last message other than the final answer just sent
					// replaces it.
					if i := cur.lastAgent(); cur.Reply != "" && last != cur.Reply && i >= 0 &&
						cur.Items[i].Kind == ItemAnswer && cur.Items[i].Text == cur.Reply {
						cur.Items = append(cur.Items, TurnItem{Kind: ItemAnswer, Text: last, At: ts, Supersedes: true})
					}
					if cur.Reply != last {
						replyAt = ts
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

// codexHead is what the opening bytes of a rollout line say about it.
type codexHead struct {
	ts, typ, payload string
	skip             bool // the parser needs nothing from the line but its time
}

// codexLine reads one rollout line. Lines can run to many megabytes (tool
// output, reasoning, compaction history), and most of them matter only
// for their timestamp, so a line whose opening names a record the parser
// ignores is read past without being kept or decoded; it comes back
// empty with h.skip set. A skipped line must end in "}" and a newline, so a partial
// line at the end of a growing file is left out, as a failed decode
// would leave it out. Any other line comes back whole; one longer than
// the reader's buffer is gathered in *buf, which keeps its storage for the
// next.
func codexLine(r *bufio.Reader, buf *[]byte) (line []byte, h codexHead, err error) {
	line, err = r.ReadSlice('\n')
	if err != bufio.ErrBufferFull {
		if h = codexHeadOf(line); h.skip {
			t := bytes.TrimRight(line, " \t\r\n")
			if h.skip = len(t) > 0 && t[len(t)-1] == '}' && bytes.HasSuffix(line, []byte("\n")); h.skip {
				return nil, h, err
			}
		}
		return line, codexHead{}, err
	}
	if h = codexHeadOf(line); h.skip {
		last := byte(0)
		for err == bufio.ErrBufferFull {
			if t := bytes.TrimRight(line, " \t\r\n"); len(t) > 0 {
				last = t[len(t)-1]
			}
			line, err = r.ReadSlice('\n')
		}
		if t := bytes.TrimRight(line, " \t\r\n"); len(t) > 0 {
			last = t[len(t)-1]
		}
		h.skip = last == '}' && bytes.HasSuffix(line, []byte("\n"))
		return nil, h, err
	}
	b := append((*buf)[:0], line...)
	for err == bufio.ErrBufferFull {
		line, err = r.ReadSlice('\n')
		b = append(b, line...)
	}
	*buf = b
	return b, codexHead{}, err
}

// codexHeadOf reads the record and payload types and the time from a
// line's opening, `{"timestamp":"…","ordinal":N,"type":"…","payload":{"type":"…"`.
// A line in any other shape is not skipped.
func codexHeadOf(line []byte) (h codexHead) {
	rest, ok := bytes.CutPrefix(line, []byte(`{"timestamp":"`))
	if !ok {
		return h
	}
	if h.ts, rest, ok = cutString(rest); !ok {
		return h
	}
	if r, ok := bytes.CutPrefix(rest, []byte(`,"ordinal":`)); ok {
		rest = bytes.TrimLeft(r, "0123456789")
	}
	if rest, ok = bytes.CutPrefix(rest, []byte(`,"type":"`)); !ok {
		return h
	}
	if h.typ, rest, ok = cutString(rest); !ok {
		return h
	}
	if r, ok := bytes.CutPrefix(rest, []byte(`,"payload":{"type":"`)); ok {
		h.payload, _, _ = cutString(r)
	}
	// The types kept here must cover every case ParseCodex's main loop
	// handles: a type skipped here never reaches that switch.
	switch h.typ {
	case "session_meta", "turn_context":
	case "response_item":
		h.skip = h.payload != "" && h.payload != "message" && !strings.HasSuffix(h.payload, "_call")
	case "event_msg":
		switch h.payload {
		case "", "task_started", "task_complete", "turn_aborted", "user_message", "agent_message":
		default:
			h.skip = true
		}
	default:
		h.skip = true
	}
	return h
}

// cutString reads a JSON string body up to its closing quote. One with
// escapes is refused rather than decoded.
func cutString(b []byte) (string, []byte, bool) {
	i := bytes.IndexAny(b, `"\`)
	if i < 0 || b[i] != '"' {
		return "", b, false
	}
	return string(b[:i]), b[i+1:], true
}

// contains reports whether pat occurs anywhere in r, reading it in chunks.
func contains(r io.Reader, pat []byte) (bool, error) {
	buf := make([]byte, 1<<20)
	keep := 0
	for {
		n, err := r.Read(buf[keep:])
		n += keep
		if bytes.Contains(buf[:n], pat) {
			return true, nil
		}
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		keep = min(len(pat)-1, n)
		copy(buf, buf[n-keep:n])
	}
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
