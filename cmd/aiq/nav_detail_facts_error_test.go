package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"

	"github.com/orlenko/aiq/internal/transcript"
)

func TestNavDetailFactsStorageFailureIsNotMissingSession(t *testing.T) {
	for _, cause := range []error{os.ErrNotExist, &os.PathError{Op: "open", Path: "fixture-metadata.db", Err: os.ErrNotExist}, fmt.Errorf("fixture wrapper: %w", os.ErrNotExist), transcript.ErrNoSuchSession, errors.New("fixture storage failure"), transcript.ErrBadID, transcript.ErrTooLarge, transcript.ErrUnshowable} {
		for _, condition := range []string{"", "*"} {
			f := detailTestNew(t)
			var facts, projections atomic.Int32
			f.deps.facts = func(context.Context, transcript.Hit, *transcript.Session) (navDetailFacts, error) {
				facts.Add(1)
				return navDetailFacts{}, cause
			}
			f.deps.project = func(s *transcript.Session, o navConversationOptions) (*navConversationWindow, error) {
				projections.Add(1)
				return projectNavConversation(s, o)
			}
			w := detailTestServe(f.handler(t), detailTestConditional(condition))
			detailTestError(t, w, 500, "internal_error")
			stats := f.cache.Stats()
			if facts.Load() != 1 || projections.Load() != 0 || stats.Parses != 1 || stats.ViewBuilds != 0 || stats.FailedViews != 0 || w.Header().Get("ETag") != "" {
				t.Fatal("facts failure skipped selected load, projected, or issued a validator")
			}
		}
	}
}

func TestNavDetailFactsPreparingRemainsRetryable(t *testing.T) {
	for _, cause := range []error{transcript.ErrPreparing, context.Canceled, context.DeadlineExceeded} {
		f := detailTestNew(t)
		f.deps.facts = func(context.Context, transcript.Hit, *transcript.Session) (navDetailFacts, error) {
			return navDetailFacts{}, cause
		}
		detailTestError(t, detailTestServe(f.handler(t), detailTestConditional("*")), 503, "preparing")
	}
}
