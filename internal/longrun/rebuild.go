package longrun

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

// rebuiltHeading opens a note aiq rebuilt itself; TakeoverPrompt tells it
// from one the session wrote by it.
const rebuiltHeading = "# Handoff note rebuilt by aiq"

// prepareNote readies the handoff note for a successor. A note older than
// the lease is an earlier takeover's: it is archived, never offered as
// current. Without a note, a successor that starts fresh gets one rebuilt
// from the previous session's transcript; a resumed one has the transcript
// itself.
func (s *Supervisor) prepareNote(l state.Lease, resume bool, now time.Time) {
	p := HandoffPath(l.Workspace)
	if fi, err := os.Stat(p); err == nil {
		if l.StartedAt == 0 || !fi.ModTime().Before(time.Unix(l.StartedAt, 0)) {
			return
		}
		ArchiveNote(l.Workspace, now)
		s.Logf("lease %d: handoff note from %s predates the session; archived", l.ID, fi.ModTime().Format("2006-01-02 15:04"))
	}
	if resume {
		return
	}
	note, err := RebuildNote(l, now)
	if err != nil {
		s.Logf("lease %d: no handoff note, and none rebuilt: %v", l.ID, err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err == nil {
		err = os.WriteFile(p, []byte(note), 0o600)
	}
	if err != nil {
		s.Logf("lease %d: writing the rebuilt note: %v", l.ID, err)
	}
}

// rebuilt reports whether the note at p is one aiq rebuilt.
func rebuilt(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, len(rebuiltHeading))
	_, err = io.ReadFull(f, head)
	return err == nil && string(head) == rebuiltHeading
}

// Caps keep a rebuilt note short enough to read first.
const (
	noteTurns   = 4    // the last turns shown
	noteSaid    = 8    // the last things said on the way, per turn
	capPrompt   = 1500 // a prompt or a reply
	capSaid     = 500  // one thing said, a steer
	capToolText = 800  // the last tool call's input, its output
	capGitLines = 30   // per git command
)

// RebuildNote writes a handoff note from the transcript of a session that
// left none: what it was asked, the last turns, the last tool call, and the
// state of the working tree.
func RebuildNote(l state.Lease, now time.Time) (string, error) {
	if l.Transcript == "" {
		return "", fmt.Errorf("the lease has no transcript recorded")
	}
	sess, err := parseTranscript(l.Provider, l.Transcript)
	if err != nil {
		return "", err
	}
	if len(sess.Turns) == 0 {
		return "", fmt.Errorf("the transcript %s has no turns", l.Transcript)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\nThe previous %s session (account %s", rebuiltHeading, l.Provider, l.AccountID)
	if l.SessionID != "" {
		fmt.Fprintf(&b, ", session %s", l.SessionID)
	}
	fmt.Fprintf(&b, ") stopped without writing a handoff note. aiq rebuilt this one at %s from its transcript:\n\n    %s\n\n", now.Format("2006-01-02 15:04"), l.Transcript)
	if !sess.Updated.IsZero() {
		fmt.Fprintf(&b, "Its last record is from %s. ", stamp(sess.Updated))
	}
	b.WriteString("It shows what was asked and said, not what the session concluded. Check the working tree before relying on it.\n")

	if t, ok := firstTyped(sess.Turns); ok {
		fmt.Fprintf(&b, "\n## The task, as first typed (%s)\n\n%s\n", stamp(t.Started), cut(t.Prompt, capPrompt))
	}
	turns := sess.Turns
	if len(turns) > noteTurns {
		turns = turns[len(turns)-noteTurns:]
	}
	b.WriteString("\n## The last turns, oldest first\n")
	for _, t := range turns {
		writeTurn(&b, t)
	}
	if call, out, ok := lastToolCall(l.Provider, l.Transcript); ok {
		fmt.Fprintf(&b, "\n## The last tool call\n\n```\n%s\n```\n", cut(call, capToolText))
		if out == "" {
			b.WriteString("\nNo result recorded: the session stopped while it ran, or before it read the result.\n")
		} else {
			fmt.Fprintf(&b, "\nIts result:\n\n```\n%s\n```\n", cut(out, capToolText))
		}
	}
	if g := gitSummary(l.Workspace); g != "" {
		fmt.Fprintf(&b, "\n## The working tree at the takeover\n%s", g)
	}
	return b.String(), nil
}

func parseTranscript(provider, path string) (*transcript.Session, error) {
	switch provider {
	case "claude":
		return transcript.ParseClaude(path)
	case "codex":
		return transcript.ParseCodex(path)
	case "agy":
		return transcript.ParseAgy(path)
	}
	return nil, fmt.Errorf("no transcript reader for %s", provider)
}

// firstTyped is the session's first prompt the person typed.
func firstTyped(turns []transcript.Turn) (transcript.Turn, bool) {
	for _, t := range turns {
		if t.Source == transcript.Human && strings.TrimSpace(t.Prompt) != "" {
			return t, true
		}
	}
	return transcript.Turn{}, false
}

func writeTurn(b *strings.Builder, t transcript.Turn) {
	who := map[transcript.Source]string{transcript.Human: "typed", transcript.Aiq: "from aiq", transcript.Peer: "from a peer session", transcript.Notice: "from a notifier"}[t.Source]
	fmt.Fprintf(b, "\n### %s · %s\n\n%s\n\n", stamp(t.Started), who, cut(t.Prompt, capPrompt))
	items := t.Items
	if len(items) == 0 { // a reader that keeps no items
		for _, st := range t.Steers {
			items = append(items, transcript.TurnItem{Kind: transcript.ItemSteer, Text: st})
		}
	}
	said := 0
	for _, it := range items {
		if it.Kind == transcript.ItemSaid {
			said++
		}
	}
	for _, it := range items {
		switch it.Kind {
		case transcript.ItemSteer:
			fmt.Fprintf(b, "- The person, while it worked: %s\n", cut(it.Text, capSaid))
		case transcript.ItemSaid:
			if said--; said < noteSaid {
				fmt.Fprintf(b, "- Said on the way: %s\n", cut(it.Text, capSaid))
			}
		case transcript.ItemAnswer:
			if it.Text != t.Reply {
				fmt.Fprintf(b, "- Answered: %s\n", cut(it.Text, capSaid))
			}
		}
	}
	switch {
	case t.Open:
		fmt.Fprintf(b, "\nThe session stopped mid-turn, after %d tool call%s.\n", t.Tools, map[bool]string{true: "", false: "s"}[t.Tools == 1])
	case t.Reply != "":
		fmt.Fprintf(b, "\nReply:\n\n%s\n", cut(t.Reply, capPrompt))
	}
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return "time unknown"
	}
	return t.Local().Format("2006-01-02 15:04")
}

// cut trims s to n bytes on a rune boundary, saying how much it dropped.
func cut(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	i := n
	for i > 0 && !utf8RuneStart(s[i]) {
		i--
	}
	return s[:i] + fmt.Sprintf(" […%d more bytes]", len(s)-i)
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// tailBytes is how much of a transcript's end lastToolCall reads.
const tailBytes = 4 << 20

// lastToolCall is the last tool call in a transcript and its output (""
// when none was recorded). Only Claude and Codex transcripts are read.
func lastToolCall(provider, path string) (call, out string, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", false
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > tailBytes {
		f.Seek(fi.Size()-tailBytes, io.SeekStart)
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	var id string
	for sc.Scan() {
		line := sc.Bytes()
		switch provider {
		case "claude":
			if c, o, cid, kind := claudeToolRecord(line); kind == "call" {
				call, out, id, ok = c, "", cid, true
			} else if kind == "result" && cid == id {
				out = o
			}
		case "codex":
			if c, o, cid, kind := codexToolRecord(line); kind == "call" {
				call, out, id, ok = c, "", cid, true
			} else if kind == "result" && cid == id {
				out = o
			}
		}
	}
	return call, out, ok
}

func claudeToolRecord(line []byte) (call, out, id, kind string) {
	if !bytes.Contains(line, []byte(`"tool_use`)) && !bytes.Contains(line, []byte(`"tool_result"`)) {
		return
	}
	var r struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &r) != nil {
		return
	}
	var parts []struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
		IsError   bool            `json:"is_error"`
	}
	json.Unmarshal(r.Message.Content, &parts)
	for _, p := range parts {
		switch p.Type {
		case "tool_use", "server_tool_use":
			var in struct {
				Command string `json:"command"`
			}
			json.Unmarshal(p.Input, &in)
			call, id, kind = p.Name+": "+string(p.Input), p.ID, "call"
			if in.Command != "" {
				call = p.Name + ": " + in.Command
			}
		case "tool_result":
			out, id, kind = toolText(p.Content), p.ToolUseID, "result"
			if p.IsError {
				out = "(error) " + out
			}
		}
	}
	return
}

func codexToolRecord(line []byte) (call, out, id, kind string) {
	if !bytes.Contains(line, []byte(`_call`)) {
		return
	}
	var r struct {
		Type    string `json:"type"`
		Payload struct {
			Type      string          `json:"type"`
			Name      string          `json:"name"`
			CallID    string          `json:"call_id"`
			Arguments string          `json:"arguments"`
			Input     string          `json:"input"`
			Output    json.RawMessage `json:"output"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &r) != nil || r.Type != "response_item" {
		return
	}
	p := r.Payload
	switch p.Type {
	case "function_call", "custom_tool_call", "local_shell_call":
		return p.Name + ": " + p.Arguments + p.Input, "", p.CallID, "call"
	case "function_call_output", "custom_tool_call_output":
		return "", toolText(p.Output), p.CallID, "result"
	}
	return
}

// toolText is a tool result's text: a string, or the text parts of a list.
func toolText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	json.Unmarshal(raw, &parts)
	var texts []string
	for _, p := range parts {
		if p.Text != "" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// gitSummary is the working tree's state, for a note; "" outside a repo.
func gitSummary(dir string) string {
	git := func(args ...string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Output()
		if err != nil {
			return ""
		}
		lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
		if len(lines) > capGitLines {
			lines = append(lines[:capGitLines], fmt.Sprintf("… %d more lines", len(lines)-capGitLines))
		}
		return strings.Join(lines, "\n")
	}
	status := git("status", "-sb")
	if status == "" {
		return ""
	}
	var b strings.Builder
	for _, c := range []struct{ name, out string }{
		{"git status -sb", status},
		{"git log --oneline --decorate -8", git("log", "--oneline", "--decorate", "-8")},
		{"git stash list", git("stash", "list")},
		{"git worktree list", git("worktree", "list")},
	} {
		if c.out != "" {
			fmt.Fprintf(&b, "\n`%s`:\n\n```\n%s\n```\n", c.name, c.out)
		}
	}
	return b.String()
}
