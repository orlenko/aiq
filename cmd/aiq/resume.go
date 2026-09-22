package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/term"

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

// liveNote says where a session is running now, so resuming it does not
// start a second copy of the same conversation.
type liveNote struct {
	pid  int    // an open Claude process
	tmux string // an aiq long tmux session
	note string
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
	sessions, err := transcript.List(defaultRoots(), dir)
	if err != nil {
		return err
	}
	live := a.liveSessions()
	origins := a.loadOrigins(dir, sessions)

	if o.id != "" {
		s, err := findSession(sessions, o.id)
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
// processes and aiq long sessions whose hooks reported a session id.
func (a *app) liveSessions() map[string]liveNote {
	out := map[string]liveNote{}
	for id, pid := range transcript.ClaudeOpen(paths.RealClaudeHome()) {
		out[id] = liveNote{pid: pid, note: fmt.Sprintf("open in pid %d", pid)}
	}
	leases, _ := a.st.ListLeases()
	for _, l := range leases {
		if l.Mode != state.ModeLong || l.SessionID == "" {
			continue
		}
		name := longrun.SessionName(a.cfg.Long.TmuxPrefix, l.Workspace)
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

func findSession(list []transcript.Session, prefix string) (transcript.Session, error) {
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
		return transcript.Session{}, fmt.Errorf("no session %q in this directory (aiq resume --print --all lists them)", prefix)
	case 1:
		return hits[0], nil
	}
	return transcript.Session{}, fmt.Errorf("%q matches %d sessions; give more of the id", prefix, len(hits))
}

// resumeArgs is the CLI command line that reopens s, with the permission
// bypass when bypass is set.
func resumeArgs(s transcript.Session, bypass bool, extra []string) []string {
	var args []string
	if bypass {
		args = append(args, longrun.BypassFlag[s.Provider])
	}
	args = append(args, extra...)
	return append(args, resumeVerb(s.Provider, s.ID)...)
}

// resumeSession closes the app and hands the terminal to the resumed CLI.
func (a *app) resumeSession(s transcript.Session, origins map[string]origin, live liveNote, o resumeOpts) error {
	if live.tmux != "" && tmux.HasSession(live.tmux) {
		fmt.Fprintf(os.Stderr, "aiq: session %s is %s; attaching instead of starting a second copy\n", s.ID, live.note)
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
		fmt.Fprintf(os.Stderr, "aiq: note: session %s is also %s\n", s.ID, live.note)
	}
	run := []string{"--mode", state.ModeInteractive}
	if launcher != "" {
		run = append(run, "--launcher", launcher)
	}
	args := resumeArgs(s, resumeBypass(s, org, launcher), o.extra)
	how := s.Provider
	if launcher != "" {
		how = launcher
	}
	fmt.Fprintf(os.Stderr, "aiq: resuming %s session %s: %s %s\n", s.Provider, s.ID, how, strings.Join(args, " "))
	if why != "" {
		fmt.Fprintf(os.Stderr, "aiq: %s\n", why)
	}
	a.close()
	return cmdRun(s.Provider, append(append(run, "--"), args...))
}

// whenLabel is a compact local timestamp: the time today, the weekday this
// week, the date before that.
func whenLabel(t, now time.Time) string {
	t, now = t.Local(), now.Local()
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

var modelDate = regexp.MustCompile(`-\d{8}$`)

func agentLabel(s transcript.Session) string {
	model := modelDate.ReplaceAllString(strings.TrimPrefix(s.Model, "claude-"), "")
	if model == "" {
		return s.Provider
	}
	return s.Provider + " " + model
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
	if h, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(h, dir); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.Join("~", rel)
		}
	}
	return dir
}

func renderSessionsPlain(dir string, list []transcript.Session, live map[string]liveNote, origins map[string]origin) string {
	var b strings.Builder
	if len(list) == 0 {
		fmt.Fprintf(&b, "no sessions in %s\n", dir)
		return b.String()
	}
	now := time.Now()
	fmt.Fprintf(&b, "%-8s  %-16s  %-22s  %5s  %5s  %s\n", "ID", "UPDATED", "AGENT", "TURNS", "TOOLS", "NAME / FIRST PROMPT")
	for _, s := range list {
		label := transcript.Clean(s.Label())
		if n, ok := live[s.ID]; ok {
			label = "[" + n.note + "] " + label
		}
		if tag := originTag(originOf(origins, s)); tag != "" {
			label = "[" + tag + "] " + label
		}
		if s.Worker {
			label = "[worker] " + label
		}
		fmt.Fprintf(&b, "%-8s  %-16s  %-22s  %5d  %5d  %s\n", shortID(s.ID), whenLabel(s.Updated, now), truncate(agentLabel(s), 22),
			len(s.Turns), s.Tools(), truncate(label, 100))
	}
	return b.String()
}

func renderTurnsPlain(s transcript.Session, origins map[string]origin) string {
	var b strings.Builder
	agent := agentLabel(s)
	if tag := originTag(originOf(origins, s)); tag != "" {
		agent += " · " + tag
	}
	fmt.Fprintf(&b, "%s session %s · %s · %s\n", s.Provider, s.ID, agent, truncate(transcript.Clean(s.Label()), 100))
	for i, t := range s.Turns {
		fmt.Fprintf(&b, "\n## Turn %d · %s · %s · %d tools\n\n", i+1, t.Started.Local().Format("2006-01-02 15:04"), durationLabel(t.Ended.Sub(t.Started)), t.Tools)
		fmt.Fprintf(&b, "> %s\n\n", strings.ReplaceAll(t.Prompt, "\n", "\n> "))
		if t.Reply != "" {
			fmt.Fprintf(&b, "%s\n", t.Reply)
		} else {
			b.WriteString("(no reply)\n")
		}
	}
	return b.String()
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
