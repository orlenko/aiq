package main

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/orlenko/aiq/internal/diagnostic"
	"github.com/orlenko/aiq/internal/transcript"
)

var errNavListFatal = errors.New("navigator list build failed")

type navListFlags struct {
	internal, resource, discovery, partial bool
	unavailable                            bool
}

// R meters admitted owned text and conservative overhead, not heap/RSS. Raw
// callback results, candidates, shared Index generations and parser work have
// their existing lifetimes. Reservations are deliberately never refunded.
type navListRun struct {
	transcript.Discovery
	parse  func(context.Context, string) (transcript.Summary, error)
	cancel context.CancelFunc
	limit  int64

	mu     sync.Mutex
	used   int64
	state  navListFlags
	needed map[string]struct{}
}

func (r *navListRun) flags() navListFlags {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

func (r *navListRun) interrupted() {
	r.mu.Lock()
	r.state.partial = true
	r.mu.Unlock()
}

func (r *navListRun) internalFailure() {
	r.mu.Lock()
	r.state.internal = true
	r.mu.Unlock()
	r.cancel()
}

func (r *navListRun) reserve(overhead int64, fields ...string) bool {
	r.mu.Lock()
	if r.state.resource || r.state.internal {
		r.mu.Unlock()
		return false
	}
	remaining := r.limit - r.used
	fits := overhead >= 0 && overhead <= remaining
	if fits {
		remaining -= overhead
		for _, s := range fields {
			if int64(len(s)) > remaining {
				fits = false
				break
			}
			remaining -= int64(len(s))
		}
	}
	if fits {
		r.used = r.limit - remaining
	} else {
		r.state.resource = true
	}
	r.mu.Unlock()
	if !fits {
		r.cancel()
	}
	return fits
}

func (r *navListRun) Candidates(ctx context.Context, dir string) ([]transcript.Candidate, error) {
	rows, err := r.Discovery.Candidates(ctx, dir)
	r.discoveryResult(ctx, err)
	return rows, err
}

func (r *navListRun) AllCandidates(ctx context.Context) ([]transcript.Candidate, error) {
	source, ok := r.Discovery.(transcript.AllDiscovery)
	if !ok {
		r.internalFailure()
		return nil, errNavListFatal
	}
	rows, err := source.AllCandidates(ctx)
	r.discoveryResult(ctx, err)
	for i := range rows {
		if ctx.Err() != nil {
			r.interrupted()
			return rows, ctx.Err()
		}
		// Directory associations become admitted row text even when parsing
		// subsequently fails or deduplication discards this candidate.
		if rows[i].Directory != "" {
			if !r.reserve(128, rows[i].Directory) {
				return nil, errNavListFatal
			}
			rows[i].Directory = strings.Clone(rows[i].Directory)
		}
	}
	return rows, err
}

func (r *navListRun) discoveryResult(ctx context.Context, err error) {
	diagnostic.Note(ctx, transcript.PanicOrigin(err))
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		// ListWith may otherwise suppress this failure when child expiry races
		// discovery. Retain classification only, never its potentially private text.
		r.mu.Lock()
		r.state.discovery = true
		r.mu.Unlock()
		r.cancel()
	} else if err != nil || ctx.Err() != nil {
		r.interrupted()
	}
}

func (r *navListRun) summary(ctx context.Context, path string) (out transcript.Summary, err error) {
	defer func() {
		if value := recover(); value != nil {
			origin := transcript.CapturePanic(value)
			diagnostic.Note(ctx, origin)
			r.internalFailure()
			out, err = transcript.Summary{}, errNavListFatal
		}
	}()
	s, err := r.parse(ctx, path)
	diagnostic.Note(ctx, transcript.PanicOrigin(err))
	if err != nil {
		if errors.Is(err, transcript.ErrBudget) {
			r.interrupted()
		} else {
			r.mu.Lock()
			r.state.unavailable = true
			r.mu.Unlock()
		}
		return transcript.Summary{}, err
	}
	if !r.reserve(512, s.Provider, s.ID, s.Path, s.Cwd, s.Label, s.Title, s.Model, s.Branch) {
		return transcript.Summary{}, errNavListFatal
	}
	s.Provider, s.ID, s.Path, s.Cwd = strings.Clone(s.Provider), strings.Clone(s.ID), strings.Clone(s.Path), strings.Clone(s.Cwd)
	s.Label, s.Title, s.Model, s.Branch = strings.Clone(s.Label), strings.Clone(s.Title), strings.Clone(s.Model), strings.Clone(s.Branch)
	if s.Provider == "codex" && s.Title == "" {
		r.mu.Lock()
		r.needed[s.ID] = struct{}{}
		r.mu.Unlock()
	}
	return s, nil
}

func (r *navListRun) CodexNames(ctx context.Context) (out map[string]string) {
	defer func() {
		if value := recover(); value != nil {
			origin := transcript.CapturePanic(value)
			diagnostic.Note(ctx, origin)
			r.internalFailure()
			out = nil
		}
		if ctx.Err() != nil {
			r.interrupted()
		}
	}()
	if ctx.Err() != nil {
		return nil
	}
	names := r.Discovery.CodexNames(ctx)
	if ctx.Err() != nil {
		return nil
	}
	// ListWith has joined summary workers. Still copy under the same mutex,
	// so this ownership rule does not depend on unsynchronized map access.
	r.mu.Lock()
	ids := make([]string, 0, len(r.needed))
	for id := range r.needed {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	out = make(map[string]string, len(ids))
	for _, id := range ids {
		title := names[id]
		if title == "" {
			continue
		}
		if !r.reserve(256, id, title) {
			return nil
		}
		out[strings.Clone(id)] = strings.Clone(title)
	}
	return out
}
