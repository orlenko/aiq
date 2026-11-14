package transcript

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var nonAlnum = regexp.MustCompile(`[^a-zA-Z0-9]`)

// claudeMaxDirName is where Claude Code truncates an encoded project path
// and appends a hash.
const claudeMaxDirName = 200

// claudeFiles lists the top-level transcripts of dir's project directory
// under each root, until ctx ends. Subagent transcripts live one level
// deeper and are left out.
func claudeFiles(ctx context.Context, roots []string, dir string) []Candidate {
	// dir's own name, and the working directory's when it is dir by another
	// path, in a fixed order so the candidates come in one.
	encodings := []string{nonAlnum.ReplaceAllString(dir, "-")}
	if wd, err := os.Getwd(); err == nil && canonical(wd) == dir {
		if enc := nonAlnum.ReplaceAllString(filepath.Clean(wd), "-"); enc != encodings[0] {
			encodings = append(encodings, enc)
		}
	}
	sort.Strings(encodings)
	var out []Candidate
	seen := map[string]bool{}
	for _, root := range roots {
		for _, enc := range encodings {
			if ctx.Err() != nil {
				return out
			}
			var projects []string
			if len(enc) <= claudeMaxDirName {
				projects = []string{filepath.Join(root, enc)}
			} else {
				entries, _ := os.ReadDir(root)
				for _, e := range entries {
					if strings.HasPrefix(e.Name(), enc[:claudeMaxDirName]) {
						projects = append(projects, filepath.Join(root, e.Name()))
					}
				}
			}
			for _, p := range projects {
				files, _ := filepath.Glob(filepath.Join(p, "*.jsonl"))
				for _, f := range files {
					if !seen[f] {
						seen[f] = true
						out = append(out, Candidate{Provider: "claude", Path: f})
					}
				}
			}
		}
	}
	return out
}

// claudeByID is <id>.jsonl in each project directory of each root, until
// ctx ends.
func claudeByID(ctx context.Context, r Roots, id string) []Candidate {
	var out []Candidate
	for _, root := range r.ClaudeProjects {
		entries, _ := os.ReadDir(root)
		for _, e := range entries {
			if ctx.Err() != nil {
				return out
			}
			p := filepath.Join(root, e.Name(), id+".jsonl")
			if _, err := os.Stat(p); err == nil {
				out = append(out, Candidate{Provider: "claude", Path: p, Root: root, Account: r.Labels[root]})
			}
		}
	}
	return out
}

type claudeRecord struct {
	Type             string        `json:"type"`
	IsMeta           bool          `json:"isMeta"`
	IsSidechain      bool          `json:"isSidechain"`
	IsCompactSummary bool          `json:"isCompactSummary"`
	Cwd              string        `json:"cwd"`
	SessionID        string        `json:"sessionId"`
	Timestamp        string        `json:"timestamp"`
	Entrypoint       string        `json:"entrypoint"`
	GitBranch        string        `json:"gitBranch"`
	CustomTitle      string        `json:"customTitle"`
	PermissionMode   string        `json:"permissionMode"`
	PromptSource     string        `json:"promptSource"`
	Origin           *claudeOrigin `json:"origin"`
	Message          *struct {
		ID         string          `json:"id"`
		Model      string          `json:"model"`
		StopReason string          `json:"stop_reason"`
		Content    json.RawMessage `json:"content"`
	} `json:"message"`
	Attachment *struct {
		Type        string          `json:"type"`
		Prompt      json.RawMessage `json:"prompt"`
		CommandMode string          `json:"commandMode"`
		Origin      *claudeOrigin   `json:"origin"`
		IsMeta      bool            `json:"isMeta"`
	} `json:"attachment"`
}

type claudeOrigin struct {
	Kind string `json:"kind"`
}

type contentPart struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
}

// narration reports whether a thinking block is text Claude Code shows the
// person. Newer models write some of what they say to the person as a
// thinking block; its signature, a protobuf, names its kind: "narration"
// rather than "thinking". The kind sits in the first bytes.
func narration(p contentPart) bool {
	if p.Type != "thinking" || p.Thinking == "" || len(p.Signature) < 64 {
		return false
	}
	head, err := base64.StdEncoding.DecodeString(p.Signature[:64])
	return err == nil && bytes.Contains(head, []byte("\x42\x09narration"))
}

// claudeTypes are the record types the parser reads; the rest (file
// snapshots, other attachments, progress) can be large and are skipped
// unparsed.
var claudeTypes = [][]byte{
	[]byte(`"type":"user"`), []byte(`"type":"assistant"`), []byte(`"type":"custom-title"`), []byte(`"type":"permission-mode"`),
	[]byte(`"type":"queued_command"`),
}

// ParseClaude reads one Claude Code session transcript.
//
// A turn opens with a human prompt, or with an aiq, peer or notifier
// message that arrives while no turn is open. Everything else (tool
// results, background task notifications, a message sent mid-turn) is
// part of the turn it arrives in. An assistant message that stops for any
// reason but a tool call ends a stretch of the turn; when the agent goes on
// after that, its earlier answer moves to Earlier.
func ParseClaude(path string) (*Session, error) {
	f, err := OpenRegular(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseClaudeReader(path, f, func() time.Time { return descriptorMtime(f.Stat) })
}

func parseClaudeReader(path string, f io.Reader, fallback func() time.Time) (*Session, error) {
	s := &Session{Provider: "claude", ID: strings.TrimSuffix(filepath.Base(path), ".jsonl"), Path: path}

	var cur *Turn
	curCommand := false
	lastTextMsg := ""
	replyAt := time.Time{} // when cur.Reply's text came
	// lastMsg is the message of the last assistant record, "" once input
	// (a tool result, a notification, a prompt) came after it.
	lastMsg := ""
	// ended says the current stretch of cur reached its end with message
	// endMsg; sawStop says some reply of cur recorded why it stopped
	// (older CLIs never do, and their turns are never shown as open).
	ended, endMsg, sawStop, replied := false, "", false, false
	open := func() bool { return cur != nil && !ended && (sawStop || !replied) }
	// synthetic says cur.Reply is an error the CLI wrote (API error, usage
	// limit), not the agent's answer.
	synthetic := false
	flush := func(last bool) {
		if cur == nil {
			return
		}
		cur.Open = last && open()
		if !cur.Open && cur.Reply != "" && !synthetic {
			n := len(cur.Items)
			cur.addAnswer(cur.Reply, replyAt) // the last stretch's answer
			// Nothing came after the message that closed the stretch, or
			// (no stop reasons) after the one the answer is from. Only an
			// answer just added can be provisional, never a steer before it.
			if last && len(cur.Items) > n && lastMsg != "" && (ended && lastMsg == endMsg || lastMsg == lastTextMsg) {
				cur.Items[len(cur.Items)-1].Provisional = true
			}
		}
		if cur.Open {
			cur.Reply = ""
		} else if cur.Reply == "" && len(cur.Earlier) > 0 {
			// The last stretch said nothing: the answer before it stands.
			cur.Reply = cur.Earlier[len(cur.Earlier)-1]
			cur.Earlier = cur.Earlier[:len(cur.Earlier)-1]
		}
		// A local command such as /model leaves no work behind.
		if !(curCommand && cur.Reply == "" && cur.Tools == 0 && len(cur.Earlier) == 0 && cur.Skipped == 0) {
			s.Turns = append(s.Turns, *cur)
		}
		cur = nil
	}
	start := func(t Turn, command bool) {
		flush(false)
		cur, curCommand, lastTextMsg, synthetic = &t, command, "", false
		ended, endMsg, sawStop, replied = false, "", false, false
	}
	// input handles a message from someone other than the person at the
	// terminal: it continues an open turn and opens one of its own when
	// none is.
	input := func(src Source, text string, ts time.Time) {
		if open() {
			if !ts.IsZero() {
				cur.Ended = ts
			}
			return
		}
		if src == Peer {
			text = peerBody(text)
		}
		start(Turn{Prompt: text, Source: src, Started: ts, Ended: ts}, false)
	}

	r := bufio.NewReaderSize(f, 1<<16)
	var buf []byte
	for {
		line, over, err := readLine(r, &buf)
		if err == nil {
			s.Unfinished = false // a complete line; an assistant record sets it again
		}
		switch {
		case over:
			// The record's own cwd, as for a whole one, so a session of only
			// such records is still listed under its own directory alone.
			if s.Cwd == "" && wanted(line) && !ownTrue(line, "isSidechain") {
				s.Cwd = ownString(line, "cwd")
			}
			switch claudeOver(line) {
			case overIgnored: // one the parser reads no part of is no loss
			case overPrompt:
				lastMsg = ""
				ts := parseTime(ownString(line, "timestamp"))
				s.touch(ts)
				start(skippedPrompt(ts), false)
			case overQueued:
				lastMsg = ""
				if !open() {
					ts := parseTime(ownString(line, "timestamp"))
					s.touch(ts)
					start(skippedPrompt(ts), false)
					break
				}
				cur.Skipped++ // a steer
			default:
				s.skipRecord(cur)
			}
		case len(line) > 0 && wanted(line):
			var rec claudeRecord
			if json.Unmarshal(line, &rec) == nil {
				ts := parseTime(rec.Timestamp)
				s.touch(ts)
				if s.Cwd == "" && rec.Cwd != "" && !rec.IsSidechain {
					s.Cwd = rec.Cwd
				}
				if rec.GitBranch != "" && rec.GitBranch != "HEAD" {
					s.Branch = rec.GitBranch
				}
				if rec.Entrypoint != "" && !s.Worker && strings.HasPrefix(rec.Entrypoint, "sdk") {
					s.Worker = true
				}
				switch rec.Type {
				case "custom-title":
					s.Title = rec.CustomTitle
				case "permission-mode":
					s.Bypass = rec.PermissionMode == "bypassPermissions"
				case "attachment":
					a := rec.Attachment
					if rec.IsSidechain || a == nil || a.Type != "queued_command" {
						break
					}
					lastMsg = ""
					// A prompt typed while the agent worked and handed to it
					// mid-turn. Without an origin (older CLIs) the command
					// mode alone says it was typed; without either, it can't
					// be told from a task notification.
					human := a.Origin != nil && a.Origin.Kind == "human" && (a.CommandMode == "" || a.CommandMode == "prompt") ||
						a.Origin == nil && a.CommandMode == "prompt" && !a.IsMeta && !rec.IsMeta
					text, _ := claudeText(a.Prompt, "")
					if !human || text == "" {
						break
					}
					if src := prefixSource(text); src != Human {
						input(src, text, ts)
						break
					}
					if open() {
						cur.Steers = append(cur.Steers, text)
						cur.Items = append(cur.Items, TurnItem{Kind: ItemSteer, Text: text, At: ts})
						if !ts.IsZero() {
							cur.Ended = ts
						}
						break
					}
					start(Turn{Prompt: text, Started: ts, Ended: ts}, false)
				case "user":
					if rec.IsSidechain || rec.IsCompactSummary || rec.Message == nil {
						break
					}
					lastMsg = ""
					if rec.PermissionMode != "" {
						s.Bypass = rec.PermissionMode == "bypassPermissions"
					}
					text, toolResults := claudeText(rec.Message.Content, "user")
					if strings.HasPrefix(text, "[Request interrupted") {
						ended, endMsg = true, lastTextMsg
						break
					}
					// Who sent it, by how it opens, before anything the
					// record claims about itself: send-keys text records as
					// typed.
					src := prefixSource(text)
					if rec.Origin != nil && rec.Origin.Kind == "peer" {
						src = Peer
					}
					if src != Human {
						input(src, text, ts)
						break
					}
					// Task notifications and other messages the CLI sends on
					// its own continue the turn they arrive in.
					if rec.IsMeta || rec.PromptSource == "system" || rec.Origin != nil && rec.Origin.Kind != "" && rec.Origin.Kind != "human" || toolResults && text == "" {
						if cur != nil && !ts.IsZero() {
							cur.Ended = ts // a tool result is still this turn's work
						}
						break
					}
					// What the person typed is a prompt even when it opens
					// with a tag, such as pasted content.
					typed := rec.Origin != nil && rec.Origin.Kind == "human" &&
						(rec.PromptSource == "typed" || rec.PromptSource == "queued" || rec.PromptSource == "suggestion_accepted")
					prompt, command, ok := claudePrompt(text, typed)
					if !ok {
						if curCommand && strings.Contains(text, "<local-command-stdout>") {
							ended = true // the command ran; nothing more comes of it
						}
						if cur != nil && !ts.IsZero() {
							cur.Ended = ts
						}
						break
					}
					start(Turn{Prompt: prompt, Started: ts, Ended: ts}, command)
				case "assistant":
					if rec.IsSidechain || rec.Message == nil || cur == nil {
						break
					}
					cur.settle(ts)
					lastMsg = rec.Message.ID
					s.Unfinished = err == nil
					if m := rec.Message.Model; m != "" && m != "<synthetic>" {
						s.Model = m
					}
					if ended && rec.Message.Model == "<synthetic>" {
						break // an API error noted after the answer was complete
					}
					if ended && (rec.Message.ID == "" || rec.Message.ID != endMsg) {
						// The agent went on after answering: a background
						// task woke it, or a hook sent it back to work.
						if cur.Reply != "" && !synthetic {
							cur.Earlier = append(cur.Earlier, cur.Reply)
							cur.addAnswer(cur.Reply, replyAt)
						}
						cur.Reply, lastTextMsg, ended = "", "", false
					}
					replied = true
					called := false
					var parts []json.RawMessage
					json.Unmarshal(rec.Message.Content, &parts)
					for _, raw := range parts {
						var p contentPart
						json.Unmarshal(raw, &p)
						if narration(p) {
							p.Type, p.Text = "text", p.Thinking
						}
						switch p.Type {
						case "tool_use", "server_tool_use":
							cur.Tools++
							called = true
						case "text":
							text := strings.TrimSpace(p.Text)
							if text == "" {
								continue
							}
							// One message streams as several records; its
							// text blocks belong together.
							if rec.Message.ID != "" && rec.Message.ID == lastTextMsg && cur.Reply != "" {
								cur.Reply += "\n\n" + text
							} else {
								cur.Reply, replyAt = text, ts
							}
							lastTextMsg, synthetic = rec.Message.ID, rec.Message.Model == "<synthetic>"
							// Text before a tool call is said on the way. A
							// record without a stop reason (older CLIs)
							// cannot tell, and adds nothing.
							if sr := rec.Message.StopReason; (sr == "tool_use" || sr == "pause_turn") && !synthetic {
								cur.Items = append(cur.Items, TurnItem{Kind: ItemSaid, Text: text, At: ts, MsgID: rec.Message.ID})
							}
						}
					}
					if !sawStop && rec.Message.StopReason == "" && called {
						// No stop reasons (older CLIs): a tool call means
						// the agent is still at work.
						replied = false
					}
					if sr := rec.Message.StopReason; sr != "" {
						sawStop = true
						if sr != "tool_use" && sr != "pause_turn" {
							ended, endMsg = true, rec.Message.ID
						}
					}
					if !ts.IsZero() {
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
	flush(true)
	if s.Updated.IsZero() {
		s.Updated = fallback()
	}
	return s, nil
}

// What a record over MaxLine was, as far as its opening shows.
const (
	overIgnored = iota // a record the parser reads nothing of
	overOther          // part of the turn it came in
	overPrompt         // a prompt: it opens a turn
	overQueued         // a prompt typed while the agent worked
)

// claudeOver reads a record over MaxLine by its opening, under the rules
// the parser applies to a whole one: a user record is a prompt unless it
// shows itself as a sidechain, a compact summary, meta, the CLI's own or a
// tool result. Fields after the opening are not seen.
func claudeOver(opening []byte) int {
	if !wanted(opening) {
		return overIgnored
	}
	// The record's own type and its attachment's, not a key a payload holds.
	// An attachment may come before the type, past the opening's end.
	typ := ownString(opening, "type")
	if att := ownValue(opening, "attachment"); (typ == "attachment" || typ == "") && ownString(att, "type") == "queued_command" {
		// Under the rules for a whole one: a command is the person's unless
		// what the opening shows says otherwise.
		kind, mode := ownString(ownValue(att, "origin"), "kind"), ownString(att, "commandMode")
		if ownTrue(opening, "isSidechain") || ownTrue(opening, "isMeta") || ownTrue(att, "isMeta") ||
			kind != "" && kind != "human" || mode != "" && mode != "prompt" {
			return overOther
		}
		return overQueued
	}
	if typ != "user" {
		return overOther
	}
	for _, f := range []string{`"isSidechain":true`, `"isCompactSummary":true`, `"isMeta":true`, `"promptSource":"system"`} {
		if bytes.Contains(opening, []byte(f)) {
			return overOther
		}
	}
	if _, rest, ok := bytes.Cut(opening, []byte(`"origin":{"kind":"`)); ok {
		if kind, _, _ := cutString(rest); kind != "human" {
			return overOther
		}
	}
	if _, msg, ok := bytes.Cut(opening, []byte(`"message":`)); ok {
		if _, content, ok := bytes.Cut(msg, []byte(`"content":`)); ok && bytes.HasPrefix(content, []byte("[")) {
			if bytes.Contains(content[:min(len(content), 256)], []byte(`"type":"tool_result"`)) {
				return overOther
			}
		}
	}
	return overPrompt
}

func wanted(line []byte) bool {
	for _, t := range claudeTypes {
		if bytes.Contains(line, t) {
			return true
		}
	}
	return false
}

func (s *Session) touch(ts time.Time) {
	if ts.IsZero() {
		return
	}
	if s.Started.IsZero() || ts.Before(s.Started) {
		s.Started = ts
	}
	if ts.After(s.Updated) {
		s.Updated = ts
	}
}

// claudeText joins the text of a message body (a string or a list of
// parts) and reports whether it carried tool results.
func claudeText(raw json.RawMessage, role string) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	if raw[0] == '"' {
		var s string
		json.Unmarshal(raw, &s)
		return strings.TrimSpace(s), false
	}
	var parts []contentPart
	json.Unmarshal(raw, &parts)
	var texts []string
	tools := false
	for _, p := range parts {
		switch p.Type {
		case "tool_result":
			tools = true
		case "text":
			// Pasted text is the person's, though it opens with a tag;
			// whether the record as a whole is a prompt is decided later.
			if role == "user" && injected(p.Text) && !strings.Contains(p.Text, "<command-name>") && !strings.HasPrefix(strings.TrimSpace(p.Text), "<pasted_content") {
				continue
			}
			if t := strings.TrimSpace(p.Text); t != "" {
				texts = append(texts, t)
			}
		}
	}
	return strings.Join(texts, "\n"), tools
}

var (
	commandName = regexp.MustCompile(`<command-name>\s*([^<]*?)\s*</command-name>`)
	commandArgs = regexp.MustCompile(`(?s)<command-args>\s*(.*?)\s*</command-args>`)
)

// claudePrompt decides whether a user record is a human prompt. A slash
// command comes back as "/name args" with command set. typed says the CLI
// recorded the text as typed by the person, so a tag it opens with (pasted
// content) does not make it the CLI's own.
func claudePrompt(text string, typed bool) (prompt string, command, ok bool) {
	if text == "" || strings.HasPrefix(text, "[Request interrupted") {
		return "", false, false
	}
	if m := commandName.FindStringSubmatch(text); m != nil && (!typed || strings.HasPrefix(text, "<command-")) {
		name := m[1]
		if !strings.HasPrefix(name, "/") {
			name = "/" + name
		}
		if a := commandArgs.FindStringSubmatch(text); a != nil && a[1] != "" {
			name += " " + a[1]
		}
		return name, true, true
	}
	if !typed && injected(text) {
		return "", false, false
	}
	return text, false, true
}
