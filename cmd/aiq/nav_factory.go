package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/orlenko/aiq/internal/daemon"
	"github.com/orlenko/aiq/internal/diagnostic"
	"github.com/orlenko/aiq/internal/navroute"
	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/provider/agy"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

type navFactoryPolicy struct {
	sessions       navSessionsLimits
	detail         navDetailLimits
	asks           navAsksHTTPLimits
	placementBytes int64
	timeout        time.Duration
}

// Direct owner resource bundle, 2026-11-13. These are distinct admission/work
// units, not a claim about total process/SQLite/parser/discovery memory.
func navProductionPolicy() navFactoryPolicy {
	const bytes = 16 << 20
	const entries = 262144
	const units = 16777216
	return navFactoryPolicy{
		sessions: navSessionsLimits{rowBytes: bytes, bodyBytes: bytes, builders: 1, arrayEntries: entries},
		detail:   navDetailLimits{bodyBytes: bytes, requests: 2, arrayEntries: entries, stringUnits: units},
		asks: navAsksHTTPLimits{requests: 1, reader: state.AsksSnapshotLimits{Rows: 50000, TextBytes: bytes},
			response: navAsksResponseOptions{width: 100, rows: 50000, inputBytes: bytes, renderCells: 1 << 34, arrayEntries: entries, stringUnits: units, bodyBytes: bytes}},
		placementBytes: 65536, timeout: 5 * time.Second,
	}
}

type navFactoryInput struct {
	roots    func(context.Context) (transcript.Roots, error)
	asksPath string
	now      func() time.Time
	location *time.Location
}
type navFactoryBundle struct{ sessions, detail, asks, turn http.Handler }
type navFactory struct {
	mu       sync.Mutex
	input    navFactoryInput
	policy   navFactoryPolicy
	cache    *transcript.Cache
	building chan struct{}
	bundle   *navFactoryBundle
	api      http.Handler
	pages    http.Handler
}

func newNavFactory(in navFactoryInput, p navFactoryPolicy) (*navFactory, error) {
	if in.roots == nil || in.asksPath == "" || in.now == nil || in.location == nil || p.timeout <= 0 || p.placementBytes < 64 ||
		p.detail.arrayEntries <= 0 || p.detail.stringUnits <= 0 || p.sessions.arrayEntries != p.detail.arrayEntries ||
		p.sessions.bodyBytes != p.detail.bodyBytes || p.asks.response.bodyBytes != p.detail.bodyBytes ||
		p.asks.response.arrayEntries != uint64(p.detail.arrayEntries) || p.asks.response.stringUnits != uint64(p.detail.stringUnits) ||
		p.detail.stringUnits < p.sessions.bodyBytes {
		return nil, errors.New("invalid navigator factory configuration")
	}
	pages, err := daemon.NavigatorPagesWithLimits(daemon.NavigatorPageLimits{BodyBytes: p.detail.bodyBytes, PlacementBytes: p.placementBytes, ArrayEntries: p.detail.arrayEntries, StringUnits: p.detail.stringUnits, Rows: int64(p.asks.response.rows)})
	if err != nil {
		return nil, errors.New("invalid navigator page configuration")
	}
	in.asksPath = strings.Clone(in.asksPath)
	f := &navFactory{input: in, policy: p, cache: transcript.NewCache(), pages: pages}
	mux := navroute.NewMux()
	for _, kind := range []navroute.Route{navroute.Sessions, navroute.Conversation, navroute.Asks, navroute.AskTurn} {
		kind := kind
		if err := mux.Handle(kind, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), f.policy.timeout)
			defer cancel()
			bundle, err := f.get(ctx)
			if err != nil {
				diagnostic.Note(ctx, transcript.PanicOrigin(err))
				navFactoryUnavailable(w)
				return
			}
			var h http.Handler
			switch kind {
			case navroute.Sessions:
				h = bundle.sessions
			case navroute.Conversation:
				h = bundle.detail
			case navroute.Asks:
				h = bundle.asks
			case navroute.AskTurn:
				h = bundle.turn
			}
			h.ServeHTTP(w, r.WithContext(ctx))
		})); err != nil {
			return nil, errors.New("invalid navigator route configuration")
		}
	}
	f.api = mux
	return f, nil
}
func navFactoryUnavailable(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(503)
	_, _ = w.Write([]byte(`{"error":"navigator_unavailable"}`))
}
func (f *navFactory) get(ctx context.Context) (bundle *navFactoryBundle, err error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		f.mu.Lock()
		if f.bundle != nil {
			b := f.bundle
			f.mu.Unlock()
			return b, nil
		}
		if done := f.building; done != nil {
			f.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-done:
				continue
			}
		}
		done := make(chan struct{})
		f.building = done
		f.mu.Unlock()
		// Cleanup is armed before roots/constructor callouts, including panic.
		defer func() {
			f.mu.Lock()
			if err == nil && bundle != nil {
				f.bundle = bundle
			}
			f.building = nil
			close(done)
			f.mu.Unlock()
		}()
		return f.build(ctx)
	}
}
func (f *navFactory) build(ctx context.Context) (out *navFactoryBundle, err error) {
	defer func() {
		if v := recover(); v != nil {
			if v == http.ErrAbortHandler {
				panic(v)
			}
			origin := transcript.CapturePanic(v)
			diagnostic.Note(ctx, origin)
			out = nil
			err = diagnostic.Failure(diagnostic.Unshowable, origin)
		}
	}()
	roots, err := f.input.roots(ctx)
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	roots = cloneNavRoots(roots) // MUST precede constructing the shared Index.
	index := transcript.NewIndex(roots)
	sessions, err := newNavSessionsHandler(navSessionsDeps{roots: roots, source: index, summary: f.cache.Summary}, f.policy.sessions)
	if err != nil {
		return nil, err
	}
	resolve := func(ctx context.Context, p, id string) (transcript.Hit, error) {
		return index.Find(ctx, p, id, f.cache.Summary)
	}
	detail, err := newNavDetailHandler(navDetailDeps{cache: f.cache, resolve: resolve, now: f.input.now, location: f.input.location, build: daemon.NavigatorBuild(), skippedNote: transcript.SkippedNote(), liveMark: "▌",
		facts: func(ctx context.Context, hit transcript.Hit, selected *transcript.Session) (navDetailFacts, error) {
			current, snap, err := f.cache.Session(ctx, hit.Path)
			if err != nil {
				return navDetailFacts{}, err
			}
			if current != selected {
				return navDetailFacts{}, errNavDetailChanged
			}
			quiet := f.input.now().Sub(time.Unix(0, snap.Mtime))
			// Manual snapshots do not infer live/ended/moved from backend refs.
			return navDetailFacts{hold: selected.Unfinished && quiet >= 0 && quiet < convoQuiet}, nil
		}}, f.policy.detail)
	if err != nil {
		return nil, err
	}
	read := func(ctx context.Context, now time.Time, l state.AsksSnapshotLimits) (snapshot *state.AsksSnapshot, err error) {
		store, err := state.OpenReadOnly(f.input.asksPath)
		if err != nil {
			return nil, errNavAsksStoreUnavailable
		}
		defer func() {
			if e := store.Close(); e != nil && err == nil {
				snapshot = nil
				err = errNavAsksStoreUnavailable
			}
		}()
		snapshot, err = store.ReadAsksSnapshot(ctx, now, l)
		if err == state.ErrAsksSnapshotUnavailable {
			err = errNavAsksStoreUnavailable
		}
		return snapshot, err
	}
	asks, err := newNavAsksHandler(navAsksHTTPDeps{read: read, now: f.input.now, resolve: navFactoryAlias}, f.policy.asks)
	if err != nil {
		return nil, err
	}
	point := func(ctx context.Context, id, limit int64) (item state.AskPlacement, err error) {
		store, err := state.OpenReadOnly(f.input.asksPath)
		if err != nil {
			return state.AskPlacement{}, errNavAsksStoreUnavailable
		}
		defer func() {
			if e := store.Close(); e != nil && err == nil {
				item = state.AskPlacement{}
				err = errNavAsksStoreUnavailable
			}
		}()
		return store.ReadAskPlacement(ctx, id, limit)
	}
	turn, err := newNavAskTurnHandler(navAskTurnDeps{read: point, resolve: resolve, cache: f.cache}, asks.slots, f.policy.asks.reader.TextBytes, f.policy.placementBytes)
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return &navFactoryBundle{sessions, detail, asks, turn}, nil
}
func navFactoryAlias(ctx context.Context, key string) (navAsksAlias, error) {
	if err := ctx.Err(); err != nil {
		return navAsksAlias{}, err
	}
	value, err := filepath.EvalSymlinks(key)
	if stopped := ctx.Err(); stopped != nil {
		return navAsksAlias{}, stopped
	}
	if os.IsNotExist(err) {
		return navAsksAlias{}, nil
	}
	if err != nil {
		return navAsksAlias{}, errNavAsksAliasUnavailable
	}
	return navAsksAlias{path: value, resolved: true}, nil
}

// Capture configuration paths only. Overlay enumeration remains lazy and
// authenticated; a failed enumeration is not converted to a complete empty set.
func navProductionInput() navFactoryInput {
	claude, codex, agyHome := paths.RealClaudeHome(), paths.RealCodexHome(), agy.AppDataDir(paths.RealAgyHome())
	parents := []string{paths.ClaudeHomesDir(), paths.CodexHomesDir()}
	return navFactoryInput{asksPath: filepath.Join(paths.DataDir(), asksDBName), now: time.Now, location: time.Local,
		roots: func(ctx context.Context) (transcript.Roots, error) {
			r := transcript.Roots{ClaudeProjects: []string{filepath.Join(claude, "projects")}, CodexSessions: []string{filepath.Join(codex, "sessions")}, CodexIndex: []string{filepath.Join(codex, "session_index.jsonl")}, AgyAppData: []string{agyHome}}
			for _, parent := range parents {
				if err := ctx.Err(); err != nil {
					return transcript.Roots{}, err
				}
				homes, err := os.ReadDir(parent)
				if os.IsNotExist(err) {
					continue
				}
				if err != nil {
					return transcript.Roots{}, errors.New("navigator roots unavailable")
				}
				for _, home := range homes {
					if err := ctx.Err(); err != nil {
						return transcript.Roots{}, err
					}
					p := filepath.Join(parent, home.Name())
					project := filepath.Join(p, "projects")
					sessions := filepath.Join(p, "sessions")
					if s, e := os.Lstat(project); e == nil && s.IsDir() {
						r.ClaudeProjects = append(r.ClaudeProjects, project)
					} else if e != nil && !os.IsNotExist(e) {
						return transcript.Roots{}, errors.New("navigator roots unavailable")
					}
					if s, e := os.Lstat(sessions); e == nil && s.IsDir() {
						r.CodexSessions = append(r.CodexSessions, sessions)
						r.CodexIndex = append(r.CodexIndex, filepath.Join(p, "session_index.jsonl"))
					} else if e != nil && !os.IsNotExist(e) {
						return transcript.Roots{}, errors.New("navigator roots unavailable")
					}
				}
			}
			return r, ctx.Err()
		}}
}
func mountProductionNavigator(s *daemon.Server) {
	f, err := newNavFactory(navProductionInput(), navProductionPolicy())
	if err != nil {
		s.MountNavigator(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { navFactoryUnavailable(w) }))
		s.MountNavigatorPages(daemon.NavigatorUnavailablePages())
		return
	}
	s.MountNavigator(f.api)
	s.MountNavigatorPages(f.pages)
}
