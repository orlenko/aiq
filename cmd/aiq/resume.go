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

const resumeUsage = `usage: aiq resume [--all] [--print] [--launcher <name>] [<session-id>] [-- extra CLI args]
  no id, on a terminal: browse this directory's sessions, read their turns, press r to resume
  <session-id>          resume that session (a unique prefix is enough)
  --print               print the sessions (or, with an id, that session's turns) and exit
  --all                 include worker sessions (claude -p, codex exec)
  --launcher <name>     resume through a registered launcher`

type resumeOpts struct {
	all      bool
	print    bool
	launcher string
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
	if o.launcher != "" {
		if _, ok := a.cfg.Launcher(o.launcher); !ok {
			a.close()
			return configErr("no-launcher", "no launcher %q; see: aiq launcher list", o.launcher)
		}
	}
	dir, err := os.Getwd()
	if err != nil {
		a.close()
		return err
	}
	roots := transcript.DefaultRoots(paths.RealClaudeHome(), paths.RealCodexHome(), paths.ClaudeHomesDir(), paths.CodexHomesDir())
	sessions, err := transcript.List(roots, dir)
	if err != nil {
		a.close()
		return err
	}
	live := a.liveSessions()
	a.close()

	if o.id != "" {
		s, err := findSession(sessions, o.id)
		if err != nil {
			return err
		}
		if o.print {
			fmt.Print(renderTurnsPlain(s))
			return nil
		}
		return resumeSession(s, live[s.ID], o)
	}
	if o.print || !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		shown := sessions
		if !o.all {
			shown = interactiveOnly(sessions)
		}
		fmt.Print(renderSessionsPlain(dir, shown, live))
		if hidden := len(sessions) - len(shown); hidden > 0 {
			fmt.Printf("(%d worker session%s hidden; --all shows %s)\n", hidden, plural(hidden), map[bool]string{true: "it", false: "them"}[hidden == 1])
		}
		return nil
	}
	b := newBrowser(dir, sessions, live, o.all)
	pick, err := b.run()
	if err != nil || pick == nil {
		return err
	}
	return resumeSession(*pick, live[pick.ID], o)
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

// resumeArgs is the CLI command line that reopens s. A session that ran
// with the permission bypass gets it again.
func resumeArgs(s transcript.Session, extra []string) []string {
	var args []string
	switch s.Provider {
	case "claude":
		if s.Bypass {
			args = append(args, "--dangerously-skip-permissions")
		}
		args = append(args, extra...)
		args = append(args, "--resume", s.ID)
	case "codex":
		if s.Bypass {
			args = append(args, "--dangerously-bypass-approvals-and-sandbox")
		}
		args = append(args, extra...)
		args = append(args, "resume", s.ID)
	}
	return args
}

func resumeSession(s transcript.Session, live liveNote, o resumeOpts) error {
	if live.tmux != "" && tmux.HasSession(live.tmux) {
		fmt.Fprintf(os.Stderr, "aiq: session %s is %s; attaching instead of starting a second copy\n", s.ID, live.note)
		return tmux.Attach(live.tmux)
	}
	if live.pid > 0 {
		fmt.Fprintf(os.Stderr, "aiq: note: session %s is also %s\n", s.ID, live.note)
	}
	run := []string{}
	if o.launcher != "" {
		l, _ := launcherByName(o.launcher)
		if l.Provider != s.Provider {
			return configErr("bad-flags", "launcher %s starts %s, but session %s is a %s session", o.launcher, l.Provider, s.ID, s.Provider)
		}
		run = append(run, "--launcher", o.launcher)
	}
	args := resumeArgs(s, o.extra)
	fmt.Fprintf(os.Stderr, "aiq: resuming %s session %s: %s %s\n", s.Provider, s.ID, s.Provider, strings.Join(args, " "))
	return cmdRun(s.Provider, append(append(run, "--mode", state.ModeInteractive, "--"), args...))
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

func renderSessionsPlain(dir string, list []transcript.Session, live map[string]liveNote) string {
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
		if s.Worker {
			label = "[worker] " + label
		}
		fmt.Fprintf(&b, "%-8s  %-16s  %-22s  %5d  %5d  %s\n", shortID(s.ID), whenLabel(s.Updated, now), truncate(agentLabel(s), 22),
			len(s.Turns), s.Tools(), truncate(label, 100))
	}
	return b.String()
}

func renderTurnsPlain(s transcript.Session) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s session %s · %s · %s\n", s.Provider, s.ID, agentLabel(s), truncate(transcript.Clean(s.Label()), 100))
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
