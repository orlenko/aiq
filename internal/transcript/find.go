package transcript

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/diagnostic"
)

// ErrBadID is an id FindByID refuses before it builds any path.
var ErrBadID = errors.New("transcript: not a session id")

// ErrNoSuchSession is a full id no root holds.
var ErrNoSuchSession = errors.New("transcript: no such session under any account")

// Hit is where a session lives: its transcript, the root it was found
// under, and that root's account label ("" for an unlabelled root).
type Hit struct {
	Path, Root, Account string
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// IsFullID reports whether provider accepts id for exact lookup across roots.
func IsFullID(provider, id string) bool { return validFullID(provider, id) }

// validFullID says id may be looked up for provider: not empty, at most 128
// bytes, not . or .. or starting with a dot, with no slash, backslash,
// control character, DEL or glob character, and a UUID, as every provider's
// session ids are.
func validFullID(provider, id string) bool {
	if id == "" || len(id) > 128 || id[0] == '.' || strings.ContainsAny(id, `/\*?[]`) {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x20 || id[i] == 0x7f {
			return false
		}
	}
	switch provider {
	case "claude", "codex", "agy":
		return uuidPattern.MatchString(id)
	}
	return false
}

// FindByID finds the transcript of provider's session id across every root
// src searches, under ctx. A file reached under several roots (a link, a
// bind mount) is one copy, known by the device and inode of the regular
// file opened; it is reported under an unlabelled root if it has one, else
// under the first in Roots order, so a copy that is also under the real
// home forces no account. Of several copies the newest by the opened
// file's modification time wins; parse, which is ListWith's, breaks a tie by
// the session's Updated, and the first in Roots order breaks the rest.
func FindByID(ctx context.Context, src Discovery, provider, id string, parse func(ctx context.Context, path string) (Summary, error)) (Hit, error) {
	if !validFullID(provider, id) {
		return Hit{}, ErrBadID
	}
	if err := ctx.Err(); err != nil {
		return Hit{}, err
	}
	cands, err := src.ByID(ctx, provider, id)
	if err != nil {
		return Hit{}, err
	}
	type fileKey struct {
		dev, ino uint64
		path     string // where there is no inode
	}
	type copyOf struct {
		hit     Hit
		mtime   time.Time
		updated *time.Time
	}
	var copies []*copyOf
	byFile := map[fileKey]*copyOf{}
	for _, c := range cands {
		if ctx.Err() != nil {
			return Hit{}, ctx.Err()
		}
		f, err := OpenRegular(c.Path)
		if err != nil {
			continue
		}
		fi, err := f.Stat()
		f.Close()
		if err != nil {
			continue
		}
		k := fileKey{}
		if k.dev, k.ino = fileID(fi); k.ino == 0 {
			k.path = c.Path
		}
		if cp := byFile[k]; cp != nil {
			if cp.hit.Account != "" && c.Account == "" {
				cp.hit = Hit{Path: c.Path, Root: c.Root}
			}
			continue
		}
		cp := &copyOf{hit: Hit{Path: c.Path, Root: c.Root, Account: c.Account}, mtime: fi.ModTime()}
		byFile[k] = cp
		copies = append(copies, cp)
	}
	if err := ctx.Err(); err != nil {
		return Hit{}, err
	}
	if len(copies) == 0 {
		return Hit{}, ErrNoSuchSession
	}
	updated := func(cp *copyOf) (time.Time, error) {
		if err := ctx.Err(); err != nil {
			return time.Time{}, err
		}
		if cp.updated == nil {
			s, err := recovered(ctx, parse, cp.hit.Path)
			if ctxErr := ctx.Err(); ctxErr != nil {
				return time.Time{}, ctxErr
			}
			if errors.Is(err, ErrBudget) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return time.Time{}, err
			}
			cp.updated = &s.Updated
		}
		return *cp.updated, nil
	}
	best := copies[0]
	for _, cp := range copies[1:] {
		switch {
		case cp.mtime.After(best.mtime):
			best = cp
		case cp.mtime.Equal(best.mtime):
			at, err := updated(cp)
			if err != nil {
				return Hit{}, err
			}
			bestAt, err := updated(best)
			if err != nil {
				return Hit{}, err
			}
			if at.After(bestAt) {
				best = cp
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return Hit{}, err
	}
	return best.hit, nil
}

type memoEntry struct {
	hit         Hit
	gen         uint64 // legacy validity token, not the candidate-source generation
	at          time.Time
	observation scanObservation
}

// One synchronous FindByID call consumes ByID once. Capture that operation's
// actual source observation, rather than the caller's accumulated diagnostics.
type observedDiscovery struct {
	*Index
	observation scanObservation
}

func (d *observedDiscovery) ByID(ctx context.Context, provider, id string) ([]Candidate, error) {
	out, observation, err := d.byIDObserved(ctx, provider, id)
	d.observation = observation
	return out, err
}

// memoMax is how many lookups the index remembers before it drops the
// expired ones.
const memoMax = 4096

// Find is FindByID through the index, remembered for at most one discovery
// TTL and forgotten when the provider's next scan completes, so a newer
// copy under another root is followed within that time. A lookup it does
// not remember goes through the index, which walks nothing within its TTL.
func (x *Index) Find(ctx context.Context, provider, id string, parse func(ctx context.Context, path string) (Summary, error)) (Hit, error) {
	if err := ctx.Err(); err != nil {
		return Hit{}, err
	}
	k := provider + "\x00" + id
	gen := x.generation(provider)
	if x.beforeMemoLookup != nil {
		x.beforeMemoLookup()
	}
	x.mu.Lock()
	if e, ok := x.memo[k]; ok && e.gen == gen && x.now().Sub(e.at) < x.ttl {
		x.mu.Unlock()
		diagnostic.Note(ctx, e.observation.origin)
		if err := ctx.Err(); err != nil {
			return Hit{}, err
		}
		return e.hit, nil
	}
	x.mu.Unlock()
	source := &observedDiscovery{Index: x}
	hit, err := FindByID(ctx, source, provider, id, parse)
	if err != nil {
		return hit, err
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Hit{}, err
	}
	if len(x.memo) >= memoMax {
		for k, e := range x.memo {
			if x.now().Sub(e.at) >= x.ttl {
				delete(x.memo, k)
			}
		}
		if len(x.memo) >= memoMax {
			clear(x.memo)
		}
	}
	x.memo[k] = memoEntry{hit: hit, gen: gen, at: x.now(), observation: source.observation}
	return hit, nil
}
