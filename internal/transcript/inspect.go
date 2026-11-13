package transcript

import (
	"context"
	"errors"
	"strings"
	"time"
)

// ErrInspection is an internal failure to inspect retained cache metadata.
// It says nothing about whether a transcript could be parsed or rendered.
var ErrInspection = errors.New("transcript: cached inspection failed")

// CacheInspection describes retained observations, not filesystem existence,
// process liveness or the copy a later discovery request would select.
type CacheInspection uint8

const (
	InspectionMissing CacheInspection = iota
	InspectionPresent
	InspectionAmbiguous
	InspectionUnusable
)

// CachedConversation contains only a captured parse's scalar metadata. Snapshot
// includes a private path and must not be serialized wholesale by a transport.
// HasTail distinguishes an absent turn from a turn with zero timestamps.
type CachedConversation struct {
	Snapshot               Snapshot
	Unfinished             bool
	HasTail, TailOpen      bool
	TailStarted, TailEnded time.Time
}

// InspectConversation observes cached parses without filesystem work, discovery,
// parsing, LRU touches, hold expiry or other cache maintenance. Only Present
// returns a payload. A captured result can become old immediately after unlock;
// it conveys no authority to publish a view or amend a later incarnation.
//
// ID validation is pure and precedes acquisition. On ordinary completion,
// cancellation wins even over a bad ID or a non-Present result. Unexpected
// internal faults return ErrInspection. Mutex acquisition and the existing
// trusted clock are not interruptible; cancellation is checked around them.
func (c *Cache) InspectConversation(ctx context.Context, provider, id string) (out CachedConversation, status CacheInspection, err error) {
	defer func() {
		if recover() != nil {
			out, status, err = CachedConversation{}, InspectionMissing, ErrInspection
		}
	}()
	valid := IsFullID(provider, id)
	if err := ctx.Err(); err != nil {
		return CachedConversation{}, InspectionMissing, err
	}
	if !valid {
		return CachedConversation{}, InspectionMissing, ErrBadID
	}
	out, status, err = c.inspectConversationLocked(ctx, provider, id)
	if err == ErrInspection {
		return CachedConversation{}, InspectionMissing, err
	}
	if stopped := ctx.Err(); stopped != nil {
		return CachedConversation{}, InspectionMissing, stopped
	}
	if err != nil || status != InspectionPresent {
		return CachedConversation{}, status, err
	}
	out.Snapshot.Path = strings.Clone(out.Snapshot.Path)
	if stopped := ctx.Err(); stopped != nil {
		return CachedConversation{}, InspectionMissing, stopped
	}
	return out, status, nil
}

func (c *Cache) inspectConversationLocked(ctx context.Context, provider, id string) (CachedConversation, CacheInspection, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return CachedConversation{}, InspectionMissing, err
	}
	var candidate CachedConversation
	var found, ambiguous, heldSeen bool
	consider := func(p *parsed) {
		if p.sess.Provider != provider || p.sess.ID != id {
			return
		}
		if found {
			ambiguous = true
			return
		}
		found = true
		candidate.Snapshot, candidate.Unfinished = p.snap, p.sess.Unfinished
		if n := len(p.sess.Turns); n > 0 {
			tail := &p.sess.Turns[n-1]
			candidate.HasTail, candidate.TailOpen = true, tail.Open
			candidate.TailStarted, candidate.TailEnded = tail.Started, tail.Ended
		}
	}
	for path, el := range c.full {
		if err := ctx.Err(); err != nil {
			return CachedConversation{}, InspectionMissing, err
		}
		if el == nil {
			return CachedConversation{}, InspectionMissing, ErrInspection
		}
		p, ok := el.Value.(*parsed)
		if !ok || p == nil || p.sess == nil || p.snap.Path != path {
			return CachedConversation{}, InspectionMissing, ErrInspection
		}
		heldSeen = heldSeen || p == c.held
		consider(p)
	}
	if p := c.held; p != nil && !heldSeen {
		if p.sess == nil {
			return CachedConversation{}, InspectionMissing, ErrInspection
		}
		if p.sess.Provider == provider && p.sess.ID == id {
			if err := ctx.Err(); err != nil {
				return CachedConversation{}, InspectionMissing, err
			}
			// Unlike heldLocked, this does not clear an expired record or
			// retire its views. Full entries have no corresponding expiry.
			if c.now().Sub(p.at) < c.cfg.floor {
				consider(p)
			}
		}
	}
	if ambiguous {
		return CachedConversation{}, InspectionAmbiguous, nil
	}
	if !found {
		return CachedConversation{}, InspectionMissing, nil
	}
	// A newer failed parse can coexist with an older good parse at this
	// path. Do not call quarantinedLocked, which would clear that failure.
	if _, blocked := c.quarantine[candidate.Snapshot.Path]; blocked {
		return CachedConversation{}, InspectionUnusable, nil
	}
	return candidate, InspectionPresent, nil
}
