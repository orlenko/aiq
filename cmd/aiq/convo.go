package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/proc"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/tmux"
	"github.com/orlenko/aiq/internal/transcript"
)

const convoUsage = `usage: aiq convo [--pane <id>] [--follow] [--last N] [<session-id>]
  what was typed into an agent session and what the agent answered, without the tool calls
  no id, in tmux    the agent in this pane, else the one live session here
  no id, elsewhere  the one live session here: a Claude session or an aiq long
                    session whose directory is this one or holds it (a plain
                    Codex session is found by id only)
  <session-id>      that session of this directory (a unique prefix is enough)
  --pane <id>       the agent in tmux pane <id> (key bindings pass #{pane_id})
  --follow          keep printing as the conversation goes on (for a side split)
  --last N          only the last N prompts you typed
On a terminal the conversation opens at its end in $PAGER, else less -R +G.

tmux key bindings (add them to ~/.tmux.conf yourself; aiq never installs them):
  # prefix a: the conversation of the pane you are in, in a popup
  %s
  # prefix A: the same, live, in a split beside it
  %s`

// convoBindings are the documented tmux bindings for this aiq binary. Both
// go through run-shell, the one place tmux expands #{pane_id}: neither
// display-popup nor split-window expands formats in its command, and inside
// a popup TMUX_PANE is empty. -EE keeps the popup open when aiq convo fails,
// so its error stays readable (Escape closes it); || true stops run-shell
// from reporting that failure again over the pane. A path these quotes
// cannot carry gives lines that name plain aiq, with a note saying so.
func convoBindings(exe string) (popup, split, note string) {
	if !plainPath.MatchString(exe) {
		exe, note = "aiq", "# aiq must be on the tmux server's PATH: the path to this binary needs quoting these lines can't do"
	}
	popup = fmt.Sprintf(`bind-key a run-shell "tmux display-popup -c '#{client_name}' -w 90%% -h 90%% -EE '%s convo --pane #{pane_id}' || true"`, exe)
	split = fmt.Sprintf(`bind-key A run-shell "tmux split-window -h -d -l 40%% -t '#{pane_id}' '%s convo --follow --pane #{pane_id}'"`, exe)
	return popup, split, note
}

var plainPath = regexp.MustCompile(`^[A-Za-z0-9/._+-]+$`)

type convoOpts struct {
	pane   string
	follow bool
	last   int
	id     string
}

func parseConvoArgs(args []string) (convoOpts, error) {
	var o convoOpts
	for i := 0; i < len(args); i++ {
		a := args[i]
		value := func(name string) (string, error) {
			if v, ok := strings.CutPrefix(a, name+"="); ok {
				return v, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s requires a value", name)
			}
			i++
			return args[i], nil
		}
		switch {
		case a == "--pane" || strings.HasPrefix(a, "--pane="):
			v, err := value("--pane")
			if err != nil {
				return o, err
			}
			if v == "" {
				return o, fmt.Errorf("--pane requires a pane id, such as %%5")
			}
			// tmux prints pane ids as %N; accept the bare number too.
			if _, err := strconv.Atoi(v); err == nil {
				v = "%" + v
			}
			o.pane = v
		case a == "--last" || strings.HasPrefix(a, "--last="):
			v, err := value("--last")
			if err != nil {
				return o, err
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return o, fmt.Errorf("--last takes a number of prompts, not %q", v)
			}
			o.last = n
		case a == "--follow" || a == "-f":
			o.follow = true
		case a == "-h" || a == "--help":
			return o, fmt.Errorf("%s", convoHelp())
		case strings.HasPrefix(a, "-"):
			return o, fmt.Errorf("unknown flag %s\n%s", a, convoHelp())
		case o.id == "":
			o.id = a
		default:
			return o, fmt.Errorf("one session id at a time\n%s", convoHelp())
		}
	}
	if o.id != "" && o.pane != "" {
		return o, fmt.Errorf("--pane and a session id contradict each other")
	}
	return o, nil
}

func convoHelp() string {
	exe, err := os.Executable()
	if err != nil {
		exe = "aiq"
	}
	// On Linux os.Executable resolves symlinks to a versioned path that an
	// upgrade removes; the PATH entry that leads to the same file lasts.
	if lp, err := exec.LookPath("aiq"); err == nil {
		if abs, err := filepath.Abs(lp); err == nil && canonicalPath(abs) == canonicalPath(exe) {
			exe = abs
		}
	}
	popup, split, note := convoBindings(exe)
	help := fmt.Sprintf(convoUsage, popup, split)
	if note != "" {
		help += "\n  " + note
	}
	return help
}

// convoQuiet is how long an Unfinished transcript must go unwritten before
// its last reply counts as final. Current Claude Code closes a turn with
// system records, which ends the wait at once; older versions don't. Across
// 4,143 final messages the thinking and text records were at most 62.5 s
// apart (p99 12 s).
const convoQuiet = 90 * time.Second

func cmdConvo(args []string) error {
	o, err := parseConvoArgs(args)
	if err != nil {
		return configErr("bad-flags", "%v", err)
	}
	r := &convoResolver{roots: defaultRoots()}
	// Leases are only read: the viewer never creates or migrates the
	// database, and works without one for plain Claude sessions.
	if st, err := state.OpenReadOnly(paths.StateDB()); err == nil {
		r.st = st
		defer st.Close()
	}
	tgt, err := r.resolve(o)
	if err != nil {
		return err
	}
	tty := term.IsTerminal(int(os.Stdout.Fd()))
	if o.follow && tgt.provider == "agy" {
		// ParseAgy has no notion of a running turn or of narration, so a
		// live follow would print every planner message as an answer.
		return configErr("bad-flags", "aiq convo --follow cannot follow Antigravity sessions yet; aiq convo without --follow prints one")
	}
	if o.follow {
		f := &convoFollow{r: r, tgt: tgt, w: os.Stdout, tty: tty, last: o.last,
			statEvery: time.Second, parseGap: 2 * time.Second, resolveEvery: 5 * time.Second, quiet: convoQuiet}
		stop := make(chan struct{})
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
		go func() { <-sig; close(stop) }()
		return f.run(stop)
	}
	_, mod := statFile(tgt.path)
	s, err := loadConvo(tgt)
	if err != nil {
		return err
	}
	if s.Unfinished && time.Since(mod) < convoQuiet && len(s.Turns) > 0 {
		// The last reply may still be on its way (see convoFollow.quiet):
		// show the turn as running rather than its narration as the answer.
		last := &s.Turns[len(s.Turns)-1]
		last.Open, last.Reply = true, ""
	}
	text := renderConvo(s, tgt, o.last, tty, time.Now())
	if !tty {
		fmt.Print(text)
		return nil
	}
	return page(text)
}

// page shows text in the user's pager, git's way: $PAGER, else less. Less
// opens at the end, where the newest exchange is.
func page(text string) error {
	pager := os.Getenv("PAGER")
	if pager == "" {
		pager = "less -R"
	}
	if f := strings.Fields(pager); len(f) > 0 && filepath.Base(f[0]) == "less" {
		pager += " +G"
	}
	// Ctrl-C belongs to the pager (it stops a search there), as under git.
	signal.Ignore(os.Interrupt)
	defer signal.Reset(os.Interrupt)
	cmd := exec.Command("sh", "-c", pager)
	cmd.Stdin = strings.NewReader(text)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if os.Getenv("LESS") == "" {
		cmd.Env = append(os.Environ(), "LESS=R")
	}
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() != 127 {
		return nil // the pager's own exit (q, a signal) is not aiq's failure
	}
	if err != nil {
		fmt.Print(text) // no pager to run
	}
	return nil
}

// convoTarget is the transcript a convo reads and where it was found.
type convoTarget struct {
	provider string
	path     string
	account  string // provider/name, when a lease says
	pane     string
	// stale says no running CLI is known to write the transcript (read by
	// id), so a turn left open is one the CLI never finished.
	stale bool
	id    string // the session id it was asked for by, if any
}

func (t convoTarget) label() string {
	if t.account != "" {
		return t.account
	}
	return t.provider
}

type convoResolver struct {
	st    *state.Store // nil without a database
	roots transcript.Roots
}

func (r *convoResolver) resolve(o convoOpts) (convoTarget, error) {
	dir, err := os.Getwd()
	if err != nil {
		return convoTarget{}, err
	}
	if o.id != "" {
		return r.resolveID(dir, o.id)
	}
	if o.pane != "" {
		return r.resolvePane(o.pane)
	}
	if pane := os.Getenv("TMUX_PANE"); pane != "" {
		t, err := r.resolvePane(pane)
		if err == nil {
			return t, nil
		}
		// A shell pane beside the agent: the directory may still name it.
		if t, derr := r.resolveDir(dir); derr == nil {
			return t, nil
		}
		return convoTarget{}, err
	}
	return r.resolveDir(dir)
}

// resolvePane finds the agent running in a tmux pane. Pane ids are reused
// after a tmux restart, so a Claude session file or lease that names the
// pane counts only while its process lives inside that pane.
func (r *convoResolver) resolvePane(pane string) (convoTarget, error) {
	info, err := tmux.PaneInfo(pane)
	if err != nil {
		return convoTarget{}, fmt.Errorf("no tmux pane %s", pane)
	}
	parents := processParents()
	lease, leased := r.paneLease(info, parents)
	// The agent is the Claude closest to the pane's shell; one started
	// under it (an interactive claude a tool opened) runs deeper.
	var in []claudeLive
	for _, c := range liveClaude() {
		if c.inPane(info, parents) {
			in = append(in, c)
		}
	}
	sort.SliceStable(in, func(i, j int) bool { return depth(parents, in[i].PID) < depth(parents, in[j].PID) })
	for _, c := range in {
		t := convoTarget{provider: "claude", pane: info.ID}
		if leased && leaseProvider(lease) == "claude" {
			t.account = lease.AccountID
			if filepath.Base(lease.Transcript) == c.SessionID+".jsonl" && fileExists(lease.Transcript) {
				t.path = lease.Transcript
			}
		}
		if t.path == "" {
			t.path = claudeTranscript(r.roots.ClaudeProjects, c.Cwd, c.SessionID)
		}
		if t.path == "" {
			return convoTarget{}, fmt.Errorf("pane %s runs Claude session %s, which has no transcript yet", info.ID, c.SessionID)
		}
		return t, nil
	}
	if leased {
		if lease.Transcript == "" {
			return convoTarget{}, fmt.Errorf("pane %s runs %s, but no hook has reported its transcript yet", info.ID, lease.AccountID)
		}
		return convoTarget{provider: leaseProvider(lease), path: lease.Transcript, account: lease.AccountID, pane: info.ID}, nil
	}
	return convoTarget{}, fmt.Errorf("pane %s runs %s, not an agent session aiq can find; aiq convo <session-id> reads one by id", info.ID, info.StartCommand)
}

// paneLease is the pane's long lease, if its process is alive and is the
// pane's own process or runs under it.
func (r *convoResolver) paneLease(p tmux.Pane, parents map[int]int) (state.Lease, bool) {
	if r.st == nil {
		return state.Lease{}, false
	}
	l, err := r.st.LongLeaseByPane(p.ID)
	if err != nil || l.Hostname != hostname() || !proc.Alive(l.PID) || !descends(parents, l.PID, p.PID) {
		return state.Lease{}, false
	}
	return l, true
}

// resolveDir finds the one live session in dir: open Claude sessions and
// leases with a transcript.
func (r *convoResolver) resolveDir(dir string) (convoTarget, error) {
	dir = canonicalPath(dir)
	var found []convoTarget
	// A session counts here when it runs in dir or in a directory holding
	// it (but not the home directory or /, which hold everything); the
	// sessions closest to dir win.
	home, _ := os.UserHomeDir()
	home = canonicalPath(home)
	best := -1
	add := func(cwd string, t convoTarget) {
		cwd = canonicalPath(cwd)
		if cwd != dir && (cwd == home || cwd == "/" || !strings.HasPrefix(dir, strings.TrimSuffix(cwd, "/")+"/")) {
			return
		}
		switch {
		case len(cwd) < best:
			return
		case len(cwd) > best:
			best, found = len(cwd), nil
		}
		for i, f := range found {
			if canonicalPath(f.path) == canonicalPath(t.path) {
				if found[i].account == "" {
					found[i].account = t.account
				}
				if found[i].pane == "" {
					found[i].pane = t.pane
				}
				return
			}
		}
		found = append(found, t)
	}
	if r.st != nil {
		leases, _ := r.st.ListLeases()
		for _, l := range leases {
			if l.Transcript != "" && l.Hostname == hostname() && proc.Alive(l.PID) {
				add(l.Cwd, convoTarget{provider: leaseProvider(l), path: l.Transcript, account: l.AccountID, pane: l.Pane})
			}
		}
	}
	for _, c := range liveClaude() {
		if p := claudeTranscript(r.roots.ClaudeProjects, c.Cwd, c.SessionID); p != "" {
			add(c.Cwd, convoTarget{provider: "claude", path: p, pane: c.pane()})
		}
	}
	switch len(found) {
	case 0:
		return convoTarget{}, fmt.Errorf("no live agent session in %s; aiq resume --print lists its sessions, aiq convo <session-id> reads one", homeRel(dir))
	case 1:
		return found[0], nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d live sessions in %s; name one:", len(found), homeRel(dir))
	for _, t := range found {
		fmt.Fprintf(&b, "\n  aiq convo %s", sessionIDOf(t.path))
		if t.pane != "" {
			fmt.Fprintf(&b, "   (or --pane %s)", t.pane)
		}
		fmt.Fprintf(&b, "   %s", t.label())
	}
	return convoTarget{}, fmt.Errorf("%s", b.String())
}

// resolveID finds a session of dir by id, the way aiq resume does.
func (r *convoResolver) resolveID(dir, id string) (convoTarget, error) {
	sessions, err := transcript.List(r.roots, dir)
	if err != nil {
		return convoTarget{}, err
	}
	s, err := findSession(sessions, id)
	if err != nil {
		return convoTarget{}, err
	}
	return convoTarget{provider: s.Provider, path: s.Path, id: s.ID, stale: !r.running(s.ID)}, nil
}

// running reports whether a live Claude process or a lease holds session id.
func (r *convoResolver) running(id string) bool {
	for _, c := range liveClaude() {
		if c.SessionID == id {
			return true
		}
	}
	if r.st == nil {
		return false
	}
	leases, _ := r.st.ListLeases()
	for _, l := range leases {
		if l.SessionID == id && l.Hostname == hostname() && proc.Alive(l.PID) {
			return true
		}
	}
	return false
}

func loadConvo(t convoTarget) (*transcript.Session, error) {
	var s *transcript.Session
	var err error
	switch t.provider {
	case "claude":
		s, err = transcript.ParseClaude(t.path)
	case "codex":
		s, err = transcript.ParseCodex(t.path)
	case "agy":
		s, err = transcript.ParseAgy(t.path)
	default:
		return nil, fmt.Errorf("aiq convo cannot read %s transcripts", t.provider)
	}
	if err == nil && t.stale && len(s.Turns) > 0 && s.Turns[len(s.Turns)-1].Open {
		// The turn ended with the CLI: its last answer is its reply.
		last := &s.Turns[len(s.Turns)-1]
		last.Open = false
		if last.Reply == "" && len(last.Earlier) > 0 {
			last.Reply = last.Earlier[len(last.Earlier)-1]
			last.Earlier = last.Earlier[:len(last.Earlier)-1]
		}
	}
	return s, err
}

func leaseProvider(l state.Lease) string {
	if l.Provider != "" {
		return l.Provider
	}
	p, _, _ := strings.Cut(l.AccountID, "/")
	return p
}

// sessionIDOf is the session id in a transcript's file name: Claude's
// <id>.jsonl, Codex's rollout-<time>-<id>.jsonl.
func sessionIDOf(path string) string {
	// Antigravity: brain/<id>/.system_generated/logs/transcript.jsonl
	if filepath.Base(path) == "transcript.jsonl" {
		if dir := filepath.Dir(filepath.Dir(filepath.Dir(path))); filepath.Base(filepath.Dir(dir)) == "brain" {
			return filepath.Base(dir)
		}
	}
	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if strings.HasPrefix(base, "rollout-") && len(base) > 36 {
		return base[len(base)-36:]
	}
	return base
}

func canonicalPath(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// claudeLive is one sessions/<pid>.json Claude Code keeps while it runs.
type claudeLive struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"` // follows /clear
	Cwd       string `json:"cwd"`
	Tmux      string `json:"tmux"` // "<session>:@<window>.%<pane>", inside tmux
	// Entrypoint is "sdk-cli" for claude -p. A worker an agent starts
	// inherits its pane and directory, so it must not pass for the agent.
	Entrypoint string `json:"entrypoint"`
}

// liveClaude reads the session files of running Claude processes, from the
// real home and from any overlay home whose sessions/ is not linked to it.
func liveClaude() []claudeLive {
	dirs := []string{filepath.Join(paths.RealClaudeHome(), "sessions")}
	homes, _ := os.ReadDir(paths.ClaudeHomesDir())
	for _, h := range homes {
		d := filepath.Join(paths.ClaudeHomesDir(), h.Name(), "sessions")
		if info, err := os.Lstat(d); err == nil && info.IsDir() {
			dirs = append(dirs, d)
		}
	}
	var out []claudeLive
	seen := map[int]bool{}
	for _, d := range dirs {
		files, _ := filepath.Glob(filepath.Join(d, "*.json"))
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			var c claudeLive
			if json.Unmarshal(data, &c) != nil || c.PID <= 0 || c.SessionID == "" || seen[c.PID] || !proc.Alive(c.PID) || strings.HasPrefix(c.Entrypoint, "sdk") {
				continue
			}
			seen[c.PID] = true
			out = append(out, c)
		}
	}
	return out
}

// pane is the tmux pane id the session file names, if any.
func (c claudeLive) pane() string {
	_, rest, _ := strings.Cut(c.Tmux, ":")
	_, pane, _ := strings.Cut(rest, ".")
	return pane
}

// inPane reports whether the session file names pane p and its process
// runs in that pane. The tmux session name is not compared: a renamed
// session keeps its panes.
func (c claudeLive) inPane(p tmux.Pane, parents map[int]int) bool {
	return c.pane() != "" && c.pane() == p.ID && descends(parents, c.PID, p.PID)
}

var claudeNonAlnum = regexp.MustCompile(`[^a-zA-Z0-9]`)

// claudeTranscript finds <id>.jsonl in the project directory Claude Code
// keeps for cwd, else in any project (cwd spelled through a symlink).
func claudeTranscript(roots []string, cwd, id string) string {
	enc := claudeNonAlnum.ReplaceAllString(cwd, "-")
	if len(enc) > 200 {
		enc = enc[:200] + "*" // Claude Code cuts long names and adds a hash
	}
	for _, pattern := range []string{enc, "*"} {
		for _, root := range roots {
			if m, _ := filepath.Glob(filepath.Join(root, pattern, id+".jsonl")); len(m) > 0 {
				return m[0]
			}
		}
	}
	return ""
}

// processParents maps every process to its parent, from one ps call.
func processParents() map[int]int {
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=").Output()
	if err != nil {
		return nil
	}
	parents := map[int]int{}
	for _, line := range strings.Split(string(out), "\n") {
		var pid, ppid int
		if _, err := fmt.Sscan(line, &pid, &ppid); err == nil {
			parents[pid] = ppid
		}
	}
	return parents
}

// descends reports whether pid is ancestor or runs under it.
// depth is how many parents pid has.
func depth(parents map[int]int, pid int) int {
	n := 0
	for ; pid > 1 && n < 64; n++ {
		pid = parents[pid]
	}
	return n
}

func descends(parents map[int]int, pid, ancestor int) bool {
	for i := 0; pid > 1 && i < 64; i++ {
		if pid == ancestor {
			return true
		}
		pid = parents[pid]
	}
	return false
}

// --- rendering ---

const (
	sgrBoldCyan  = "\x1b[1;36m"
	sgrBoldGreen = "\x1b[1;32m"
	// osc133Prompt marks the start of a prompt, so tmux copy-mode's
	// previous-prompt and next-prompt jump between what the user typed.
	osc133Prompt = "\x1b]133;A\x1b\\"
)

// convoPrinter writes a conversation append-only: each piece once, in the
// order it turned up, so a snapshot and a --follow run print the same text.
type convoPrinter struct {
	provider string
	color    bool
	marks    bool
	// live says the printer follows a running session: the last turn's
	// "(no reply)" is never printed, since more may come.
	live bool
	// hold keeps back the last turn's final reply: the transcript may
	// still be writing it (Session.Unfinished and the file not yet quiet).
	// Claude Code writes thinking and text as separate records, seconds
	// apart, and a parse between them sees the narration before them as
	// the reply.
	hold bool
	// pending says emit held a reply back; parse again even if the file
	// has not changed.
	pending bool
	seen    map[string]*turnSeen
	// piece, if set, hears every text printed: the turn's key, the kind
	// (prompt, steer, answer, none) and the text.
	piece func(key, kind, text string)
}

type turnSeen struct {
	hidden  bool // left out by --last
	head    bool
	steers  int
	answers map[string]int // how often each agent text was printed: the parser can move one between Earlier and Reply, and an agent can say the same thing twice
	noReply bool
}

func newConvoPrinter(provider string, color, marks bool) *convoPrinter {
	return &convoPrinter{provider: provider, color: color, marks: marks, seen: map[string]*turnSeen{}}
}

// skip marks turns as already shown (those --last leaves out).
func (p *convoPrinter) skip(turns []transcript.Turn) {
	for i, t := range turns {
		p.seen[turnKey(i, t)] = &turnSeen{hidden: true}
	}
}

// emit returns what of s has not been printed yet. A turn shows its prompt
// and steers as they come and each answer once, by text; a turn that ended
// can go on (a background task or a peer message wakes the agent), and its
// new answers follow what was printed. A prompt aiq, a peer or a notifier
// sent is a dim line, its answers dim one-liners under it, unless the
// person typed into that turn: then its answers print in full.
func (p *convoPrinter) emit(s *transcript.Session) string {
	var b strings.Builder
	p.pending = false
	for i, t := range s.Turns {
		k := turnKey(i, t)
		seen := p.seen[k]
		if seen == nil {
			seen = &turnSeen{answers: map[string]int{}}
			p.seen[k] = seen
		}
		if seen.hidden || idleNotice(t) {
			continue
		}
		answers := t.Earlier
		if t.Reply != "" {
			answers = append(answers[:len(answers):len(answers)], t.Reply)
		}
		human := t.Source == transcript.Human
		if !seen.head {
			if human && t.Open && t.Tools == 0 && len(answers) == 0 && slashCommand(t.Prompt) {
				continue // a local command: the parser drops it if nothing comes of it
			}
			if human {
				p.block(&b, true, sgrBoldCyan, "▌ you · "+whenLabel(t.Started, time.Now()), sgrBold, t.Prompt)
			} else {
				b.WriteString("\n" + p.paint(sgrDim, "· "+sourceLabel(t.Source)+": "+firstLine(t.Prompt)) + "\n")
			}
			p.note(k, "prompt", t.Prompt)
			seen.head = true
		}
		for ; seen.steers < len(t.Steers); seen.steers++ {
			p.block(&b, true, sgrBoldCyan, "▌ you, while it worked", sgrBold, t.Steers[seen.steers])
			p.note(k, "steer", t.Steers[seen.steers])
		}
		head := "▌ " + p.provider
		if !t.Started.IsZero() && !t.Ended.IsZero() {
			head += " · " + durationLabel(t.Ended.Sub(t.Started))
		}
		if t.Tools > 0 {
			head += fmt.Sprintf(" · %d tool%s", t.Tools, plural(t.Tools))
		}
		last := i == len(s.Turns)-1
		occ := map[string]int{}
		for j, a := range answers {
			if occ[a]++; occ[a] <= seen.answers[a] {
				continue
			}
			final := j == len(answers)-1 && t.Reply != ""
			if final && last && p.hold {
				p.pending = true
				continue
			}
			seen.answers[a]++
			switch {
			case !human && len(t.Steers) == 0:
				b.WriteString(p.paint(sgrDim, "  → "+firstLine(a)) + "\n")
			case final:
				p.block(&b, false, sgrBoldGreen, head, "", a)
			default:
				p.block(&b, false, sgrDim, "▌ "+p.provider+" · earlier", sgrDim, a)
			}
			p.note(k, "answer", a)
		}
		if human && !t.Open && len(seen.answers) == 0 && !seen.noReply && !(p.live && last) {
			p.block(&b, false, sgrBoldGreen, head, sgrDim, "(no reply)")
			p.note(k, "none", "")
			seen.noReply = true
		}
	}
	return b.String()
}

func (p *convoPrinter) note(key, kind, text string) {
	if p.piece != nil {
		p.piece(key, kind, text)
	}
}

// block writes a header line and its text after a blank line. A human
// block carries the prompt mark.
func (p *convoPrinter) block(b *strings.Builder, human bool, headSGR, head, bodySGR, body string) {
	b.WriteString("\n")
	if human && p.marks {
		b.WriteString(osc133Prompt)
	}
	b.WriteString(p.paint(headSGR, head) + "\n")
	b.WriteString(p.paint(bodySGR, strings.TrimRight(body, "\n")) + "\n")
}

// paint colours each line on its own, so a pager that starts mid-block
// still shows it right. Everything it paints is cleaned of control
// characters first.
func (p *convoPrinter) paint(sgr, text string) string {
	text = sanitize(text)
	if !p.color || sgr == "" {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = sgr + l + sgrReset
		}
	}
	return strings.Join(lines, "\n")
}

// sanitize drops control characters from text a transcript holds, so
// nothing an agent or a pasted file wrote can move the cursor, retitle the
// terminal, set the clipboard (OSC 52) or fake a prompt mark. Newlines and
// tabs stay; bytes that are not UTF-8 become U+FFFD.
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r == 0x2028 || r == 0x2029: // line and paragraph separators
			return '\n'
		case r < 0x20 || r == 0x7f || r >= 0x80 && r <= 0x9f:
			return -1
		case r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069 || r == 0x200e || r == 0x200f || r == 0x061c:
			return -1 // bidi embeddings and isolates, which can reorder what a terminal shows
		}
		return r
	}, s)
}

// slashCommand reports a prompt that is a CLI command (/model, /mcp
// list), as opposed to one that merely starts with a path.
func slashCommand(prompt string) bool {
	word, _, _ := strings.Cut(strings.TrimSpace(prompt), " ")
	return len(word) > 1 && word[0] == '/' && !strings.Contains(word[1:], "/")
}

// firstLine is the first non-blank line of s, cut to fit a one-liner.
func firstLine(s string) string {
	s = strings.TrimSpace(sanitize(s))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncate(strings.TrimSpace(s), 100)
}

// turnKey names a turn across re-parses: by its start, which does not move
// when the file grows.
func turnKey(i int, t transcript.Turn) string {
	if t.Started.IsZero() {
		return fmt.Sprintf("#%d", i)
	}
	return fmt.Sprintf("%d|%s", t.Started.UnixNano(), truncate(t.Prompt, 40))
}

// idleNotice is an agent-team teammate reporting it went idle: a peer
// message with nothing in it for the person reading.
func idleNotice(t transcript.Turn) bool {
	return t.Source == transcript.Peer && strings.HasPrefix(t.Prompt, `{"type":"idle_notification"`)
}

func sourceLabel(s transcript.Source) string {
	switch s {
	case transcript.Aiq:
		return "aiq"
	case transcript.Peer:
		return "peer agent"
	case transcript.Notice:
		return "notice"
	}
	return "you"
}

// convoStart is the index of the first turn --last N shows: the Nth human
// prompt from the end, with the notices after it.
func convoStart(turns []transcript.Turn, last int) int {
	if last <= 0 {
		return 0
	}
	n := 0
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Source == transcript.Human {
			if n++; n == last {
				return i
			}
		}
	}
	return 0
}

func convoHeader(s *transcript.Session, t convoTarget, hidden int, color bool) string {
	parts := []string{agentLabel(*s)}
	if t.account != "" {
		parts = append(parts, t.account)
	}
	if s.Cwd != "" {
		parts = append(parts, homeRel(s.Cwd))
	}
	parts = append(parts, "session "+shortID(s.ID))
	if t.pane != "" {
		parts = append(parts, "pane "+t.pane)
	}
	head := sanitize(strings.Join(parts, " · "))
	if color {
		head = sgrBold + head + sgrReset
	}
	if hidden > 0 {
		head += fmt.Sprintf("\n(%d earlier turn%s not shown)", hidden, plural(hidden))
	}
	return head + "\n"
}

// workingLabel describes the running turn: "working · 12m · 47 tools".
func workingLabel(s *transcript.Session, now time.Time) string {
	if len(s.Turns) == 0 || !s.Turns[len(s.Turns)-1].Open {
		return ""
	}
	t := s.Turns[len(s.Turns)-1]
	out := "working"
	if !t.Started.IsZero() {
		out += " · " + durationLabel(now.Sub(t.Started))
	}
	return out + fmt.Sprintf(" · %d tool%s", t.Tools, plural(t.Tools))
}

// renderConvo is the snapshot: the header, the turns --last keeps, and a
// line saying the agent is still at it.
func renderConvo(s *transcript.Session, t convoTarget, last int, color bool, now time.Time) string {
	from := convoStart(s.Turns, last)
	p := newConvoPrinter(s.Provider, color, false)
	p.skip(s.Turns[:from])
	out := convoHeader(s, t, from, color) + p.emit(s)
	if w := workingLabel(s, now); w != "" {
		out += "\n" + p.paint(sgrYellow, w) + "\n"
	}
	return out
}

// convoFollow prints a conversation as it grows, for a side split. Output
// is append-only; on a terminal the bottom line is a status line that is
// rewritten in place and erased before anything new is printed.
type convoFollow struct {
	r    *convoResolver // re-resolves the pane, when following one
	tgt  convoTarget
	w    io.Writer
	tty  bool
	last int

	statEvery    time.Duration // how often to look at the file
	parseGap     time.Duration // minimum time between re-parses
	resolveEvery time.Duration // how often to ask the pane what it runs
	quiet        time.Duration // see convoQuiet

	status string // on screen now
}

func (f *convoFollow) run(stop <-chan struct{}) error {
	// The file is looked at before each parse, never after, so a write
	// that lands during a parse shows as a change on the next tick.
	size, mod := statFile(f.tgt.path)
	s, err := loadConvo(f.tgt)
	if err != nil {
		return err
	}
	p := f.printer(s)
	from := convoStart(s.Turns, f.last)
	p.skip(s.Turns[:from])
	f.print(convoHeader(s, f.tgt, from, f.tty) + f.emit(p, s, mod))

	lastParse, lastResolve := time.Now(), time.Now()
	dirty := p.pending
	note := ""
	tick := time.NewTicker(f.statEvery)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			f.setStatus("")
			return nil
		case <-tick.C:
		}
		now := time.Now()
		if f.r != nil && f.tgt.pane == "" && f.tgt.id != "" && now.Sub(lastResolve) >= f.resolveEvery {
			// Read by id: the CLI that writes it can start or stop.
			lastResolve = now
			if stale := !f.r.running(f.tgt.id); stale != f.tgt.stale {
				f.tgt.stale = stale
				dirty, lastParse = true, time.Time{}
			}
		}
		if f.r != nil && f.tgt.pane != "" && now.Sub(lastResolve) >= f.resolveEvery {
			lastResolve = now
			t, err := f.r.resolvePane(f.tgt.pane)
			switch {
			case err != nil:
				note = err.Error()
			case canonicalPath(t.path) != canonicalPath(f.tgt.path):
				sz, m := statFile(t.path)
				ns, err := loadConvo(t)
				if err != nil {
					note = err.Error()
					break
				}
				note = ""
				f.tgt, s = t, ns
				p = f.printer(s)
				from := convoStart(s.Turns, f.last)
				p.skip(s.Turns[:from])
				line := fmt.Sprintf("── moved to %s · session %s · %s ──", t.label(), shortID(s.ID), now.Format("15:04"))
				if from > 0 {
					line += fmt.Sprintf("\n(%d earlier turn%s not shown)", from, plural(from))
				}
				f.print("\n" + p.paint(sgrYellow, line) + "\n" + f.emit(p, s, m))
				size, mod = sz, m
				lastParse, dirty = now, p.pending
			default:
				note = ""
				f.tgt.account = t.account
			}
		}
		if sz, m := statFile(f.tgt.path); sz != size || !m.Equal(mod) {
			size, mod, dirty = sz, m, true
		}
		if dirty && now.Sub(lastParse) >= f.parseGap {
			lastParse, dirty = now, false
			if ns, err := loadConvo(f.tgt); err == nil {
				s = ns
				f.print(f.emit(p, s, mod))
			}
			dirty = p.pending
		}
		// A pane that no longer answers says more than a turn that may
		// never end now.
		status := note
		if status == "" {
			status = workingLabel(s, now)
		}
		f.setStatus(status)
	}
}

// emit prints what s adds, holding the last reply while the transcript,
// last written at mod, may still be writing it.
func (f *convoFollow) emit(p *convoPrinter, s *transcript.Session, mod time.Time) string {
	p.hold = s.Unfinished && time.Since(mod) < f.quiet
	return p.emit(s)
}

func (f *convoFollow) printer(s *transcript.Session) *convoPrinter {
	p := newConvoPrinter(s.Provider, f.tty, f.tty)
	p.live = true
	return p
}

// print writes new conversation text, erasing the status line first.
func (f *convoFollow) print(text string) {
	if text == "" {
		return
	}
	if f.status != "" {
		fmt.Fprint(f.w, "\r\x1b[K")
		f.status = ""
	}
	fmt.Fprint(f.w, text)
}

// setStatus rewrites the status line in place. It never ends in a newline
// and is cut to fit, so \r always returns to its start.
func (f *convoFollow) setStatus(s string) {
	s = sanitize(s)
	if !f.tty || s == f.status {
		return
	}
	width := 80
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 1 {
		width = w
	}
	f.status = s
	fmt.Fprint(f.w, "\r\x1b[K")
	if s != "" {
		fmt.Fprint(f.w, sgrDim+truncate(s, width-1)+sgrReset)
	}
}

func statFile(path string) (int64, time.Time) {
	info, err := os.Stat(path)
	if err != nil {
		return -1, time.Time{}
	}
	return info.Size(), info.ModTime()
}
