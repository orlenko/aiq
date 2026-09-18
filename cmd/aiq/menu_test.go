package main

import (
	"strings"
	"testing"
)

func testMenuEnv() *menuEnv {
	return &menuEnv{
		dir:       "/w",
		launchers: map[string]string{"boxed": "claude"},
		accounts: map[string][]menuAccount{
			"claude": {{"fourth", "claude/fourth: 72% left."}, {"orlenko", "claude/orlenko."}},
			"codex":  {{"bjola", "codex/bjola."}},
		},
		routed: map[string]string{"claude": "claude/fourth", "codex": "codex/bjola"},
	}
}

func TestMenuArgs(t *testing.T) {
	e := testMenuEnv()
	c := func(f func(*menuChoice)) menuChoice {
		x := defaultChoice()
		f(&x)
		return x
	}
	for _, tt := range []struct {
		name   string
		choice menuChoice
		want   string
	}{
		{"default", defaultChoice(), "run auto"},
		{"auto long tier", c(func(x *menuChoice) { x.Length, x.Tier, x.Effort = "long", 0, 3 }), "long auto --model-tier 0 --effort 3"},
		{"claude plain", c(func(x *menuChoice) { x.Agent = "claude" }), "claude"},
		{"claude bypass", c(func(x *menuChoice) { x.Agent, x.Bypass = "claude", true }), "claude --dangerously-skip-permissions"},
		{"codex account", c(func(x *menuChoice) { x.Agent, x.Account, x.Bypass = "codex", "bjola", true }), "run codex --account bjola -- --yolo"},
		{"launcher tier", c(func(x *menuChoice) { x.Agent, x.Tier = "boxed", 2 }), "run claude --launcher boxed --model-tier 2 --"},
		{"launcher plain", c(func(x *menuChoice) { x.Agent = "boxed" }), "boxed"},
		{"claude long", c(func(x *menuChoice) { x.Agent, x.Length, x.Account, x.Bypass = "claude", "long", "fourth", true }),
			"long claude --account fourth -- --dangerously-skip-permissions"},
		{"launcher long", c(func(x *menuChoice) { x.Agent, x.Length, x.Effort = "boxed", "long", 4 }), "long boxed --effort 4"},
		{"resume short ignores model", c(func(x *menuChoice) { x.Session, x.Tier, x.Agent = "resume", 0, "codex" }), "resume"},
		{"resume long", c(func(x *menuChoice) { x.Session, x.Length, x.Tier = "resume", "long", 0 }), "long auto resume --model-tier 0"},
		{"auto ignores account and bypass", c(func(x *menuChoice) { x.Account, x.Bypass = "fourth", false }), "run auto"},
	} {
		if got := strings.Join(menuArgs(tt.choice, e), " "); got != tt.want {
			t.Errorf("%s: aiq %s, want aiq %s", tt.name, got, tt.want)
		}
	}
	e.longHere = "claude/fourth"
	if got := strings.Join(menuArgs(c(func(x *menuChoice) { x.Length = "long" }), e), " "); got != "long attach" {
		t.Errorf("long with a session running here: aiq %s", got)
	}
	if got := strings.Join(menuArgs(defaultChoice(), e), " "); got != "run auto" {
		t.Errorf("short with a long session running here: aiq %s", got)
	}
}

// Every command the menu composes must parse the way aiq's own parsers do.
func TestMenuArgsParse(t *testing.T) {
	e := testMenuEnv()
	for _, agent := range []string{"auto", "claude", "codex", "boxed"} {
		for _, length := range []string{"short", "long"} {
			for _, session := range []string{"new", "resume"} {
				x := menuChoice{Session: session, Agent: agent, Length: length, Tier: 1, Effort: 6, Bypass: true}
				if agent != "auto" {
					x.Account = e.accounts[e.provider(agent)][0].name
				}
				args := menuArgs(x, e)
				switch {
				case args[0] == "long" && args[1] == "auto" && len(args) > 2 && args[2] == "resume":
				case args[0] == "long" && args[1] == "auto":
					if _, _, err := parseAutoFlags(append([]string{"--long"}, args[2:]...)); err != nil {
						t.Errorf("aiq %v: %v", args, err)
					}
				case args[0] == "long":
					account, flags, rest, err := parseLongArgs(args[2:])
					if err != nil || account != x.Account || len(flags) != 4 || len(rest) != 1 {
						t.Errorf("aiq %v: account %q flags %v rest %v err %v", args, account, flags, rest, err)
					}
				case args[0] == "run" && args[1] == "auto":
					if _, _, err := parseAutoFlags(args[2:]); err != nil {
						t.Errorf("aiq %v: %v", args, err)
					}
				case args[0] == "run":
					f := parseRunFlags(args[2:])
					if f.flagsErr != nil || f.account != x.Account || f.modelTier == nil || *f.modelTier != 1 || f.effort != 6 || len(f.rest) != 1 {
						t.Errorf("aiq %v: %+v", args, f)
					}
				case args[0] == "resume":
				default:
					t.Errorf("unexpected aiq %v", args)
				}
			}
		}
	}
}

func TestMenuNavigation(t *testing.T) {
	m := &menu{env: testMenuEnv(), c: defaultChoice(), focus: "session", w: 100, h: 30}
	press := func(ks ...keypress) []string {
		for _, k := range ks {
			if args, done := m.handle(k); done {
				return args
			}
		}
		return nil
	}
	down, right, left := keypress{code: keyDown}, keypress{code: keyRight}, keypress{code: keyLeft}
	// Agent → Codex, Length → Long.
	press(down, right, right, down, right)
	if m.c.Agent != "codex" || m.c.Length != "long" {
		t.Fatalf("choice = %+v", m.c)
	}
	// More opens on the model tier; pick tier 0, then the account.
	press(keypress{r: 'm'}, right, down, down, right)
	if m.c.Tier != 0 || m.c.Account != "bjola" {
		t.Fatalf("choice = %+v, focus %s", m.c, m.focus)
	}
	// Back to Any: account and permissions no longer apply and are skipped.
	press(keypress{r: 'm'}, keypress{code: keyUp}, keypress{code: keyUp}, left, left)
	if m.c.Agent != "auto" {
		t.Fatalf("agent = %s", m.c.Agent)
	}
	if got := strings.Join(press(keypress{code: keyEnter}), " "); got != "long auto --model-tier 0" {
		t.Fatalf("Enter ran aiq %s", got)
	}
	// Resume turns the agent row off; focus steps over it.
	m = &menu{env: testMenuEnv(), c: defaultChoice(), focus: "session", w: 100, h: 30}
	press(right, down)
	if m.focus != "length" {
		t.Fatalf("focus = %s after resume", m.focus)
	}
	if args := press(keypress{r: 'q'}); args != nil {
		t.Fatalf("q ran aiq %v", args)
	}
}

func TestMenuRender(t *testing.T) {
	for _, size := range [][2]int{{120, 40}, {40, 12}, {10, 3}} {
		m := &menu{env: testMenuEnv(), c: defaultChoice(), focus: "session", w: size[0], h: size[1]}
		for _, expanded := range []bool{false, true} {
			m.expanded = expanded
			for _, r := range m.rows() {
				m.focus = r.key
				if m.hint(r) == "" && r.off == "" {
					t.Errorf("row %s has no hint", r.key)
				}
				m.render()
			}
		}
	}
	m := &menu{env: testMenuEnv(), c: defaultChoice(), w: 100, h: 30}
	var shown []string
	for _, l := range m.render() {
		var sb strings.Builder
		for _, s := range l {
			sb.WriteString(s.text)
		}
		shown = append(shown, sb.String())
	}
	if !strings.Contains(strings.Join(shown, "\n"), "$ aiq run auto") {
		t.Errorf("render lacks the command preview:\n%s", strings.Join(shown, "\n"))
	}
}

func TestMenuNormalize(t *testing.T) {
	e := testMenuEnv()
	got := e.normalize(menuChoice{Session: "x", Agent: "gone", Length: "y", Tier: 9, Effort: 9, Account: "fourth"})
	if got != defaultChoice() {
		t.Errorf("normalize = %+v", got)
	}
	keep := menuChoice{Session: "resume", Agent: "boxed", Length: "long", Tier: 0, Effort: 2, Account: "fourth", Bypass: true}
	if e.normalize(keep) != keep {
		t.Errorf("normalize changed a valid choice: %+v", e.normalize(keep))
	}
}
