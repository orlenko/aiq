package diagnostic

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestHolderDerivedContextFirstUnknownAndIsolation(t *testing.T) {
	parent, holder := WithHolder(context.Background())
	derived, cancel := context.WithCancel(parent)
	defer cancel()
	unknown := Capture(nil)
	known := caught(explicitLeaf)
	if !unknown.Present() || !known.Present() {
		t.Fatal("fixture origins absent")
	}
	Note(derived, Origin{})
	if holder.Origin().Present() {
		t.Fatal("ordinary result fabricated a panic")
	}
	Note(derived, unknown)
	Note(parent, known)
	if holder.Origin() != unknown {
		t.Fatal("later known origin replaced first unknown")
	}
	other, separate := WithHolder(parent)
	Note(other, known)
	if separate.Origin() != known || holder.Origin() != unknown {
		t.Fatal("nested operation shared its parent's sink")
	}
	Note(context.Background(), known) // no opt-in holder: no retained observation
}

func TestHolderConcurrentNotesPublishOneCompleteValue(t *testing.T) {
	known, unknown := caught(explicitLeaf), Capture(nil)
	for range 20 {
		var holder Holder
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range 8 {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				if i%2 == 0 {
					holder.Note(known)
				} else {
					holder.Note(unknown)
				}
				_ = holder.Origin()
			}(i)
		}
		close(start)
		wg.Wait()
		first := holder.Origin()
		if first != known && first != unknown {
			t.Fatal("concurrent note published a torn or absent value")
		}
		holder.Note(known)
		holder.Note(unknown)
		if holder.Origin() != first {
			t.Fatal("completed first observation changed")
		}
	}
}

func TestTranscriptFailureKindsKeepFixedBases(t *testing.T) {
	origin := caught(explicitLeaf)
	for _, test := range []struct {
		kind FailureKind
		text string
	}{
		{Unreadable, "transcript could not be read"}, {Inspection, "transcript: cached inspection failed"},
	} {
		if Base(test.kind).Error() != test.text || Failure(test.kind, Origin{}) != Base(test.kind) {
			t.Fatal("ordinary base identity/text changed")
		}
		err := Failure(test.kind, origin)
		if err.Error() != test.text || !errors.Is(err, Base(test.kind)) || FromError(err) != origin {
			t.Fatal("fixed classified failure lost its safe origin")
		}
	}
}
