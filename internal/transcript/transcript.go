// Package transcript reads the session transcripts Claude Code, Codex and
// the Antigravity CLI leave on disk and reduces each one to what a person
// coming back to a directory wants to know: who worked here, when, and what
// each turn asked and answered.
//
// Claude keeps one JSONL file per session under projects/<encoded-cwd>/.
// Codex keeps rollout-*.jsonl files under sessions/YYYY/MM/DD/, each opening
// with a session_meta record that names its cwd. Antigravity keeps
// brain/<id>/.system_generated/logs/transcript.jsonl and records the
// workspace of each prompt in history.jsonl. All formats change between CLI
// versions, so every field is optional and unknown records are ignored.
package transcript

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Session is one transcript.
type Session struct {
	Provider string // "claude", "codex" or "agy"
	ID       string
	Path     string
	Cwd      string
	Title    string // the name the user gave the session, if any
	Branch   string
	Model    string
	Started  time.Time
	Updated  time.Time
	Turns    []Turn

	// Worker marks a non-interactive run (claude -p, codex exec).
	Worker bool
	// Bypass records that the session ran with the permission bypass
	// (Claude bypassPermissions, Codex never-ask + full access).
	Bypass bool
}

// Turn is one human prompt and what the agent did with it.
type Turn struct {
	Prompt  string
	Reply   string // the agent's last message of the turn
	Started time.Time
	Ended   time.Time
	Tools   int // tool calls made during the turn
}

// Label is the session's name, else its first prompt.
func (s Session) Label() string {
	if s.Title != "" {
		return s.Title
	}
	for _, t := range s.Turns {
		if t.Prompt != "" {
			return t.Prompt
		}
	}
	return "(no prompt)"
}

// Tools is the number of tool calls across the whole session.
func (s Session) Tools() int {
	n := 0
	for _, t := range s.Turns {
		n += t.Tools
	}
	return n
}

// Roots names the directories to search. Overlay homes normally link
// projects/ and sessions/ to the real home; a real directory in an overlay
// (created before the first sync) is searched too.
type Roots struct {
	ClaudeProjects []string
	CodexSessions  []string
	CodexIndex     []string // session_index.jsonl files (thread names)
	AgyAppData     []string // Antigravity CLI app data dirs (brain/, history.jsonl)
}

// DefaultRoots derives the search roots from the real homes (realAgy is the
// Antigravity CLI's app data dir; "" leaves it out) and the parent
// directories that hold aiq's overlay homes.
func DefaultRoots(realClaude, realCodex, realAgy string, overlayParents ...string) Roots {
	r := Roots{
		ClaudeProjects: []string{filepath.Join(realClaude, "projects")},
		CodexSessions:  []string{filepath.Join(realCodex, "sessions")},
		CodexIndex:     []string{filepath.Join(realCodex, "session_index.jsonl")},
	}
	if realAgy != "" {
		r.AgyAppData = []string{realAgy}
	}
	for _, parent := range overlayParents {
		homes, _ := os.ReadDir(parent)
		for _, h := range homes {
			home := filepath.Join(parent, h.Name())
			if isRealDir(filepath.Join(home, "projects")) {
				r.ClaudeProjects = append(r.ClaudeProjects, filepath.Join(home, "projects"))
			}
			if isRealDir(filepath.Join(home, "sessions")) {
				r.CodexSessions = append(r.CodexSessions, filepath.Join(home, "sessions"))
			}
		}
	}
	return r
}

func isRealDir(p string) bool {
	info, err := os.Lstat(p)
	return err == nil && info.IsDir()
}

// List returns every session whose working directory is dir, newest first.
// A session found under more than one root is listed once.
func List(r Roots, dir string) ([]Session, error) {
	dir = canonical(dir)
	var paths []found
	paths = append(paths, claudeFiles(r.ClaudeProjects, dir)...)
	paths = append(paths, codexFiles(r.CodexSessions, dir)...)
	paths = append(paths, agyFiles(r.AgyAppData, dir)...)

	out := make([]*Session, len(paths))
	parallel(len(paths), func(i int) {
		var s *Session
		var err error
		switch paths[i].provider {
		case "claude":
			s, err = ParseClaude(paths[i].path)
		case "codex":
			s, err = ParseCodex(paths[i].path)
		case "agy":
			s, err = ParseAgy(paths[i].path)
		}
		if err != nil || s == nil {
			return
		}
		if s.Cwd != "" && canonical(s.Cwd) != dir {
			return // an encoded project name shared by two directories
		}
		out[i] = s
	})

	names := codexNames(r.CodexIndex)
	byID := map[string]*Session{}
	for _, s := range out {
		if s == nil {
			continue
		}
		if s.Provider == "codex" && s.Title == "" {
			s.Title = names[s.ID]
		}
		key := s.Provider + "/" + s.ID
		if prev, ok := byID[key]; ok && !s.Updated.After(prev.Updated) {
			continue
		}
		byID[key] = s
	}
	list := make([]Session, 0, len(byID))
	for _, s := range byID {
		if len(s.Turns) == 0 {
			continue // opened and closed without a word
		}
		list = append(list, *s)
	}
	sort.Slice(list, func(i, j int) bool {
		if !list[i].Updated.Equal(list[j].Updated) {
			return list[i].Updated.After(list[j].Updated)
		}
		return list[i].ID < list[j].ID
	})
	return list, nil
}

type found struct {
	provider string
	path     string
}

// canonical resolves symlinks (/tmp → /private/tmp) so a session recorded
// under either spelling matches.
func canonical(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

func parallel(n int, fn func(i int)) {
	workers := runtime.GOMAXPROCS(0)
	if workers > 8 {
		workers = 8
	}
	next := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				fn(i)
			}
		}()
	}
	for i := 0; i < n; i++ {
		next <- i
	}
	close(next)
	wg.Wait()
}

// Clean flattens whitespace so a prompt fits on one line.
func Clean(s string) string {
	return strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " ")
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// injected reports text a CLI put into the user's side of the conversation
// on its own: environment blocks, instructions, command echoes, reminders.
func injected(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return true
	}
	if strings.HasPrefix(t, "# AGENTS.md instructions") {
		return true
	}
	if strings.HasPrefix(t, "</") {
		return true
	}
	if strings.HasPrefix(t, "<") {
		end := strings.IndexAny(t, " >")
		if end > 1 && isTagName(t[1:end]) {
			return true
		}
	}
	return false
}

func isTagName(s string) bool {
	if s == "" || !(s[0] >= 'a' && s[0] <= 'z' || s[0] >= 'A' && s[0] <= 'Z') {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
