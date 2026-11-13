package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/orlenko/aiq/internal/transcript"
)

func validateConvoID(id string) error {
	for i := 0; i < len(id); i++ {
		if id[i] < 0x20 || id[i] == 0x7f {
			return fmt.Errorf("invalid session id %q: control characters are not allowed", id)
		}
	}
	return nil
}

var (
	errConvoIdentity    = errors.New("transcript session identity mismatch")
	errConvoAmbiguousID = errors.New("session id matches multiple providers")
)

// The cause is available to callers, never interpolated into terminal output.
type convoLoadError struct {
	path  string
	cause error
}

func (e *convoLoadError) Error() string { return fmt.Sprintf("could not read transcript %q", e.path) }
func (e *convoLoadError) Unwrap() error { return e.cause }

type convoLookupError struct {
	id    string
	cause error
}

func (e *convoLookupError) Error() string {
	switch {
	case errors.Is(e.cause, transcript.ErrNoSuchSession):
		return fmt.Sprintf("no readable session %q under configured roots", e.id)
	case errors.Is(e.cause, errConvoAmbiguousID):
		return fmt.Sprintf("session %q matches multiple providers under configured roots", e.id)
	default:
		return fmt.Sprintf("could not find session %q under configured roots", e.id)
	}
}

func (e *convoLookupError) Unwrap() error { return e.cause }

// resolveGlobalConvo delegates copy selection to the shared resolver. A hit
// from one provider is not unique until all providers have been checked.
func resolveGlobalConvo(ctx context.Context, src transcript.Discovery, id string, parse func(context.Context, string) (transcript.Summary, error)) (convoTarget, error) {
	fail := func(err error) (convoTarget, error) {
		return convoTarget{}, &convoLookupError{id: id, cause: err}
	}
	if err := validateConvoID(id); err != nil {
		return convoTarget{}, err
	}
	if !transcript.IsFullID("claude", id) {
		return fail(transcript.ErrBadID)
	}
	var hits []convoTarget
	for _, provider := range []string{"claude", "codex", "agy"} {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		hit, err := transcript.FindByID(ctx, src, provider, id, parse)
		if err != nil {
			if errors.Is(err, transcript.ErrNoSuchSession) || errors.Is(err, transcript.ErrBadID) {
				continue
			}
			return fail(err)
		}
		hits = append(hits, convoTarget{provider: provider, path: hit.Path, id: id, global: true})
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	switch len(hits) {
	case 0:
		return fail(transcript.ErrNoSuchSession)
	case 1:
		return hits[0], nil
	default:
		return fail(errConvoAmbiguousID)
	}
}

// A target is copied into follow/screen state. Copies share this once-owned
// diagnostic plan so an early command exit and its loader cannot both report.
type convoWarnings struct {
	once     sync.Once
	paths    []string
	selected string
	warn     io.Writer
}

func (t convoTarget) flushWarnings(attempt bool) {
	w := t.warnings
	if w == nil {
		return
	}
	w.once.Do(func() {
		paths := w.paths
		if attempt {
			paths = make([]string, 0, len(w.paths))
			for _, path := range w.paths {
				if path != w.selected {
					paths = append(paths, path)
				}
			}
		}
		reportUnreadable(w.warn, paths)
		w.paths, w.selected, w.warn = nil, "", nil
	})
}
