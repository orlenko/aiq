package transcript

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/orlenko/aiq/internal/diagnostic"
)

// DiscoveryTTL is how long one scan of the Codex rollouts or of
// Antigravity's history answers the navigator.
const DiscoveryTTL = 15 * time.Second

// Index is the navigator's Discovery. Claude is found by project directory,
// as the direct walk finds it. The Codex rollouts and Antigravity's history
// are scanned on a shared TTL: a read past it starts one scan, shared by
// concurrent readers, and is answered from the last completed scan while it
// runs; nothing else starts one. A successfully classified regular entry is
// opened once per (path, inode); linked and unsuccessful heads are reread on each scan,
// and a path the latest scan did not see is dropped. The completion of a
// provider's scan is its generation.
type Index struct {
	r     Roots
	ttl   time.Duration
	now   func() time.Time
	codex scan
	agy   scan
	walks atomic.Int64

	mu   sync.Mutex
	memo map[string]memoEntry
	// Fixture seam after the legacy generation sample, outside memo/scan locks.
	beforeMemoLookup func()
}

// scanObservation describes the completed attempt, not when every reused head
// was read. dataGen identifies the non-aborted publication supplying the data.
type scanObservation struct {
	attemptGen, dataGen uint64
	outcome             scanOutcome
	origin              diagnostic.Origin
}
type scanOutcome uint8

const (
	scanHealthy scanOutcome = iota + 1
	scanPartial
	scanFallbackOld
	scanFallbackEmpty
)

// scan is one provider's latest scan, and the one running.
type scan struct {
	mu          sync.Mutex
	gen         uint64
	at          time.Time
	data        any             // *codexScan or *agyScan
	running     chan struct{}   // closed when the running scan completes
	observation scanObservation // completed tuple only; pending faults stay worker-local
}

type codexScan struct {
	all   []Candidate            // every rollout, roots in order
	byCwd map[string][]Candidate // by the cwd its head names, canonical
	seen  map[string]codexSeen   // by path
	names map[string]string
}

type codexSeen struct {
	dev, ino    uint64
	cwd         string // canonical; "" when the head names none
	interactive bool
	classified  bool // success; only nonzero regular-entry identities permit reuse
}

type agyScan struct {
	byDir map[string][]Candidate
}

// NewIndex is an Index over r.
func NewIndex(r Roots) *Index {
	return &Index{r: r, ttl: DiscoveryTTL, now: time.Now, memo: map[string]memoEntry{}}
}

// Walks is how many scans of the Codex rollouts the index has run.
func (x *Index) Walks() int64 { return x.walks.Load() }

// read is s's data: the last completed scan, starting a new one past the
// TTL. With no scan completed yet it waits for the first, until ctx ends.
func (x *Index) read(ctx context.Context, s *scan, run func(prev any) any) (any, error) {
	d, _, err := x.readObserved(ctx, s, func(prev any, _ *diagnostic.Holder) any { return run(prev) })
	return d, err
}

func notedScan(ctx context.Context, d any, observation scanObservation, err error) (any, scanObservation, error) {
	diagnostic.Note(ctx, observation.origin)
	return d, observation, err
}

func (x *Index) readObserved(ctx context.Context, s *scan, run func(any, *diagnostic.Holder) any) (any, scanObservation, error) {
	s.mu.Lock()
	if s.data != nil && x.now().Sub(s.at) < x.ttl {
		d, observation := s.data, s.observation
		s.mu.Unlock()
		return notedScan(ctx, d, observation, nil)
	}
	if s.running == nil {
		done := make(chan struct{})
		s.running = done
		prev, prevDataGen := s.data, s.observation.dataGen
		go func() {
			d := prev
			var faults diagnostic.Holder
			aborted := false
			func() {
				// The existing run guard retains old data on an aborted attempt.
				defer func() {
					if value := recover(); value != nil {
						origin := capturePanic(value)
						faults.Note(origin)
						aborted = true
					}
				}()
				d = run(prev, &faults)
			}()
			origin := faults.Origin()
			outcome := scanHealthy
			if aborted {
				outcome = scanFallbackEmpty
				if prev != nil {
					outcome = scanFallbackOld
				}
			} else if origin.Present() {
				outcome = scanPartial
			}
			s.mu.Lock()
			// Preserve the existing unguarded publication clock and assignment.
			s.data, s.at, s.running = d, x.now(), nil
			s.gen++
			dataGen := prevDataGen
			if d == nil {
				dataGen = 0
			} else if !aborted {
				dataGen = s.gen
			}
			s.observation = scanObservation{attemptGen: s.gen, dataGen: dataGen, outcome: outcome, origin: origin}
			s.mu.Unlock()
			close(done)
		}()
	}
	d, observation, running := s.data, s.observation, s.running
	s.mu.Unlock()
	if d != nil {
		return notedScan(ctx, d, observation, nil)
	}
	select {
	case <-running:
	case <-ctx.Done():
		// Observe only an already completed tuple, never pending faults.
		select {
		case <-running:
			s.mu.Lock()
			observation = s.observation
			s.mu.Unlock()
		default:
			observation = scanObservation{}
		}
		diagnostic.Note(ctx, observation.origin)
		return nil, observation, ctx.Err()
	}
	s.mu.Lock()
	d, observation = s.data, s.observation
	s.mu.Unlock()
	return notedScan(ctx, d, observation, nil)
}

func (x *Index) codexScan(ctx context.Context) (*codexScan, error) {
	cs, _, err := x.codexScanObserved(ctx)
	return cs, err
}

func (x *Index) codexScanObserved(ctx context.Context) (*codexScan, scanObservation, error) {
	d, observation, err := x.readObserved(ctx, &x.codex, x.scanCodexObserved)
	if err != nil {
		return nil, observation, err
	}
	if cs, _ := d.(*codexScan); cs != nil {
		return cs, observation, nil
	}
	return &codexScan{}, observation, nil
}

func (x *Index) agyScan(ctx context.Context) (*agyScan, error) {
	d, err := x.read(ctx, &x.agy, x.scanAgy)
	if err != nil {
		return nil, err
	}
	if as, _ := d.(*agyScan); as != nil {
		return as, nil
	}
	return &agyScan{}, nil
}

// scanCodex reuses successful regular-entry classifications; linked heads are
// reread because the directory entry's identity does not identify its target.
func (x *Index) scanCodex(prev any) any {
	return x.scanCodexObserved(prev, &diagnostic.Holder{})
}

func (x *Index) scanCodexObserved(prev any, faults *diagnostic.Holder) any {
	return x.scanCodexWithMeta(prev, faults, codexMeta)
}

func (x *Index) scanCodexWithMeta(prev any, faults *diagnostic.Holder, head func(string) (codexPayload, bool)) any {
	x.walks.Add(1)
	old, _ := prev.(*codexScan)
	next := &codexScan{byCwd: map[string][]Candidate{}, seen: map[string]codexSeen{}, names: codexNames(x.r.CodexIndex)}
	type found struct {
		c        Candidate
		dev, ino uint64
	}
	var all []found
	for _, root := range x.r.CodexSessions {
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasPrefix(d.Name(), "rollout-") || !strings.HasSuffix(d.Name(), ".jsonl") {
				return nil
			}
			f := found{c: Candidate{Provider: "codex", Path: p, Root: root, Account: x.r.Labels[root]}}
			if info, err := d.Info(); err == nil && info.Mode()&os.ModeSymlink == 0 {
				f.dev, f.ino = fileID(info)
			}
			all = append(all, f)
			return nil
		})
	}
	seen := make([]codexSeen, len(all))
	parallel(len(all), func(i int) {
		f := all[i]
		if old != nil {
			if s, ok := old.seen[f.c.Path]; ok && s.classified && s.ino != 0 && s.dev == f.dev && s.ino == f.ino {
				seen[i] = s
				return
			}
		}
		seen[i] = codexSeen{dev: f.dev, ino: f.ino}
		// A head that makes the decoder panic leaves that rollout out, and
		// the worker carries on.
		defer func() {
			if value := recover(); value != nil {
				origin := capturePanic(value)
				faults.Note(origin)
			}
		}()
		if meta, ok := head(f.c.Path); ok {
			seen[i].cwd, seen[i].interactive = canonical(meta.Cwd), interactive(meta)
			seen[i].classified = true
		}
	})
	for i, f := range all {
		next.all = append(next.all, f.c)
		next.seen[f.c.Path] = seen[i]
		if cwd := seen[i].cwd; cwd != "" {
			next.byCwd[cwd] = append(next.byCwd[cwd], f.c)
		}
	}
	return next
}

// scanAgy maps each recorded workspace to its conversations' transcripts.
func (x *Index) scanAgy(any) any {
	next := &agyScan{byDir: map[string][]Candidate{}}
	type association struct{ dir, path string }
	seen := map[association]bool{}
	for _, root := range x.r.AgyAppData {
		for dir, ids := range agyWorkspaces(context.Background(), root) {
			for _, id := range ids {
				p := AgyTranscript(root, id)
				key := association{dir, p}
				if seen[key] {
					continue
				}
				if _, err := os.Stat(p); err != nil {
					continue
				}
				seen[key] = true
				next.byDir[dir] = append(next.byDir[dir], Candidate{Provider: "agy", Path: p})
			}
		}
	}
	return next
}

func (x *Index) Candidates(ctx context.Context, dir string) ([]Candidate, error) {
	out := claudeFiles(ctx, x.r.ClaudeProjects, dir)
	cs, err := x.codexScan(ctx)
	if err != nil {
		return out, err
	}
	out = append(out, cs.byCwd[dir]...)
	as, err := x.agyScan(ctx)
	if err != nil {
		return out, err
	}
	out = append(out, as.byDir[dir]...)
	return out, ctx.Err()
}

func (x *Index) CodexNames(ctx context.Context) map[string]string {
	cs, err := x.codexScan(ctx)
	if err != nil {
		return nil
	}
	return cs.names
}

func (x *Index) ByID(ctx context.Context, provider, id string) ([]Candidate, error) {
	out, _, err := x.byIDObserved(ctx, provider, id)
	return out, err
}

func (x *Index) byIDObserved(ctx context.Context, provider, id string) ([]Candidate, scanObservation, error) {
	switch provider {
	case "claude":
		return claudeByID(ctx, x.r, id), scanObservation{}, ctx.Err()
	case "codex":
		cs, observation, err := x.codexScanObserved(ctx)
		if err != nil {
			return nil, observation, err
		}
		var out []Candidate
		name := "-" + id + ".jsonl"
		for _, c := range cs.all {
			if strings.HasSuffix(c.Path, name) {
				out = append(out, c)
			}
		}
		return out, observation, nil
	case "agy":
		return agyByID(x.r, id), scanObservation{}, ctx.Err()
	}
	return nil, scanObservation{}, nil
}

func (x *Index) Written(ctx context.Context, provider, dir string, since time.Time) []string {
	dir = canonical(dir)
	var cands []Candidate
	switch provider {
	case "codex":
		cs, err := x.codexScan(ctx)
		if err != nil {
			return nil
		}
		for _, c := range cs.byCwd[dir] {
			if cs.seen[c.Path].interactive {
				cands = append(cands, c)
			}
		}
	case "agy":
		as, err := x.agyScan(ctx)
		if err != nil {
			return nil
		}
		cands = as.byDir[dir]
	}
	return newestWritten(cands, since)
}

// generation is provider's count of completed scans; Claude has none.
func (x *Index) generation(provider string) uint64 {
	var s *scan
	switch provider {
	case "codex":
		s = &x.codex
	case "agy":
		s = &x.agy
	default:
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gen
}
