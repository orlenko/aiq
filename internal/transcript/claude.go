package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var nonAlnum = regexp.MustCompile(`[^a-zA-Z0-9]`)

// claudeMaxDirName is where Claude Code truncates an encoded project path
// and appends a hash.
const claudeMaxDirName = 200

// claudeFiles lists the top-level transcripts of dir's project directory
// under each root. Subagent transcripts live one level deeper and are left
// out.
func claudeFiles(roots []string, dir string) []found {
	encodings := map[string]bool{nonAlnum.ReplaceAllString(dir, "-"): true}
	if wd, err := os.Getwd(); err == nil && canonical(wd) == dir {
		encodings[nonAlnum.ReplaceAllString(filepath.Clean(wd), "-")] = true
	}
	var out []found
	seen := map[string]bool{}
	for _, root := range roots {
		for enc := range encodings {
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
						out = append(out, found{"claude", f})
					}
				}
			}
		}
	}
	return out
}

type claudeRecord struct {
	Type             string `json:"type"`
	IsMeta           bool   `json:"isMeta"`
	IsSidechain      bool   `json:"isSidechain"`
	IsCompactSummary bool   `json:"isCompactSummary"`
	Cwd              string `json:"cwd"`
	SessionID        string `json:"sessionId"`
	Timestamp        string `json:"timestamp"`
	Entrypoint       string `json:"entrypoint"`
	GitBranch        string `json:"gitBranch"`
	CustomTitle      string `json:"customTitle"`
	PermissionMode   string `json:"permissionMode"`
	PromptSource     string `json:"promptSource"`
	Origin           *struct {
		Kind string `json:"kind"`
	} `json:"origin"`
	Message *struct {
		ID      string          `json:"id"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type contentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// claudeTypes are the record types the parser reads; the rest (file
// snapshots, attachments, progress) can be large and are skipped unparsed.
var claudeTypes = [][]byte{
	[]byte(`"type":"user"`), []byte(`"type":"assistant"`), []byte(`"type":"custom-title"`), []byte(`"type":"permission-mode"`),
}

// ParseClaude reads one Claude Code session transcript.
func ParseClaude(path string) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s := &Session{Provider: "claude", ID: strings.TrimSuffix(filepath.Base(path), ".jsonl"), Path: path}

	var cur *Turn
	curCommand := false
	lastTextMsg := ""
	flush := func() {
		if cur == nil {
			return
		}
		// A local command such as /model leaves no work behind.
		if !(curCommand && cur.Reply == "" && cur.Tools == 0) {
			s.Turns = append(s.Turns, *cur)
		}
		cur = nil
	}

	r := bufio.NewReaderSize(f, 1<<16)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && wanted(line) {
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
				case "user":
					if rec.IsMeta || rec.IsSidechain || rec.IsCompactSummary || rec.Message == nil {
						break
					}
					if rec.PermissionMode != "" {
						s.Bypass = rec.PermissionMode == "bypassPermissions"
					}
					text, toolResults := claudeText(rec.Message.Content, "user")
					prompt, command, ok := claudePrompt(text)
					// Task notifications and other messages the CLI sends on
					// its own continue the turn they arrive in.
					system := rec.PromptSource == "system" || (rec.Origin != nil && rec.Origin.Kind != "" && rec.Origin.Kind != "human")
					if !ok || system || (toolResults && text == "") {
						if cur != nil && !ts.IsZero() {
							cur.Ended = ts // a tool result is still this turn's work
						}
						break
					}
					flush()
					cur, curCommand, lastTextMsg = &Turn{Prompt: prompt, Started: ts, Ended: ts}, command, ""
				case "assistant":
					if rec.IsSidechain || rec.Message == nil || cur == nil {
						break
					}
					if m := rec.Message.Model; m != "" && m != "<synthetic>" {
						s.Model = m
					}
					var parts []json.RawMessage
					json.Unmarshal(rec.Message.Content, &parts)
					for _, raw := range parts {
						var p contentPart
						json.Unmarshal(raw, &p)
						switch p.Type {
						case "tool_use", "server_tool_use":
							cur.Tools++
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
								cur.Reply = text
							}
							lastTextMsg = rec.Message.ID
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
	flush()
	if s.Updated.IsZero() {
		if info, err := f.Stat(); err == nil {
			s.Updated = info.ModTime()
		}
	}
	return s, nil
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
			if role == "user" && injected(p.Text) && !strings.Contains(p.Text, "<command-name>") {
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
// command comes back as "/name args" with command set.
func claudePrompt(text string) (prompt string, command, ok bool) {
	if text == "" || strings.HasPrefix(text, "[Request interrupted") {
		return "", false, false
	}
	if m := commandName.FindStringSubmatch(text); m != nil {
		name := m[1]
		if !strings.HasPrefix(name, "/") {
			name = "/" + name
		}
		if a := commandArgs.FindStringSubmatch(text); a != nil && a[1] != "" {
			name += " " + a[1]
		}
		return name, true, true
	}
	if injected(text) {
		return "", false, false
	}
	return text, false, true
}
