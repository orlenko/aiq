package transcript

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrBudget is what a list's parse returns when the list's time ran out
// before it: the transcript counts in ListResult.Of and the list is Partial.
var ErrBudget = errors.New("transcript: out of time for the list")

// ErrUnreadable marks a transcript whose parse panicked. ParseFile returns
// it wrapped with the path, so a caller can leave the file out, say so, and
// not try it again until it changes.
var ErrUnreadable = errors.New("transcript could not be read")

// errNoSession is a parse that found no session of its own: a Codex
// subagent's rollout, or a file with no id.
var errNoSession = errors.New("transcript: no session")

// Candidate is a transcript that may belong to a directory.
type Candidate struct {
	Provider string // "claude", "codex" or "agy"
	Path     string
}

// Discovery finds the transcripts of a directory: Claude by project directory,
// Codex by the cwd at the head of each rollout, Antigravity by its history.
type Discovery interface {
	// Candidates lists them for dir (canonical) in a stable order. When ctx
	// ends it stops and returns what it has with ctx's error.
	Candidates(ctx context.Context, dir string) ([]Candidate, error)
	// CodexNames maps Codex thread ids to the names given with /rename.
	CodexNames(ctx context.Context) map[string]string
}

// DirectWalk is the Discovery that reads the roots each time it is asked.
func DirectWalk(r Roots) Discovery { return directWalk{r} }

type directWalk struct{ r Roots }

func (w directWalk) Candidates(ctx context.Context, dir string) ([]Candidate, error) {
	var out []Candidate
	out = append(out, claudeFiles(ctx, w.r.ClaudeProjects, dir)...)
	out = append(out, codexFiles(ctx, w.r.CodexSessions, dir, time.Time{})...)
	out = append(out, agyFiles(ctx, w.r.AgyAppData, dir)...)
	return out, ctx.Err()
}

func (w directWalk) CodexNames(ctx context.Context) map[string]string {
	return codexNames(w.r.CodexIndex)
}

// Summary is what a list row shows of a session, without its turns.
type Summary struct {
	Provider, ID, Path, Cwd string
	Label                   string // the name, else the first human prompt
	Title, Model, Branch    string
	Started, Updated        time.Time
	Turns, Tools            int
	Worker, Bypass          bool
	// Empty says the session has no turns and skipped no record: opened
	// and closed without a word.
	Empty bool
}

// Summarize is the row of s.
func Summarize(s *Session) Summary {
	return Summary{
		Provider: s.Provider, ID: s.ID, Path: s.Path, Cwd: s.Cwd,
		Label: s.Label(), Title: s.Title, Model: s.Model, Branch: s.Branch,
		Started: s.Started, Updated: s.Updated,
		Turns: len(s.Turns), Tools: s.Tools(),
		Worker: s.Worker, Bypass: s.Bypass, Empty: len(s.Turns) == 0 && s.Skipped == 0,
	}
}

// ListResult is one directory's sessions, newest first.
type ListResult struct {
	Rows []Summary // empty sessions included, marked Empty
	// Scanned is how many candidates were parsed, of the Of found.
	Scanned, Of int
	// Partial says the time ran out: discovery stopped, or a parse was
	// skipped with ErrBudget.
	Partial bool
	// Unreadable lists the candidates whose parse panicked.
	Unreadable []string
}

// ListWith returns every session src finds for dir, with the cwd each
// transcript records checked against dir again: an encoded Claude project
// name can be shared by two directories. A session found under more than one
// root is listed once, its newest copy. parse reads one transcript, and
// answers ErrBudget for one it had no time for and ErrUnreadable for one
// whose parse panicked; a parse that panics itself counts as ErrUnreadable,
// its value dropped. parse runs on up to 8 goroutines at once. The list's
// time is ctx's deadline.
func ListWith(ctx context.Context, src Discovery, dir string, parse func(ctx context.Context, path string) (Summary, error)) (ListResult, error) {
	dir = canonical(dir)
	var res ListResult
	cands, err := src.Candidates(ctx, dir)
	if err != nil {
		if ctx.Err() == nil {
			return res, err
		}
		res.Partial = true
	}
	res.Of = len(cands)
	rows := make([]*Summary, len(cands))
	errs := make([]error, len(cands))
	parallel(len(cands), func(i int) {
		if ctx.Err() != nil {
			errs[i] = ErrBudget
			return
		}
		s, err := recovered(ctx, parse, cands[i].Path)
		// A file named for another CLI's parser is not this provider's.
		if errs[i] = err; err != nil || s.Provider != cands[i].Provider {
			return
		}
		if s.Cwd != "" && canonical(s.Cwd) != dir {
			return // an encoded project name shared by two directories
		}
		rows[i] = &s
	})
	for i, err := range errs {
		switch {
		case errors.Is(err, ErrBudget):
			res.Partial = true
			continue
		case errors.Is(err, ErrUnreadable):
			res.Unreadable = append(res.Unreadable, cands[i].Path)
		}
		res.Scanned++
	}

	names := src.CodexNames(ctx)
	byID := map[string]*Summary{}
	for _, s := range rows {
		if s == nil {
			continue
		}
		if s.Provider == "codex" && s.Title == "" && names[s.ID] != "" {
			s.Title, s.Label = names[s.ID], names[s.ID]
		}
		key := s.Provider + "/" + s.ID
		if prev, ok := byID[key]; ok && !s.Updated.After(prev.Updated) {
			continue
		}
		byID[key] = s
	}
	res.Rows = make([]Summary, 0, len(byID))
	for _, s := range byID {
		res.Rows = append(res.Rows, *s)
	}
	sort.Slice(res.Rows, func(i, j int) bool {
		a, b := res.Rows[i], res.Rows[j]
		if !a.Updated.Equal(b.Updated) {
			return a.Updated.After(b.Updated)
		}
		return a.ID < b.ID
	})
	return res, nil
}

// recovered runs parse on path, and answers ErrUnreadable for a parse that
// panics, as ParseFile does, so a parse that does not recover cannot take the
// list down.
func recovered(ctx context.Context, parse func(ctx context.Context, path string) (Summary, error), path string) (s Summary, err error) {
	defer func() {
		if recover() != nil {
			s, err = Summary{}, &unreadableError{path}
		}
	}()
	return parse(ctx, path)
}

// List returns every session whose working directory is dir, newest first,
// leaving out those with no turns, and the transcripts it could not read. It
// prints nothing: the caller says what was left out.
func List(r Roots, dir string) ([]Session, []string, error) {
	var mu sync.Mutex
	parsed := map[string]*Session{}
	parse := func(ctx context.Context, path string) (Summary, error) {
		s, err := ParseFile(path)
		if err != nil {
			return Summary{}, err
		}
		if s == nil {
			return Summary{}, errNoSession
		}
		mu.Lock()
		parsed[s.Path] = s
		mu.Unlock()
		return Summarize(s), nil
	}
	res, err := ListWith(context.Background(), DirectWalk(r), dir, parse)
	if err != nil {
		return nil, res.Unreadable, err
	}
	list := make([]Session, 0, len(res.Rows))
	for _, row := range res.Rows {
		if row.Empty {
			continue
		}
		p := parsed[row.Path]
		if p == nil {
			continue
		}
		s := *p
		s.Title = row.Title
		list = append(list, s)
	}
	return list, res.Unreadable, nil
}

// ParseSummary is a list's parse with no cache in front of it: ParseFile,
// summarised. Once ctx has ended it returns ErrBudget without parsing.
func ParseSummary(ctx context.Context, path string) (Summary, error) {
	if ctx.Err() != nil {
		return Summary{}, ErrBudget
	}
	s, err := ParseFile(path)
	if err != nil {
		return Summary{}, err
	}
	if s == nil {
		return Summary{}, errNoSession
	}
	return Summarize(s), nil
}

// ParseFile parses the transcript at path with the parser its path calls for.
// A parse that panics returns ErrUnreadable rather than taking down the
// caller; the panic's value, which may quote the transcript, is dropped.
// That covers the callers that parse through ParseFile, the list walk among
// them. The daemon's readers and aiq convo's parse of the session it shows
// still call the parsers directly. Nothing is cached.
func ParseFile(path string) (s *Session, err error) {
	defer func() {
		if recover() != nil {
			s, err = nil, &unreadableError{path}
		}
	}()
	return (*parser.Load())(path)
}

type unreadableError struct{ path string }

// Error quotes the path, which is a file's name and may hold any byte.
func (e *unreadableError) Error() string        { return ErrUnreadable.Error() + ": " + strconv.Quote(e.path) }
func (e *unreadableError) Is(target error) bool { return target == ErrUnreadable }

// parser is what ParseFile runs; tests swap it to make a parse panic.
var parser atomic.Pointer[func(path string) (*Session, error)]

func init() {
	f := parseByPath
	parser.Store(&f)
}

// SetParserForTest makes ParseFile run f until restore is called.
func SetParserForTest(f func(path string) (*Session, error)) (restore func()) {
	old := parser.Swap(&f)
	return func() { parser.Store(old) }
}

// parseByPath parses a transcript by its path's shape: Antigravity's
// brain/<id>/.system_generated/logs/transcript.jsonl, a Codex
// rollout-*.jsonl, or else a Claude <session id>.jsonl.
func parseByPath(path string) (*Session, error) {
	switch {
	case strings.HasSuffix(filepath.ToSlash(path), "/.system_generated/logs/transcript.jsonl"):
		return ParseAgy(path)
	case strings.HasPrefix(filepath.Base(path), "rollout-") && strings.HasSuffix(path, ".jsonl"):
		return ParseCodex(path)
	}
	return ParseClaude(path)
}
