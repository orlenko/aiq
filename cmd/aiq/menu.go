package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/longrun"
	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/tier"
	"github.com/orlenko/aiq/internal/tmux"
	"github.com/orlenko/aiq/internal/transcript"
)

// The start menu is what `aiq` with no arguments opens on a terminal. It
// composes one ordinary aiq command from a few choices, shows that command
// as it changes, and on Enter runs it in place of itself. It adds no launch
// path of its own: everything it can start, the command line can too.

// menuChoice is what the menu composes, and what it remembers between runs
// so that Enter alone repeats the last start.
type menuChoice struct {
	Session string `json:"session"` // new | resume
	Agent   string `json:"agent"`   // auto | claude | codex | agy | <launcher>
	Length  string `json:"length"`  // short | long
	Tier    int    `json:"tier"`    // -1: default
	Effort  int    `json:"effort"`  // 0: default
	Account string `json:"account"` // "": routed
	Bypass  bool   `json:"bypass"`  // for a named provider; auto always bypasses
}

func defaultChoice() menuChoice {
	return menuChoice{Session: "new", Agent: "auto", Length: "short", Tier: -1}
}

// menuAccount is one account the Account row offers.
type menuAccount struct {
	name     string
	provider string
	note     string // headroom, or why it is not eligible
}

// menuEnv is what the menu knows about this machine and directory.
type menuEnv struct {
	dir       string
	launchers map[string]string // launcher name -> provider
	accounts  map[string][]menuAccount
	routed    map[string]string // provider -> account a routed start would take now
	longHere  string            // account of the long session running in this workspace
	sessions  *menuSessions     // nil until loaded
}

type menuSessions struct {
	count  int
	latest transcript.Session
}

// accountChoices are the accounts the Account row offers. Resuming, the
// session's provider is not known until the pick, so every pool is on
// offer and the value is the account's id rather than its bare name.
func (e *menuEnv) accountChoices(c menuChoice) []menuAccount {
	if c.Session != "resume" {
		return e.accounts[e.provider(c.Agent)]
	}
	var out []menuAccount
	for _, p := range config.Providers {
		out = append(out, e.accounts[p]...)
	}
	return out
}

// accountValue is what --account carries for a, and accountLabel what the
// row shows: the bare name, qualified when it would otherwise be ambiguous.
func accountValue(c menuChoice, a menuAccount) string {
	if c.Session == "resume" {
		return state.AccountID(a.provider, a.name)
	}
	return a.name
}

func accountLabel(a menuAccount, all []menuAccount) string {
	for _, o := range all {
		if o.name == a.name && o.provider != a.provider {
			return state.AccountID(a.provider, a.name)
		}
	}
	return a.name
}

func (e *menuEnv) provider(agent string) string {
	if knownProvider(agent) {
		return agent
	}
	return e.launchers[agent]
}

// normalize drops remembered values that no longer apply.
func (e *menuEnv) normalize(c menuChoice) menuChoice {
	d := defaultChoice()
	if c.Session != "new" && c.Session != "resume" {
		c.Session = d.Session
	}
	if c.Length != "short" && c.Length != "long" {
		c.Length = d.Length
	}
	if c.Agent != "auto" && e.provider(c.Agent) == "" {
		c.Agent = d.Agent
	}
	if c.Tier < -1 || c.Tier > 3 {
		c.Tier = -1
	}
	if c.Effort < 0 || c.Effort > 6 {
		c.Effort = 0
	}
	if c.Account != "" {
		found := false
		for _, a := range e.accountChoices(c) {
			found = found || accountValue(c, a) == c.Account
		}
		if !found {
			c.Account = ""
		}
	}
	return c
}

// menuArgs is the aiq command line a choice stands for.
func menuArgs(c menuChoice, e *menuEnv) []string {
	var model []string
	if c.Tier >= 0 {
		model = append(model, "--model-tier", strconv.Itoa(c.Tier))
	}
	if c.Effort > 0 {
		model = append(model, "--effort", strconv.Itoa(c.Effort))
	}
	if c.Session == "resume" {
		if c.Length == "long" {
			args := []string{"long", "auto", "resume"}
			if c.Account != "" {
				args = append(args, "--account", c.Account)
			}
			return append(args, model...)
		}
		return []string{"resume"}
	}
	if c.Length == "long" && e.longHere != "" {
		return []string{"long", "attach"}
	}
	if c.Agent == "auto" {
		verb := "run"
		if c.Length == "long" {
			verb = "long"
		}
		return append([]string{verb, "auto"}, model...)
	}
	provider := e.provider(c.Agent)
	var cli []string
	if c.Bypass {
		cli = append(cli, bypassFlag(provider))
	}
	if c.Length == "long" {
		args := []string{"long", c.Agent}
		if c.Account != "" {
			args = append(args, "--account", c.Account)
		}
		args = append(args, model...)
		if len(cli) > 0 {
			args = append(append(args, "--"), cli...)
		}
		return args
	}
	if c.Account == "" && len(model) == 0 {
		return append([]string{c.Agent}, cli...) // aiq claude / aiq <launcher>
	}
	args := []string{"run", provider}
	if c.Account != "" {
		args = append(args, "--account", c.Account)
	}
	if c.Agent != provider {
		args = append(args, "--launcher", c.Agent)
	}
	args = append(args, model...)
	return append(append(args, "--"), cli...)
}

// --- rows ---

type menuOpt struct{ value, text string }

type menuRow struct {
	key   string
	label string
	opts  []menuOpt
	cur   string // selected value
	off   string // why the row does not apply; empty when it does
}

type menu struct {
	env      *menuEnv
	c        menuChoice
	expanded bool
	focus    string // key of the focused row
	w, h     int
}

func (m *menu) rows() []menuRow {
	c, e := m.c, m.env
	resume := c.Session == "resume"

	agents := []menuOpt{{"auto", "Any"}}
	for _, p := range config.Providers {
		agents = append(agents, menuOpt{p, cliName(p)})
	}
	var names []string
	for n := range e.launchers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		agents = append(agents, menuOpt{n, n})
	}
	rows := []menuRow{
		{key: "session", label: "Session", opts: []menuOpt{{"new", "New"}, {"resume", "Resume"}}, cur: c.Session},
		{key: "agent", label: "Agent", opts: agents, cur: c.Agent},
		{key: "length", label: "Length", opts: []menuOpt{{"short", "Short"}, {"long", "Long"}}, cur: c.Length},
	}
	if resume {
		rows[1].off = "the session's own"
	}
	if !m.expanded {
		return append(rows, menuRow{key: "more", label: "More", opts: []menuOpt{{"", m.moreSummary()}}})
	}

	tiers := []menuOpt{{"-1", "default"}}
	for i := 0; i < 4; i++ {
		tiers = append(tiers, menuOpt{strconv.Itoa(i), strconv.Itoa(i)})
	}
	efforts := []menuOpt{{"0", "default"}}
	for i, l := range tier.Efforts["codex"] {
		efforts = append(efforts, menuOpt{strconv.Itoa(i + 1), l})
	}
	choices := e.accountChoices(c)
	accounts := []menuOpt{{"", "routed"}}
	for _, a := range choices {
		accounts = append(accounts, menuOpt{accountValue(c, a), accountLabel(a, choices)})
	}
	more := []menuRow{
		{key: "tier", label: "Model tier", opts: tiers, cur: strconv.Itoa(c.Tier)},
		{key: "effort", label: "Effort", opts: efforts, cur: strconv.Itoa(c.Effort)},
		{key: "account", label: "Account", opts: accounts, cur: c.Account},
		{key: "bypass", label: "Permissions", opts: []menuOpt{{"false", "ask"}, {"true", "bypass"}}, cur: strconv.FormatBool(c.Bypass)},
	}
	switch {
	case resume && c.Length == "short":
		more[0].off, more[1].off = "set when the session resumes long", "set when the session resumes long"
	}
	switch {
	case resume:
		if c.Length == "long" {
			more[3].off = "bypass, always, for a long resume"
		} else {
			more[2].off = "routed, on the session's provider"
			more[3].off = "as the session ran"
		}
	case c.Agent == "auto":
		more[2].off = "routed, with Any"
		more[3].off = "bypass, always, with Any"
	}
	return append(rows, more...)
}

// moreSummary names the advanced settings that differ from the defaults.
func (m *menu) moreSummary() string {
	var on []string
	if m.c.Tier >= 0 {
		on = append(on, "tier "+strconv.Itoa(m.c.Tier))
	}
	if m.c.Effort > 0 {
		on = append(on, "effort "+tier.Efforts["codex"][m.c.Effort-1])
	}
	// The account counts where the command carries it: a named agent on a
	// new start, either provider on a long resume.
	forced := m.c.Session == "new" && m.c.Agent != "auto" || m.c.Session == "resume" && m.c.Length == "long"
	if m.c.Account != "" && forced {
		on = append(on, "account "+m.c.Account)
	}
	if m.c.Bypass && m.c.Agent != "auto" && m.c.Session == "new" {
		on = append(on, "bypass")
	}
	if len(on) == 0 {
		return "model tier, effort, account, permissions"
	}
	return strings.Join(on, " · ")
}

func (m *menu) set(key, value string) {
	switch key {
	case "session":
		if value != m.c.Session {
			m.c.Account = "" // a resume names the provider too; a new start does not
		}
		m.c.Session = value
	case "agent":
		if value != m.c.Agent && m.env.provider(value) != m.env.provider(m.c.Agent) {
			m.c.Account = ""
		}
		m.c.Agent = value
	case "length":
		m.c.Length = value
	case "tier":
		m.c.Tier, _ = strconv.Atoi(value)
	case "effort":
		m.c.Effort, _ = strconv.Atoi(value)
	case "account":
		m.c.Account = value
	case "bypass":
		m.c.Bypass = value == "true"
	}
}

// hint explains the focused row's current value in a sentence or two.
func (m *menu) hint(r menuRow) string {
	c, e := m.c, m.env
	if r.off != "" && r.key != "more" {
		return ""
	}
	switch r.key {
	case "session":
		if c.Session == "new" {
			return "Start a new conversation in this directory."
		}
		switch s := e.sessions; {
		case s == nil:
			return "Opens the browser of this directory's sessions; r resumes the one you pick. Counting them now."
		case s.count == 0:
			return "No sessions have run in this directory yet."
		default:
			title := s.latest.Title
			if title == "" && len(s.latest.Turns) > 0 {
				title = s.latest.Turns[0].Prompt
			}
			title = strings.Join(strings.Fields(title), " ")
			return fmt.Sprintf("Opens the browser of this directory's %d session%s; r resumes the one you pick. Latest, %s: %s",
				s.count, plural(s.count), whenLabel(s.latest.Updated, time.Now()), title)
		}
	case "agent":
		now := func(p string) string {
			if id := e.routed[p]; id != "" {
				return id
			}
			return "none eligible"
		}
		switch {
		case c.Agent == "auto":
			var best []string
			for _, p := range config.Providers {
				best = append(best, p+" "+now(p))
			}
			return fmt.Sprintf("aiq picks the account with the most quota to spare on any provider, and skips permission prompts. Best now: %s.", strings.Join(best, ", "))
		case knownProvider(c.Agent):
			return fmt.Sprintf("%s on a routed %s account. Best now: %s.", cliName(c.Agent), c.Agent, now(c.Agent))
		default:
			return fmt.Sprintf("%s through the %s launcher.", cliName(e.provider(c.Agent)), c.Agent)
		}
	case "length":
		if c.Length == "short" {
			return "Runs in this terminal and ends when you exit."
		}
		if e.longHere != "" && c.Session == "new" {
			return "A long session already runs here, on " + e.longHere + ". Enter attaches to it."
		}
		return "Runs in tmux and outlives this terminal. Before an account runs dry, aiq moves the session to another one."
	case "more":
		return "m or → shows them."
	case "tier":
		if c.Tier < 0 {
			if c.Agent == "auto" || c.Session == "resume" {
				return "Tier 1 unless you pick one: " + tierModels(1, "") + "."
			}
			return "The CLI's own model setting."
		}
		return fmt.Sprintf("Tier %d of 0 (strongest) to 3: %s.", c.Tier, tierModels(c.Tier, e.provider(c.Agent)))
	case "effort":
		if c.Effort == 0 {
			return "The CLI's own default effort."
		}
		var levels []string
		for _, p := range config.Providers {
			levels = append(levels, cliName(p)+" "+tier.Efforts[p][c.Effort-1])
		}
		return fmt.Sprintf("Level %d of 6: %s.", c.Effort, strings.Join(levels, ", "))
	case "account":
		if c.Account == "" {
			if c.Session == "resume" {
				return "aiq picks, on the provider of the session you resume."
			}
			if id := e.routed[e.provider(c.Agent)]; id != "" {
				return "aiq picks; now that is " + id + "."
			}
			return "aiq picks."
		}
		for _, a := range e.accountChoices(c) {
			if accountValue(c, a) == c.Account {
				if c.Session == "resume" {
					return a.note + " Pick a " + a.provider + " session: the account must match it."
				}
				return a.note
			}
		}
	case "bypass":
		if c.Bypass {
			return "Starts with " + bypassFlag(e.provider(c.Agent)) + ": no permission prompts."
		}
		return "The CLI asks before it runs commands or edits files."
	}
	return ""
}

func tierModels(n int, provider string) string {
	if provider != "" && tier.Known(provider) {
		if !tier.Has(provider, n) {
			return "none on " + cliName(provider)
		}
		return tier.Models[provider][n]
	}
	var names []string
	for _, p := range config.Providers {
		if tier.Has(p, n) && !slices.Contains(names, tier.Models[p][n]) {
			names = append(names, tier.Models[p][n])
		}
	}
	return joinOr(names)
}

// --- keys ---

// handle applies one key and returns the command to run, if any, and
// whether the menu is done.
func (m *menu) handle(k keypress) (args []string, done bool) {
	rows := m.rows()
	at := 0
	for i, r := range rows {
		if r.key == m.focus {
			at = i
		}
	}
	move := func(d int) {
		for i := at + d; i >= 0 && i < len(rows); i += d {
			if rows[i].off == "" {
				m.focus = rows[i].key
				return
			}
		}
	}
	cycle := func(d int) {
		r := rows[at]
		if r.off != "" {
			return
		}
		if r.key == "more" {
			m.toggleMore()
			return
		}
		i := 0
		for j, o := range r.opts {
			if o.value == r.cur {
				i = j
			}
		}
		i = clamp(i+d, 0, len(r.opts)-1)
		m.set(r.key, r.opts[i].value)
	}
	switch k.code {
	case keyUp:
		move(-1)
	case keyDown:
		move(1)
	case keyLeft:
		cycle(-1)
	case keyRight:
		cycle(1)
	case keyEnter:
		return menuArgs(m.c, m.env), true
	case keyBack, keyQuit:
		return nil, true
	}
	switch k.r {
	case 'm', '\t':
		m.toggleMore()
	case 'q':
		return nil, true
	case '?':
		return []string{"help"}, true
	case 'a':
		if m.env.longHere != "" {
			return []string{"long", "attach"}, true
		}
	}
	return nil, false
}

func (m *menu) toggleMore() {
	m.expanded = !m.expanded
	if m.expanded {
		m.focus = "tier"
		for _, r := range m.rows() {
			if r.key == "tier" && r.off != "" {
				m.focus = "account"
			}
		}
		for _, r := range m.rows() {
			if r.key == m.focus && r.off != "" {
				m.focus = "length"
			}
		}
	} else if m.focus != "session" && m.focus != "agent" && m.focus != "length" {
		m.focus = "more"
	}
}

// --- drawing ---

type seg struct{ text, style string }

func (m *menu) render() [][]seg {
	var out [][]seg
	blank := func() { out = append(out, nil) }
	out = append(out, []seg{{" aiq " + version, sgrBold}, {" · " + homeRel(m.env.dir), sgrDim}})
	if m.env.longHere != "" {
		out = append(out, []seg{{" ● a long session runs here on " + m.env.longHere + "; a attaches", sgrYellow}})
	}
	blank()
	rows := m.rows()
	var focused menuRow
	for _, r := range rows {
		mark := "   "
		labelStyle := ""
		if r.key == m.focus {
			mark, labelStyle, focused = " › ", sgrBold, r
		}
		if r.key == "tier" {
			blank()
		}
		line := []seg{{mark, sgrCyan}, {pad(r.label, 13), labelStyle}}
		switch {
		case r.off != "":
			line[1].style = sgrDim
			line = append(line, seg{" " + r.off, sgrDim})
		case r.key == "more":
			line = append(line, seg{" " + r.opts[0].text, sgrDim})
		default:
			for _, o := range r.opts {
				style := ""
				if o.value == r.cur {
					style = sgrBold + sgrCyan
					if r.key == m.focus {
						style = sgrReverse
					}
					line = append(line, seg{" " + o.text + " ", style})
				} else {
					line = append(line, seg{" " + o.text + " ", sgrDim})
				}
			}
		}
		out = append(out, line)
	}
	blank()
	for _, l := range wrap(m.hint(focused), max(m.w-6, 20)) {
		out = append(out, []seg{{"   " + l, ""}})
	}
	blank()
	out = append(out, []seg{{" $ ", sgrDim}, {tmux.Quote(append([]string{"aiq"}, menuArgs(m.c, m.env)...)), sgrGreen}})
	return out
}

func (m *menu) footer() []seg {
	keys := "Enter start · ↑↓ move · ←→ choose · m more · ? all commands · q quit"
	if m.env.longHere != "" {
		keys = "Enter start · ↑↓ move · ←→ choose · m more · a attach · ? all commands · q quit"
	}
	return []seg{{" " + keys, sgrDim}}
}

func (m *menu) draw(out *os.File) {
	var sb strings.Builder
	sb.WriteString("\x1b[H")
	lines := m.render()
	if len(lines) > m.h-1 {
		lines = lines[:max(m.h-1, 0)]
	}
	for len(lines) < m.h-1 {
		lines = append(lines, nil)
	}
	lines = append(lines, m.footer())
	for i, l := range lines {
		room := m.w
		for _, s := range l {
			if room <= 0 {
				break
			}
			text := s.text
			if cells(text) > room {
				text = truncate(text, room)
			}
			room -= cells(text)
			if s.style != "" {
				sb.WriteString(s.style + text + sgrReset)
			} else {
				sb.WriteString(text)
			}
		}
		sb.WriteString("\x1b[K")
		if i < len(lines)-1 {
			sb.WriteString("\r\n")
		}
	}
	sb.WriteString("\x1b[J")
	out.WriteString(sb.String())
}

// run shows the menu and returns the aiq arguments to run, or nil to quit.
func (m *menu) run(sessions <-chan *menuSessions) ([]string, error) {
	in, out := os.Stdin, os.Stdout
	old, err := term.MakeRaw(int(in.Fd()))
	if err != nil {
		return nil, err
	}
	out.WriteString("\x1b[?1049h\x1b[?25l")
	defer func() {
		out.WriteString("\x1b[?25h\x1b[?1049l")
		term.Restore(int(in.Fd()), old)
	}()
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	keys := make(chan []byte)
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := in.Read(buf)
			if err != nil {
				close(keys)
				return
			}
			keys <- append([]byte(nil), buf[:n]...)
		}
	}()
	resize := func() {
		m.w, m.h, err = term.GetSize(int(out.Fd()))
		if err != nil || m.w <= 0 || m.h <= 0 {
			m.w, m.h = 80, 24
		}
	}
	resize()
	m.draw(out)
	for {
		select {
		case <-winch:
			resize()
		case s := <-sessions:
			m.env.sessions, sessions = s, nil
		case buf, ok := <-keys:
			if !ok {
				return nil, nil
			}
			for _, k := range parseKeys(buf) {
				if args, done := m.handle(k); done {
					return args, nil
				}
			}
		}
		m.draw(out)
	}
}

// --- entry ---

func menuStatePath() string { return filepath.Join(paths.DataDir(), "menu.json") }

func loadMenuChoice() menuChoice {
	c := defaultChoice()
	if data, err := os.ReadFile(menuStatePath()); err == nil {
		json.Unmarshal(data, &c)
	}
	return c
}

func saveMenuChoice(c menuChoice) {
	if data, err := json.MarshalIndent(c, "", "  "); err == nil {
		os.WriteFile(menuStatePath(), append(data, '\n'), 0o600)
	}
}

// menuEnvironment gathers accounts, headroom, launchers and any long
// session in this workspace. Sessions load separately: parsing transcripts
// can take a moment, and the menu should not wait for it.
func (a *app) menuEnvironment(dir string) *menuEnv {
	e := &menuEnv{dir: dir, launchers: map[string]string{}, accounts: map[string][]menuAccount{}, routed: map[string]string{}}
	for name, l := range a.cfg.Launchers {
		e.launchers[name] = l.Provider
	}
	if v, err := a.pool.View(0); err == nil {
		sort.SliceStable(v.Accounts, func(i, j int) bool { return v.Accounts[i].Order < v.Accounts[j].Order })
		for _, acc := range v.Accounts {
			if !acc.Enabled {
				continue
			}
			note := acc.ID
			if left := headroom(acc.Windows); left >= 0 {
				note += fmt.Sprintf(": %d%% left in its tightest window", left)
			}
			if !acc.Eligible && acc.Ineligible != "" {
				note += "; " + acc.Ineligible
			}
			e.accounts[acc.Provider] = append(e.accounts[acc.Provider], menuAccount{acc.Name, acc.Provider, note + "."})
		}
		for _, p := range config.Providers {
			for _, r := range v.Rankings[p+"/interactive"] {
				if r.Eligible {
					e.routed[p] = r.ID
					break
				}
			}
		}
	}
	ws := workspaceID()
	if tmux.Available() && tmux.HasSession(longrun.SessionName(a.cfg.Long.TmuxPrefix, ws)) {
		if leases, err := a.longLeases(); err == nil {
			for _, l := range leases {
				if l.Workspace == ws {
					e.longHere = l.AccountID
				}
			}
		}
	}
	return e
}

// headroom is the remaining percentage of an account's fullest unscoped
// window, or -1 when nothing has been observed.
func headroom(windows []pool.WindowView) int {
	left := -1
	for _, w := range windows {
		if w.Scope != "" || w.Kind == state.KindOther {
			continue
		}
		r := int(100 - w.UsedPct + 0.5)
		if r < 0 {
			r = 0
		}
		if left < 0 || r < left {
			left = r
		}
	}
	return left
}

func loadMenuSessions(dir string) *menuSessions {
	all, err := transcript.List(defaultRoots(), dir)
	if err != nil {
		return &menuSessions{}
	}
	list := interactiveOnly(all)
	s := &menuSessions{count: len(list)}
	for _, x := range list {
		if x.Updated.After(s.latest.Updated) {
			s.latest = x
		}
	}
	return s
}

// cmdMenu opens the start menu and runs what it composes.
func cmdMenu() error {
	a, err := openApp()
	if err != nil {
		return err
	}
	dir, err := os.Getwd()
	if err != nil {
		a.close()
		return err
	}
	env := a.menuEnvironment(dir)
	a.close()
	sessions := make(chan *menuSessions, 1)
	go func() { sessions <- loadMenuSessions(dir) }()

	m := &menu{env: env, c: env.normalize(loadMenuChoice()), focus: "session"}
	args, err := m.run(sessions)
	if err != nil || args == nil {
		return err
	}
	if args[0] == "help" {
		fmt.Printf(usage, version)
		return nil
	}
	saveMenuChoice(m.c)
	fmt.Println("$ " + tmux.Quote(append([]string{"aiq"}, args...)))
	self, err := os.Executable()
	if err != nil {
		return err
	}
	return syscall.Exec(self, append([]string{"aiq"}, args...), os.Environ())
}
