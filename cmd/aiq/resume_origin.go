package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

// origin is the aiq launch a session came from.
type origin struct {
	launch state.Launch
	// exact: the launch named this session's id. Otherwise the session was
	// matched to the latest launch before it in the same directory.
	exact bool
}

// originGap bounds how long after a launch a session may start and still
// be matched to it by time. Claude writes its first record with the first
// prompt, and one CLI process can open several sessions (/clear, /new).
const originGap = 12 * time.Hour

// originSlack allows for a session clock a little ahead of the launch.
const originSlack = 2 * time.Second

func sessionKey(s transcript.Session) string { return s.Provider + "/" + s.ID }

// matchOrigins pairs sessions with the launches that started them. Among
// the launches that named a session, the latest one through a launcher
// wins, else the latest one: a sandbox, once used, stays on until a resume
// asks for --bare again. A session no launch named is matched to the latest
// launch of the same provider in the same directory shortly before it began.
func matchOrigins(sessions []transcript.Session, launches []state.Launch, canon func(string) string) map[string]origin {
	out := map[string]origin{}
	later := func(l, than *state.Launch) bool {
		if than == nil {
			return true
		}
		if (l.Launcher != "") != (than.Launcher != "") {
			return l.Launcher != ""
		}
		return !l.StartedAt.Before(than.StartedAt)
	}
	for _, s := range sessions {
		var best *state.Launch
		for i := range launches {
			l := &launches[i]
			if l.Provider == s.Provider && l.SessionID == s.ID && later(l, best) {
				best = l
			}
		}
		if best != nil {
			out[sessionKey(s)] = origin{launch: *best, exact: true}
			continue
		}
		if s.Started.IsZero() {
			continue
		}
		dir := canon(s.Cwd)
		for i := range launches {
			l := &launches[i]
			if l.Provider != s.Provider || l.StartedAt.After(s.Started.Add(originSlack)) || s.Started.Sub(l.StartedAt) > originGap {
				continue
			}
			if canon(l.Cwd) != dir {
				continue
			}
			if best == nil || !l.StartedAt.Before(best.StartedAt) {
				best = l
			}
		}
		if best != nil {
			out[sessionKey(s)] = origin{launch: *best}
		}
	}
	return out
}

// loadOrigins reads the launches that could explain sessions in dir.
func (a *app) loadOrigins(dir string, sessions []transcript.Session) map[string]origin {
	cwds := map[string]bool{dir: true, canonicalDir(dir): true}
	if wd := os.Getenv("PWD"); wd != "" && canonicalDir(wd) == canonicalDir(dir) {
		cwds[filepath.Clean(wd)] = true
	}
	var dirs, ids []string
	for c := range cwds {
		dirs = append(dirs, c)
	}
	for _, s := range sessions {
		ids = append(ids, s.ID)
	}
	var launches []state.Launch
	// SQLite caps bound parameters; ask in chunks.
	for len(ids) > 0 || dirs != nil {
		n := min(len(ids), 400)
		chunk, err := a.st.ListLaunches(hostname(), dirs, ids[:n])
		if err != nil {
			return nil
		}
		launches = append(launches, chunk...)
		ids, dirs = ids[n:], nil
	}
	return matchOrigins(sessions, dedupeLaunches(launches), canonicalDir)
}

func dedupeLaunches(in []state.Launch) []state.Launch {
	seen := map[int64]bool{}
	var out []state.Launch
	for _, l := range in {
		if !seen[l.ID] {
			seen[l.ID] = true
			out = append(out, l)
		}
	}
	return out
}

func canonicalDir(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// resumeLauncher decides which launcher reopens a session: the one asked
// for, none when --bare, else the one the session ran under.
func resumeLauncher(cfg *config.Config, s transcript.Session, org *origin, o resumeOpts) (name, why string, err error) {
	switch {
	case o.bare:
		if org != nil && org.launch.Launcher != "" {
			return "", fmt.Sprintf("without launcher %s (--bare)", org.launch.Launcher), nil
		}
		return "", "", nil
	case o.launcher != "":
		name, why = o.launcher, "through launcher "+o.launcher
	case org != nil && org.launch.Launcher != "":
		name = org.launch.Launcher
		why = "through launcher " + name + ", as it ran"
		if !org.exact {
			why = "through launcher " + name + ", the launcher of the last aiq launch before it here"
		}
		if _, ok := cfg.Launcher(name); !ok {
			return "", "", configErr("no-launcher", "session %s ran through launcher %s, which is no longer registered; resume with --launcher <name> or --bare", s.ID, name)
		}
	default:
		return "", "", nil
	}
	l, ok := cfg.Launcher(name)
	if !ok {
		return "", "", configErr("no-launcher", "no launcher %q; see: aiq launcher list", name)
	}
	if l.Provider != s.Provider {
		return "", "", configErr("bad-flags", "launcher %s starts %s, but session %s is a %s session", name, l.Provider, s.ID, s.Provider)
	}
	return name, why, nil
}

// originTag is the short launcher marker shown next to a session.
func originTag(org *origin) string {
	if org == nil || org.launch.Launcher == "" {
		return ""
	}
	if org.exact {
		return "via " + org.launch.Launcher
	}
	return "via " + org.launch.Launcher + "?"
}

// resumeBypass decides whether aiq adds the permission bypass flag. A bare
// resume follows the transcript. Through a launcher, the transcript cannot
// tell a bypass the user asked for from one the launcher adds itself (the
// safe-* wrappers do, and Codex refuses the flag twice), so aiq adds it only
// when the user passed it to that same launcher before.
func resumeBypass(s transcript.Session, org *origin, launcher string) bool {
	if launcher == "" {
		return s.Bypass
	}
	return org != nil && org.launch.Launcher == launcher && hasBypass(org.launch.Args)
}

func hasBypass(args []string) bool {
	for i, a := range args {
		switch a {
		case "--dangerously-skip-permissions", "--dangerously-bypass-approvals-and-sandbox", "--yolo",
			"--permission-mode=bypassPermissions":
			return true
		case "--permission-mode":
			if i+1 < len(args) && args[i+1] == "bypassPermissions" {
				return true
			}
		case "--":
			return false
		}
	}
	return false
}
