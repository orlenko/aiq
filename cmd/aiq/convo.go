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
  no id, in tmux    the agent in this pane, else the one live session in this directory
  no id, elsewhere  the one live session in this directory
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
// from reporting that failure again over the pane.
func convoBindings(exe string) (popup, split string) {
	popup = fmt.Sprintf(`bind-key a run-shell "tmux display-popup -c '#{client_name}' -w 90%% -h 90%% -EE '%s convo --pane #{pane_id}' || true"`, exe)
	split = fmt.Sprintf(`bind-key A run-shell "tmux split-window -h -d -l 40%% -t '#{pane_id}' '%s convo --follow --pane #{pane_id}'"`, exe)
	return popup, split
}

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
	popup, split := convoBindings(exe)
	return fmt.Sprintf(convoUsage, popup, split)
}

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
	s, err := loadConvo(tgt)
	if err != nil {
		return err
	}
	tty := term.IsTerminal(int(os.Stdout.Fd()))
	if o.follow {
		f := &convoFollow{r: r, tgt: tgt, w: os.Stdout, tty: tty, last: o.last,
			statEvery: time.Second, parseGap: 2 * time.Second, resolveEvery: 5 * time.Second}
		stop := make(chan struct{})
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
		go func() { <-sig; close(stop) }()
		return f.run(s, stop)
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
	if err := cmd.Run(); err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return nil // the pager's own exit (q, a signal) is not aiq's failure
		}
		fmt.Print(text)
	}
	return nil
}

// convoTarget is the transcript a convo reads and where it was found.
type convoTarget struct {
	provider string
	path     string
	account  string // provider/name, when a lease says
	pane     string
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
	for _, c := range liveClaude() {
		if !c.inPane(info, parents) {
			continue
		}
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
	if err != nil || !proc.Alive(l.PID) || !descends(parents, l.PID, p.PID) {
		return state.Lease{}, false
	}
	return l, true
}

// resolveDir finds the one live session in dir: open Claude sessions and
// leases with a transcript.
func (r *convoResolver) resolveDir(dir string) (convoTarget, error) {
	dir = canonicalPath(dir)
	var found []convoTarget
	add := func(t convoTarget) {
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
			if l.Transcript != "" && proc.Alive(l.PID) && canonicalPath(l.Cwd) == dir {
				add(convoTarget{provider: leaseProvider(l), path: l.Transcript, account: l.AccountID, pane: l.Pane})
			}
		}
	}
	for _, c := range liveClaude() {
		if canonicalPath(c.Cwd) != dir {
			continue
		}
		if p := claudeTranscript(r.roots.ClaudeProjects, c.Cwd, c.SessionID); p != "" {
			add(convoTarget{provider: "claude", path: p})
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
	return convoTarget{provider: s.Provider, path: s.Path}, nil
}

func loadConvo(t convoTarget) (*transcript.Session, error) {
	switch t.provider {
	case "claude":
		return transcript.ParseClaude(t.path)
	case "codex":
		return transcript.ParseCodex(t.path)
	case "agy":
		return transcript.ParseAgy(t.path)
	}
	return nil, fmt.Errorf("aiq convo cannot read %s transcripts", t.provider)
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
			if json.Unmarshal(data, &c) != nil || c.PID <= 0 || c.SessionID == "" || seen[c.PID] || !proc.Alive(c.PID) {
				continue
			}
			seen[c.PID] = true
			out = append(out, c)
		}
	}
	return out
}

// inPane reports whether the session file names pane p of p's tmux session
// and its process runs in that pane.
func (c claudeLive) inPane(p tmux.Pane, parents map[int]int) bool {
	sess, rest, ok := strings.Cut(c.Tmux, ":")
	if !ok {
		return false
	}
	_, pane, ok := strings.Cut(rest, ".")
	return ok && sess == p.Session && pane == p.ID && descends(parents, c.PID, p.PID)
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
	seen     map[string]*turnSeen
}

type turnSeen struct {
	head    bool
	steers  int
	earlier int
	done    bool
}

func newConvoPrinter(provider string, color, marks bool) *convoPrinter {
	return &convoPrinter{provider: provider, color: color, marks: marks, seen: map[string]*turnSeen{}}
}

// skip marks turns as already shown (those --last leaves out).
func (p *convoPrinter) skip(turns []transcript.Turn) {
	for i, t := range turns {
		p.seen[turnKey(i, t)] = &turnSeen{done: true}
	}
}

// emit returns what of s has not been printed yet. A turn still running
// shows its prompt, steers and earlier answers; its reply, or a notice's
// one-line summary, waits until it ends.
func (p *convoPrinter) emit(s *transcript.Session) string {
	var b strings.Builder
	for i, t := range s.Turns {
		k := turnKey(i, t)
		seen := p.seen[k]
		if seen == nil {
			seen = &turnSeen{}
			p.seen[k] = seen
		}
		if seen.done {
			continue
		}
		if t.Source != transcript.Human {
			if !t.Open {
				line := fmt.Sprintf("· %s: %s", sourceLabel(t.Source), truncate(transcript.Clean(t.Prompt), 70))
				if t.Reply != "" {
					line += " → " + truncate(transcript.Clean(t.Reply), 100)
				}
				b.WriteString("\n" + p.paint(sgrDim, line) + "\n")
				seen.done = true
			}
			continue
		}
		if !seen.head {
			p.block(&b, true, sgrBoldCyan, "▌ you · "+whenLabel(t.Started, time.Now()), sgrBold, t.Prompt)
			seen.head = true
		}
		for ; seen.steers < len(t.Steers); seen.steers++ {
			p.block(&b, true, sgrBoldCyan, "▌ you, while it worked", sgrBold, t.Steers[seen.steers])
		}
		for ; seen.earlier < len(t.Earlier); seen.earlier++ {
			p.block(&b, false, sgrDim, "▌ "+p.provider+" · earlier", sgrDim, t.Earlier[seen.earlier])
		}
		if t.Open {
			continue
		}
		head := "▌ " + p.provider
		if !t.Started.IsZero() && !t.Ended.IsZero() {
			head += " · " + durationLabel(t.Ended.Sub(t.Started))
		}
		if t.Tools > 0 {
			head += fmt.Sprintf(" · %d tool%s", t.Tools, plural(t.Tools))
		}
		if t.Reply != "" {
			p.block(&b, false, sgrBoldGreen, head, "", t.Reply)
		} else {
			p.block(&b, false, sgrBoldGreen, head, sgrDim, "(no reply)")
		}
		seen.done = true
	}
	return b.String()
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
// still shows it right.
func (p *convoPrinter) paint(sgr, text string) string {
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

// turnKey names a turn across re-parses: by its start, which does not move
// when the file grows.
func turnKey(i int, t transcript.Turn) string {
	if t.Started.IsZero() {
		return fmt.Sprintf("#%d", i)
	}
	return fmt.Sprintf("%d|%s", t.Started.UnixNano(), truncate(t.Prompt, 40))
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
	head := strings.Join(parts, " · ")
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

	status string // on screen now
}

func (f *convoFollow) run(s *transcript.Session, stop <-chan struct{}) error {
	p := newConvoPrinter(s.Provider, f.tty, f.tty)
	from := convoStart(s.Turns, f.last)
	p.skip(s.Turns[:from])
	f.print(convoHeader(s, f.tgt, from, f.tty) + p.emit(s))

	size, mod := statFile(f.tgt.path)
	lastParse, lastResolve := time.Now(), time.Now()
	dirty := false
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
		if f.r != nil && f.tgt.pane != "" && now.Sub(lastResolve) >= f.resolveEvery {
			lastResolve = now
			t, err := f.r.resolvePane(f.tgt.pane)
			switch {
			case err != nil:
				note = err.Error()
			case canonicalPath(t.path) != canonicalPath(f.tgt.path):
				ns, err := loadConvo(t)
				if err != nil {
					note = err.Error()
					break
				}
				note = ""
				f.tgt, s = t, ns
				p = newConvoPrinter(s.Provider, f.tty, f.tty)
				line := fmt.Sprintf("── moved to %s · session %s · %s ──", t.label(), shortID(s.ID), now.Format("15:04"))
				f.print("\n" + p.paint(sgrYellow, line) + "\n" + p.emit(s))
				size, mod = statFile(t.path)
				lastParse, dirty = now, false
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
				f.print(p.emit(s))
			}
		}
		status := workingLabel(s, now)
		if status == "" && note != "" {
			status = note
		}
		f.setStatus(status)
	}
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
