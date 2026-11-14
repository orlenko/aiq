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
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode"
)

// idPattern is what a session id may look like: a letter or digit, then
// letters, digits, dots, dashes and underscores. Every CLI's own ids (UUIDs)
// fit.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ValidID says whether id is one aiq passes to a CLI to resume a session.
// Ids come from transcripts, hooks and launch records, and the CLI reads the
// id as one of its arguments: one that started with a dash would be read as
// a flag.
func ValidID(id string) bool { return idPattern.MatchString(id) }

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
	// Unfinished says the file ends on an agent record with nothing after
	// it. Claude Code writes one message as several records (thinking,
	// then text) and closes a turn with system records, so the last turn's
	// reply may still be on its way.
	Unfinished bool
	// Skipped counts the records over MaxLine read past before the first
	// turn; a turn counts those that came while it ran.
	Skipped int
}

// Source says who put a turn's prompt into the conversation.
type Source uint8

const (
	Human  Source = iota // typed (or queued) by the person at the terminal
	Aiq                  // aiq's own takeover nudge
	Peer                 // another agent session's message
	Notice               // a notifier tool typed it in (agent-nudge, Agent Orchestra)
)

// Turn is one prompt and what the agent did with it.
type Turn struct {
	Prompt  string
	Source  Source
	Steers  []string  // human input that arrived while the turn ran
	Reply   string    // the agent's final message of the turn; "" while Open
	ReplyAt time.Time // when Reply was written, where the transcript has no answer items to say so
	Earlier []string  // final messages of the turn's earlier stretches, when a background wakeup or peer message made the agent go on after answering
	Open    bool      // the agent is still working on it
	Started time.Time
	Ended   time.Time
	Tools   int // tool calls made during the turn
	// Items is the turn after its prompt, in the order it happened: the
	// person's steers, what the agent said while it worked, and the
	// answers that ended a stretch (Earlier, and Reply once the turn is
	// over). Items only grow as the file does. A parser that cannot tell
	// work-in-progress text from an answer (an older CLI) leaves the
	// agent's part out, and Earlier and Reply stand alone.
	Items []TurnItem
	// Skipped counts the records over MaxLine read past while the turn ran.
	Skipped int
	// PromptSkipped says the record that opened the turn was its prompt,
	// over MaxLine and skipped: Prompt is empty, and the record is one of
	// Skipped.
	PromptSkipped bool
}

// ItemKind says what a TurnItem is.
type ItemKind uint8

const (
	ItemSteer  ItemKind = iota // the person typed it while the turn ran
	ItemSaid                   // the agent said it on the way: Claude text before a tool call, Codex commentary
	ItemAnswer                 // an answer that ended a stretch of the turn
)

// TurnItem is one thing that happened in a turn after its prompt.
type TurnItem struct {
	Kind ItemKind
	Text string
	At   time.Time
	// MsgID is the agent message a said item comes from (Claude). A
	// message with several text blocks, or written as several records,
	// gives one item each, in a row.
	MsgID string
	// Promotes marks an answer that repeats the agent's last said item: a
	// stretch that ended on its own narration (Claude's thinking-only end,
	// an interrupt) or on commentary (an aborted Codex turn), or a final
	// answer that says again what the last commentary said.
	Promotes bool
	// Provisional marks the answer of a stretch whose closing message is
	// the last thing in the file: Claude Code may still be writing it (its
	// text comes as a record after its thinking), so the answer can still
	// change. Anything after that message settles it.
	Provisional bool
	// Supersedes marks an answer that replaces the answer just before it,
	// of the same stretch: Codex's task_complete naming a last message
	// other than the final_answer it had sent.
	Supersedes bool
}

// addAnswer records an answer that ended a stretch. It Promotes the last
// thing said when it repeats it: the whole message, all of its text
// blocks joined, as the stretch's answer joins them, and only when that
// message is the turn's last item. When the person typed something after
// it, it is not promoted to answer what they typed: it stands for the
// answer where it is, and the answer is not recorded again.
func (t *Turn) addAnswer(text string, at time.Time) {
	if n := len(t.Items); n > 0 && t.Items[n-1].Kind == ItemSaid && t.saidEndingAt(n-1) == text {
		t.Items = append(t.Items, TurnItem{Kind: ItemAnswer, Text: text, At: at, Promotes: true})
		return
	}
	if i := t.lastAgent(); i >= 0 && t.Items[i].Kind == ItemSaid && t.saidEndingAt(i) == text {
		return
	}
	t.Items = append(t.Items, TurnItem{Kind: ItemAnswer, Text: text, At: at})
}

// answered reports whether the agent's last item is an answer saying text.
func (t *Turn) answered(text string) bool {
	i := t.lastAgent()
	return i >= 0 && t.Items[i].Kind == ItemAnswer && t.Items[i].Text == text
}

// saidEndingAt is the message said on the way whose last part is item i,
// its parts joined.
func (t *Turn) saidEndingAt(i int) string {
	j := i
	for j > 0 && t.Items[j-1].Kind == ItemSaid && t.Items[i].MsgID != "" && t.Items[j-1].MsgID == t.Items[i].MsgID {
		j--
	}
	parts := make([]string, 0, i-j+1)
	for _, it := range t.Items[j : i+1] {
		parts = append(parts, it.Text)
	}
	return strings.Join(parts, "\n\n")
}

// lastAgent is the index of the agent's last item, or -1.
func (t *Turn) lastAgent() int {
	for i := len(t.Items) - 1; i >= 0; i-- {
		if t.Items[i].Kind != ItemSteer {
			return i
		}
	}
	return -1
}

// skippedPrompt is the turn a prompt over MaxLine opens, at ts if its
// opening says when.
func skippedPrompt(ts time.Time) Turn {
	return Turn{Source: Human, Skipped: 1, PromptSkipped: true, Started: ts, Ended: ts}
}

// settle gives a turn whose skipped prompt had no time the time of the first
// record it gets, so no duration runs from the zero time.
func (t *Turn) settle(ts time.Time) {
	if t.PromptSkipped && t.Started.IsZero() && !ts.IsZero() {
		t.Started = ts
		if t.Ended.Before(ts) {
			t.Ended = ts
		}
	}
}

// SkippedNote says a parser read past a record longer than it reads.
func SkippedNote() string {
	return fmt.Sprintf("a record over %d MB was skipped", MaxLine>>20)
}

// skipRecord counts a record over MaxLine against cur, or against s before
// the first turn.
func (s *Session) skipRecord(cur *Turn) {
	if cur != nil {
		cur.Skipped++
	} else {
		s.Skipped++
	}
}

// Label is the session's name, else its first human prompt, else the note
// that a record was skipped when one was.
func (s Session) Label() string {
	if s.Title != "" {
		return s.Title
	}
	skipped := s.Skipped > 0
	for _, t := range s.Turns {
		if t.Prompt != "" && t.Source == Human {
			return t.Prompt
		}
		skipped = skipped || t.PromptSkipped
	}
	if skipped {
		return SkippedNote()
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
	CopilotAppData []string
	// Labels names the account a root belongs to, by the root as listed
	// above: a root in an overlay home exactly one account records. A root
	// without a label is the real home's, or not known to be one account's.
	Labels map[string]string
}

// DefaultRoots derives the search roots from the real homes (realAgy is the
// Antigravity CLI's app data dir; "" leaves it out) and the parent
// directories that hold aiq's overlay homes.
func DefaultRoots(realClaude, realCodex, realAgy, realCopilot string, overlayParents ...string) Roots {
	r := Roots{
		ClaudeProjects: []string{filepath.Join(realClaude, "projects")},
		CodexSessions:  []string{filepath.Join(realCodex, "sessions")},
		CodexIndex:     []string{filepath.Join(realCodex, "session_index.jsonl")},
	}
	if realAgy != "" {
		r.AgyAppData = []string{realAgy}
	}
	if realCopilot != "" {
		r.CopilotAppData = []string{realCopilot}
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

// The prompts aiq types into a session it moved to a fresh account start
// with these; longrun builds its prompts from them so the two never drift.
const (
	AiqResumeNudge  = "aiq moved this session to a fresh quota account; the conversation above is yours. "
	AiqHandoffNudge = "aiq is handing a long-running task over to you from a "
)

// peerPrefix opens a message another Claude session sent to this one.
// Claude Code 2.1.268–284 records it with no origin.
const peerPrefix = "Another Claude session sent a message"

// OrchestraNotice opens what Agent Orchestra types into a session when
// mail waits: the same text every time, a command to read the inbox.
const OrchestraNotice = "Agent Orchestra local inbox notice."

// noticePrefixes open what notifier tools type into a session.
var noticePrefixes = []string{"[agent-nudge]", OrchestraNotice}

// AiqDrainNudge and AiqDrainEnd open and close the instruction aiq gives a session running
// low on quota. Both must match, as a person can open their own text with the same words.
const (
	AiqDrainNudge = "aiq: this account's quota"
	AiqDrainEnd   = "relies on the note plus the working tree."
)

// prefixSource names who sent a prompt by how it opens. Text typed in with
// tmux send-keys records as typed by a human, so the prefix decides.
func prefixSource(text string) Source {
	t := strings.TrimSpace(text)
	switch {
	case strings.HasPrefix(t, strings.TrimSpace(AiqResumeNudge)), strings.HasPrefix(t, AiqHandoffNudge), strings.HasPrefix(t, AiqDrainNudge) && strings.HasSuffix(t, AiqDrainEnd):
		return Aiq
	case strings.HasPrefix(t, peerPrefix):
		return Peer
	}
	for _, p := range noticePrefixes {
		if strings.HasPrefix(t, p) {
			return Notice
		}
	}
	return Human
}

// peerBody strips the header line, the wrapping tag and the advice the CLI
// appends after it from a peer message, leaving what the other session said.
func peerBody(text string) string {
	t := strings.TrimSpace(text)
	if strings.HasPrefix(t, peerPrefix) {
		if i := strings.IndexByte(t, '\n'); i >= 0 {
			t = strings.TrimSpace(t[i+1:])
		}
	}
	if strings.HasPrefix(t, "<") {
		end := strings.IndexAny(t, " >")
		if end > 1 && isTagName(t[1:end]) {
			closing := "</" + t[1:end] + ">"
			gt, end := strings.IndexByte(t, '>'), strings.Index(t, closing)
			if gt > 0 && end > gt {
				t = strings.TrimSpace(t[gt+1 : end])
			}
		}
	}
	if t == "" {
		return strings.TrimSpace(text)
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
