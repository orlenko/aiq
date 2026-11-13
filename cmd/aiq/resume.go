package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/term"

	"errors"
	"github.com/orlenko/aiq/internal/herdr"
	"github.com/orlenko/aiq/internal/longrun"
	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/tmux"
	"github.com/orlenko/aiq/internal/transcript"
)

const resumeUsage = `usage: aiq resume [--all] [--print] [--launcher <name> | --bare] [<session-id>] [-- extra CLI args]
  no id, on a terminal: browse this directory's sessions, read their turns, press r to resume
  <session-id>          resume that session (a unique prefix is enough)
  --print               print the sessions (or, with an id, that session's turns) and exit
  --all                 include worker sessions (claude -p, codex exec, agy -p)
  --launcher <name>     resume through this launcher instead of the one the session ran under
  --bare                resume without a launcher, even if the session ran under one`

type resumeOpts struct {
	all      bool
	print    bool
	launcher string
	bare     bool
	id       string
	extra    []string
}

func parseResumeArgs(args []string) (resumeOpts, error) {
	var o resumeOpts
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			o.extra = args[i+1:]
			return o, nil
		case a == "--all" || a == "-a":
			o.all = true
		case a == "--print" || a == "-p":
			o.print = true
		case a == "--bare":
			o.bare = true
		case a == "--launcher":
			if i+1 >= len(args) {
				return o, fmt.Errorf("--launcher requires a name")
			}
			o.launcher = args[i+1]
			i++
		case strings.HasPrefix(a, "--launcher="):
			o.launcher = strings.TrimPrefix(a, "--launcher=")
		case a == "-h" || a == "--help":
			return o, fmt.Errorf("%s", resumeUsage)
		case strings.HasPrefix(a, "-"):
			return o, fmt.Errorf("unknown flag %s\n%s", a, resumeUsage)
		case o.id == "":
			o.id = a
		default:
			return o, fmt.Errorf("one session id at a time\n%s", resumeUsage)
		}
	}
	if o.bare && o.launcher != "" {
		return o, fmt.Errorf("--bare and --launcher contradict each other")
	}
	return o, nil
}

// unreadableShown is how many unreadable transcripts are named one by one.
const unreadableShown = 10

// reportUnreadable names, one line each, the transcripts a list left out
// because they could not be read, so the person can find the file; past
// the first few, it counts the rest. It goes out before any screen is up.
func reportUnreadable(w io.Writer, paths []string) {
	for i, p := range paths {
		if i == unreadableShown {
			fmt.Fprintf(w, "aiq: …and %d more could not be read\n", len(paths)-i)
			return
		}
		fmt.Fprintf(w, "aiq: could not read transcript %q\n", p)
	}
}

// liveNote says where a session is running now, so resuming it does not
// start a second copy of the same conversation.
type liveNote struct {
	native string // native Herdr runner reference
	pid    int    // an open Claude process
	tmux   string // an aiq long tmux session
	note   string
}

func cmdResume(args []string) error {
	o, err := parseResumeArgs(args)
	if err != nil {
		return configErr("bad-flags", "%v", err)
	}
	a, err := openApp()
	if err != nil {
		return err
	}
	defer a.close()
	if o.launcher != "" {
		if _, ok := a.cfg.Launcher(o.launcher); !ok {
			return configErr("no-launcher", "no launcher %q; see: aiq launcher list", o.launcher)
		}
	}
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	sessions, unreadable, err := transcript.List(defaultRoots(), dir)
	reportUnreadable(os.Stderr, unreadable)
	if err != nil {
		return err
	}
	live := liveSessions(a.st, paths.RealClaudeHome(), a.cfg.Long.TmuxPrefix)
	origins := loadOrigins(a.st, hostname(), os.Getenv("PWD"), dir, sessions)

	if o.id != "" {
		s, err := findSession(sessions, unreadable, o.id)
		if err != nil {
			return err
		}
		if o.print {
			fmt.Print(renderTurnsPlain(s, origins))
			return nil
		}
		return a.resumeSession(s, origins, live[s.ID], o)
	}
	if o.print || !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		shown := sessions
		if !o.all {
			shown = interactiveOnly(sessions)
		}
		fmt.Print(renderSessionsPlain(dir, shown, live, origins))
		if hidden := len(sessions) - len(shown); hidden > 0 {
			fmt.Printf("(%d worker session%s hidden; --all shows %s)\n", hidden, plural(hidden), map[bool]string{true: "it", false: "them"}[hidden == 1])
		}
		return nil
	}
	b := newBrowser(dir, sessions, live, o.all)
	b.origins = origins
	pick, err := b.run()
	if err != nil || pick == nil {
		return err
	}
	if b.bare {
		o.bare, o.launcher = true, ""
	}
	return a.resumeSession(*pick, origins, live[pick.ID], o)
}

// liveSessions gathers the sessions running right now: open Claude
// processes of the real home realClaude, and aiq long sessions in st whose
// hooks reported a session id, in tmux sessions named with tmuxPrefix.
func liveSessions(st *state.Store, realClaude, tmuxPrefix string) map[string]liveNote {
	out := map[string]liveNote{}
	for id, pid := range transcript.ClaudeOpen(realClaude) {
		out[id] = liveNote{pid: pid, note: fmt.Sprintf("open in pid %d", pid)}
	}
	leases, _ := st.ListLeases()
	for _, l := range leases {
		if l.Mode != state.ModeLong || l.SessionID == "" {
			continue
		}
		if herdr.IsRef(l.Pane) {
			out[l.SessionID] = liveNote{native: l.Pane, note: fmt.Sprintf("running in aiq long (%s, Herdr)", l.AccountID)}
			continue
		}
		name := longrun.SessionName(tmuxPrefix, l.Workspace)
		out[l.SessionID] = liveNote{tmux: name, note: fmt.Sprintf("running in aiq long (%s, %s)", l.AccountID, name)}
	}
	return out
}

func interactiveOnly(list []transcript.Session) []transcript.Session {
	var out []transcript.Session
	for _, s := range list {
		if !s.Worker {
			out = append(out, s)
		}
	}
	return out
}

var errSessionNoMatch = errors.New("no matching session")

type sessionNoMatch string

func (e sessionNoMatch) Error() string        { return string(e) }
func (e sessionNoMatch) Is(target error) bool { return target == errSessionNoMatch }

// findSession finds the one session of list whose id is prefix or starts
// with it. unreadable is what the list could not read: the session asked
// for may be among them, and --all would not list it. The caller reports it.
func findSession(list []transcript.Session, unreadable []string, prefix string) (transcript.Session, error) {
	var hits []transcript.Session
	for _, s := range list {
		if s.ID == prefix {
			return s, nil
		}
		if strings.HasPrefix(s.ID, prefix) {
			hits = append(hits, s)
		}
	}
	switch len(hits) {
	case 0:
		if len(unreadable) > 0 {
			return transcript.Session{}, sessionNoMatch(fmt.Sprintf("no readable session %q in this directory; the transcripts named above could not be read", prefix))
		}
		return transcript.Session{}, sessionNoMatch(fmt.Sprintf("no session %q in this directory (aiq resume --print --all lists them)", prefix))
	case 1:
		return hits[0], nil
	}
	return transcript.Session{}, fmt.Errorf("%q matches %d sessions; give more of the id", prefix, len(hits))
}

// resumeArgs is the CLI command line that reopens s, with the permission
// bypass when bypass is set.
func resumeArgs(s transcript.Session, bypass bool, extra []string) ([]string, error) {
	verb, err := resumeVerb(s.Provider, s.ID)
	if err != nil {
		return nil, err
	}
	var args []string
	if bypass {
		args = append(args, longrun.BypassFlag[s.Provider])
	}
	args = append(args, extra...)
	return append(args, verb...), nil
}

// resumeNote tells the user, in one line, what resume does. The session id and the live
// note come from transcripts and directory names, so the line goes out through oneLine.
func resumeNote(w io.Writer, format string, args ...any) {
	longNote(w, "%s", strings.TrimSuffix(fmt.Sprintf(format, args...), "\n"))
}

// resumeSession closes the app and hands the terminal to the resumed CLI.
func (a *app) resumeSession(s transcript.Session, origins map[string]origin, live liveNote, o resumeOpts) error {
	if live.native != "" {
		err := herdr.Attach(live.native)
		if !errors.Is(err, herdr.ErrEnded) {
			return err
		}
	}

	if live.tmux != "" && tmux.HasSession(live.tmux) {
		resumeNote(os.Stderr, "aiq: session %s is %s; attaching instead of starting a second copy\n", s.ID, live.note)
		return tmux.Attach(live.tmux)
	}
	var org *origin
	if g, ok := origins[sessionKey(s)]; ok {
		org = &g
	}
	launcher, why, err := resumeLauncher(a.cfg, s, org, o)
	if err != nil {
		return err
	}
	if live.pid > 0 {
		resumeNote(os.Stderr, "aiq: note: session %s is also %s\n", s.ID, live.note)
	}
	run := []string{"--mode", state.ModeInteractive}
	if launcher != "" {
		run = append(run, "--launcher", launcher)
	}
	args, err := resumeArgs(s, resumeBypass(s, org, launcher), o.extra)
	if err != nil {
		return err
	}
	how := s.Provider
	if launcher != "" {
		how = launcher
	}
	resumeNote(os.Stderr, "aiq: resuming %s session %s: %s %s\n", s.Provider, s.ID, how, strings.Join(args, " "))
	if why != "" {
		resumeNote(os.Stderr, "aiq: %s\n", why)
	}
	a.close()
	return cmdRun(s.Provider, append(append(run, "--"), args...))
}

// whenLabel is a compact local timestamp: the time today, the weekday this
// week, the date before that.
func whenLabel(t, now time.Time) string {
	return whenLabelIn(t, now, time.Local)
}

func whenLabelIn(t, now time.Time, location *time.Location) string {
	t, now = t.In(location), now.In(location)
	y1, m1, d1 := t.Date()
	y2, m2, d2 := now.Date()
	switch {
	case y1 == y2 && m1 == m2 && d1 == d2:
		return "today " + t.Format("15:04")
	case now.Sub(t) < 6*24*time.Hour:
		return t.Format("Mon 15:04")
	case y1 == y2:
		return t.Format("Jan 2 15:04")
	}
	return t.Format("2006-01-02")
}

// turnTime is a turn's start in layout, or "--" when it has none: a turn whose
// prompt was too long to read takes its time from the next record, which may
// not have come yet.
func turnTime(t time.Time, layout string) string {
	if t.IsZero() {
		return "--"
	}
	return t.Local().Format(layout)
}

// youHeader heads a prompt the person typed, with its time when it has one.
func youHeader(t time.Time) string {
	if t.IsZero() {
		return "▌ you"
	}
	return "▌ you · " + whenLabel(t, time.Now())
}

var modelDate = regexp.MustCompile(`-\d{8}$`)

func agentLabel(s transcript.Session) string {
	model := modelDate.ReplaceAllString(strings.TrimPrefix(s.Model, "claude-"), "")
	if model == "" {
		return oneLine(s.Provider)
	}
	return oneLine(s.Provider + " " + model)
}

func durationLabel(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func homeRel(dir string) string {
	h, _ := os.UserHomeDir()
	return homeRelTo(h, dir)
}

// homeRelTo is dir under home as ~/..., or dir as it is outside it or with no home.
func homeRelTo(home, dir string) string {
	if home != "" {
		if rel, err := filepath.Rel(home, dir); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.Join("~", rel)
		}
	}
	return dir
}

// renderSessionsPlain lists sessions as plain text. What a transcript holds goes out through
// sanitize, as in convo, so it cannot drive the terminal.
func renderSessionsPlain(dir string, list []transcript.Session, live map[string]liveNote, origins map[string]origin) string {
	return sanitize(sessionsPlain(dir, list, live, origins))
}

func sessionsPlain(dir string, list []transcript.Session, live map[string]liveNote, origins map[string]origin) string {
	var b strings.Builder
	if len(list) == 0 {
		fmt.Fprintf(&b, "no sessions in %s\n", oneLine(dir))
		return b.String()
	}
	now := time.Now()
	fmt.Fprintf(&b, "%-8s  %-16s  %-22s  %5s  %5s  %s\n", "ID", "UPDATED", "AGENT", "TURNS", "TOOLS", "NAME / FIRST PROMPT")
	for _, s := range list {
		label := transcript.Clean(s.Label())
		if n, ok := live[s.ID]; ok {
			label = "[" + oneLine(n.note) + "] " + label
		}
		if tag := originTag(originOf(origins, s)); tag != "" {
			label = "[" + tag + "] " + label
		}
		if s.Worker {
			label = "[worker] " + label
		}
		fmt.Fprintf(&b, "%-8s  %-16s  %-22s  %5d  %5d  %s\n", oneLine(shortID(s.ID)), whenLabel(s.Updated, now), truncate(agentLabel(s), 22),
			len(s.Turns), s.Tools(), truncate(label, 100))
	}
	return b.String()
}

// renderTurnsPlain prints every turn of s; one whose prompt came from aiq,
// another agent or a notifier says so in its heading. What a transcript holds goes out
// through sanitize, as in convo, so it cannot drive the terminal.
func renderTurnsPlain(s transcript.Session, origins map[string]origin) string {
	return sanitize(turnsPlain(s, origins))
}

func turnsPlain(s transcript.Session, origins map[string]origin) string {
	var b strings.Builder
	agent := agentLabel(s)
	if tag := originTag(originOf(origins, s)); tag != "" {
		agent += " · " + tag
	}
	fmt.Fprintf(&b, "%s session %s · %s · %s\n", oneLine(s.Provider), oneLine(s.ID), agent, truncate(transcript.Clean(s.Label()), 100))
	for range s.Skipped {
		fmt.Fprintf(&b, "(%s)\n", transcript.SkippedNote())
	}
	for i, t := range s.Turns {
		from := ""
		if t.Source != transcript.Human {
			from = " (" + sourceLabel(t.Source) + ")"
		}
		fmt.Fprintf(&b, "\n## Turn %d%s · %s · %s · %d tools\n\n", i+1, from, turnTime(t.Started, "2006-01-02 15:04"), durationLabel(t.Ended.Sub(t.Started)), t.Tools)
		// sanitize first: it turns line and paragraph separators into line breaks, which
		// the quote then marks.
		shown := 0 // skipped records already shown
		if t.PromptSkipped {
			fmt.Fprintf(&b, "(%s)\n\n", transcript.SkippedNote())
			shown = 1
		} else {
			fmt.Fprintf(&b, "> %s\n\n", strings.ReplaceAll(sanitize(t.Prompt), "\n", "\n> "))
		}
		if r := turnReply(t); r != "" {
			fmt.Fprintf(&b, "%s\n", r)
		} else {
			b.WriteString("(no reply)\n")
		}
		for range t.Skipped - shown {
			fmt.Fprintf(&b, "(%s)\n", transcript.SkippedNote())
		}
	}
	return b.String()
}

// turnReply is the turn's final answer, else (a turn still running, or one
// its CLI never finished) the last answer it gave before going on.
func turnReply(t transcript.Turn) string {
	if t.Reply == "" && len(t.Earlier) > 0 {
		return t.Earlier[len(t.Earlier)-1]
	}
	return t.Reply
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func originOf(origins map[string]origin, s transcript.Session) *origin {
	if g, ok := origins[sessionKey(s)]; ok {
		return &g
	}
	return nil
}
