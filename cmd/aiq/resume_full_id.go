package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

func resumeGlobalEcho(out io.Writer, s transcript.Session, dir, account string) {
	short := s.ID
	if len(short) > 8 {
		short = short[:8]
	}
	fmt.Fprintf(out, "aiq: resuming %q in %q", short, dir)
	if account != "" {
		fmt.Fprintf(out, " (%q)", s.Provider+"/"+account)
	}
	fmt.Fprintln(out)
}

// Labels come from recorded homes, never the directory's basename. A native
// home or a home shared by accounts is deliberately not forced to an account.
func resumeRootsForAccounts(roots transcript.Roots, accounts []state.Account) transcript.Roots {
	r := cloneNavRoots(roots)
	r.Labels = map[string]string{}
	canonical := func(path string) string {
		if !filepath.IsAbs(path) {
			return ""
		}
		p, err := filepath.EvalSymlinks(path)
		if err != nil {
			return ""
		}
		return p
	}
	real := map[string]bool{}
	if len(r.ClaudeProjects) > 0 {
		if p := canonical(r.ClaudeProjects[0]); p != "" {
			real["claude\x00"+p] = true
		}
	}
	if len(r.CodexSessions) > 0 {
		if p := canonical(r.CodexSessions[0]); p != "" {
			real["codex\x00"+p] = true
		}
	}
	owners := map[string]string{}
	for _, account := range accounts {
		if account.Provider != "claude" && account.Provider != "codex" || !filepath.IsAbs(account.Home) {
			continue
		}
		suffix := "projects"
		if account.Provider == "codex" {
			suffix = "sessions"
		}
		root := filepath.Join(account.Home, suffix)
		resolved := canonical(root)
		if resolved == "" {
			continue
		}
		key := account.Provider + "\x00" + resolved
		if account.Native {
			real[key] = true
		} else if old, present := owners[key]; !present {
			owners[key] = account.ID
		} else if old != account.ID {
			owners[key] = ""
		}
		appendUnique := func(rows []string) []string {
			for _, path := range rows {
				if path == root {
					return rows
				}
			}
			return append(rows, root)
		}
		if account.Provider == "claude" {
			r.ClaudeProjects = appendUnique(r.ClaudeProjects)
		} else {
			r.CodexSessions = appendUnique(r.CodexSessions)
			index := filepath.Join(account.Home, "session_index.jsonl")
			found := false
			for _, path := range r.CodexIndex {
				found = found || path == index
			}
			if !found {
				r.CodexIndex = append(r.CodexIndex, index)
			}
		}
	}
	for provider, rows := range map[string][]string{"claude": r.ClaudeProjects, "codex": r.CodexSessions} {
		for _, root := range rows {
			resolved := canonical(root)
			key := provider + "\x00" + resolved
			if resolved != "" && !real[key] {
				if owner := owners[key]; owner != "" {
					r.Labels[root] = owner
				}
			}
		}
	}
	return r
}

type resumeSelection struct {
	s      transcript.Session
	forced string
	global bool
}

func resumeAccountForPath(roots transcript.Roots, provider, path string) string {
	file, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	rows := roots.ClaudeProjects
	if provider == "codex" {
		rows = roots.CodexSessions
	} else if provider != "claude" {
		return ""
	}
	owner := ""
	for _, root := range rows {
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(resolved, file)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
			continue
		}
		label := roots.Labels[root]
		if label == "" || owner != "" && label != owner {
			return ""
		}
		owner = label
	}
	return owner
}

func resolveResumeSelection(st *state.Store, roots transcript.Roots, sessions []transcript.Session, unreadable []string, id string) (resumeSelection, error) {
	if err := validateConvoID(id); err != nil {
		return resumeSelection{}, err
	}
	if transcript.IsFullID("claude", id) {
		provider := ""
		for _, s := range sessions {
			if s.ID == id {
				if provider != "" && provider != s.Provider {
					return resumeSelection{}, &convoLookupError{id: id, cause: errConvoAmbiguousID}
				}
				provider = s.Provider
			}
		}
	}
	s, err := findSession(sessions, unreadable, id)
	if err == nil {
		selected := resumeSelection{s: s}
		if st != nil && transcript.IsFullID(s.Provider, id) {
			accounts, e := st.ListAccounts("")
			if e != nil {
				return resumeSelection{}, errors.New("session account metadata unavailable")
			}
			selected.forced = resumeAccountForPath(resumeRootsForAccounts(roots, accounts), s.Provider, s.Path)
		}
		return selected, nil
	}
	if !errors.Is(err, errSessionNoMatch) || !transcript.IsFullID("claude", id) {
		return resumeSelection{}, err
	}
	accounts, err := st.ListAccounts("")
	if err != nil {
		return resumeSelection{}, errors.New("session account metadata unavailable")
	}
	return resolveResumeExact(resumeRootsForAccounts(roots, accounts), id)
}

func resolveResumeExact(roots transcript.Roots, id string) (resumeSelection, error) {
	ctx, cancel := context.WithTimeout(context.Background(), transcript.ListBudget)
	defer cancel()
	target, err := resolveGlobalConvo(ctx, transcript.DirectWalk(roots), id, transcript.ParseSummary)
	if err != nil {
		return resumeSelection{}, err
	}
	parsed, err := transcript.ParseFile(target.path)
	if err != nil || parsed == nil {
		return resumeSelection{}, &convoLoadError{path: target.path, cause: err}
	}
	if parsed.Provider != target.provider || parsed.ID != id {
		return resumeSelection{}, &convoLoadError{path: target.path, cause: errConvoIdentity}
	}
	s := *parsed
	s.Path = target.path
	if s.Provider == "agy" && s.Cwd == "" {
		if dir, known := transcript.AgyDirectory(ctx, target.copyRoot, id); known {
			s.Cwd = dir
		}
	}
	return resumeSelection{s: s, forced: target.copyAccount, global: true}, nil
}

func (a *app) resumeCopyAccount(provider, copyAccount, requested string) (string, error) {
	if copyAccount == "" {
		if requested == "" {
			return "", nil
		}
		return a.longAccountName(provider, requested)
	}
	name, err := a.longAccountName(provider, copyAccount)
	if err != nil {
		return "", err
	}
	if requested != "" {
		explicit, err := a.longAccountName(provider, requested)
		if err != nil {
			return "", err
		}
		if explicit != name {
			return "", fmt.Errorf("session transcript is in account %q; requested account %q cannot read that copy", copyAccount, requested)
		}
	}
	return name, nil
}
