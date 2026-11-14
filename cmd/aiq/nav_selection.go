package main

import (
	"container/list"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"io"
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/orlenko/aiq/internal/navauth"
	"github.com/orlenko/aiq/internal/transcript"
)

type navSelectedParse struct {
	provider, id, path, token string
	context                   string
	snapshot                  transcript.Snapshot
	identity                  transcript.ParseIdentity // No reference to the parsed transcript graph.
}
type navSelections struct {
	mu       sync.Mutex
	key      [32]byte
	sequence uint64
	rows     map[string]*list.Element
	lru      list.List
	cache    *transcript.Cache
}

func newNavSelections(cache *transcript.Cache, entropy io.Reader) (*navSelections, error) {
	if cache == nil || entropy == nil {
		return nil, errNavDetailConfiguration
	}
	r := &navSelections{cache: cache, rows: map[string]*list.Element{}}
	if _, err := io.ReadFull(entropy, r.key[:]); err != nil {
		return nil, errNavDetailConfiguration
	}
	return r, nil
}
func (r *navSelections) register(ctx context.Context, hit transcript.Hit, s *transcript.Session, snap transcript.Snapshot, sourceContext string) (string, error) {
	if ctx == nil || r == nil || r.cache == nil {
		return "", errNavDetailConfiguration
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if s == nil || !transcript.IsFullID(s.Provider, s.ID) || hit.Path != snap.Path || s.Path != hit.Path {
		return "", errNavDetailChanged
	}
	identity, ok := r.cache.SessionIdentity(hit.Path, snap, s)
	if !ok {
		return "", errNavDetailChanged
	}
	key := s.Provider + "/" + s.ID
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if el := r.rows[key]; el != nil {
		old := el.Value.(navSelectedParse)
		if old.path == hit.Path && old.snapshot == snap && old.identity == identity && old.context == sourceContext {
			r.lru.MoveToFront(el)
			return old.token, nil
		}
	}
	if r.sequence == math.MaxUint64 {
		return "", errNavDetailConfiguration
	}
	r.sequence++
	mac := hmac.New(sha256.New, r.key[:])
	var sequence [8]byte
	binary.BigEndian.PutUint64(sequence[:], r.sequence)
	mac.Write([]byte("aiq-nav-selection-v1"))
	mac.Write(sequence[:])
	mac.Write([]byte(key))
	var digest [32]byte
	copy(digest[:], mac.Sum(nil))
	token := navauth.Encode32(digest)
	value := navSelectedParse{provider: strings.Clone(s.Provider), id: strings.Clone(s.ID), path: strings.Clone(hit.Path), token: token, snapshot: snap, identity: identity, context: strings.Clone(sourceContext)}
	if el := r.rows[key]; el != nil {
		el.Value = value
		r.lru.MoveToFront(el)
	} else {
		r.rows[key] = r.lru.PushFront(value)
	}
	if r.lru.Len() > 32 {
		el := r.lru.Back()
		old := el.Value.(navSelectedParse)
		delete(r.rows, old.provider+"/"+old.id)
		r.lru.Remove(el)
	}
	return token, nil
}
func (r *navSelections) lookup(provider, id, token string) (navSelectedParse, bool) {
	if _, err := navauth.Decode32(token); err != nil {
		return navSelectedParse{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	el := r.rows[provider+"/"+id]
	if el == nil {
		return navSelectedParse{}, false
	}
	record := el.Value.(navSelectedParse)
	if !hmac.Equal([]byte(token), []byte(record.token)) {
		return navSelectedParse{}, false
	}
	return record, true
}
func (r *navSelections) current(record navSelectedParse) bool {
	current, ok := r.lookup(record.provider, record.id, record.token)
	if !ok || current.identity != record.identity || current.snapshot != record.snapshot || current.path != record.path {
		return false
	}
	s, snap, ok := r.cache.CachedSession(record.path)
	if !ok || snap != record.snapshot || s.Provider != record.provider || s.ID != record.id {
		return false
	}
	identity, ok := r.cache.SessionIdentity(record.path, snap, s)
	return ok && identity == record.identity
}

// This private scope remains stable across an append to the same file, but
// changes with its copy, inode, root bundle or managed-session context.
func (r *navSelections) prefixScope(hit transcript.Hit, snap transcript.Snapshot, sourceContext string) string {
	mac := hmac.New(sha256.New, r.key[:])
	mac.Write([]byte(navConversationKey("nav-prefix-scope-v1", hit.Path, strconv.FormatUint(snap.Dev, 10), strconv.FormatUint(snap.Ino, 10), sourceContext)))
	var digest [32]byte
	copy(digest[:], mac.Sum(nil))
	return navauth.Encode32(digest)
}
