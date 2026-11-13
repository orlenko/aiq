package navauth

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"math"
	"sync"
	"time"
)

const (
	PendingLimit    = 64
	CredentialLimit = 32
	NonceLimit      = 8192
	codeLife        = int64(120000)
	idleLife        = int64(30 * 60 * 1000)
	absoluteLife    = int64(8 * 60 * 60 * 1000)
)

var ErrCapacity = errors.New("navigator authentication capacity unavailable")

// Kept separate from refusal so an admitted action can return a signed capacity
// response. Unadmitted proof failures always remain ErrInvalid.
type pending struct {
	expires         int64
	epoch, identity uint64
}
type credential struct {
	born, used int64
	sequence   uint64
}

// State belongs to exactly one listening generation. It is never repointed to a
// replacement listener. Only digests of browser codes and credentials are retained.
type State struct {
	mu              sync.Mutex
	key             [32]byte
	binding         Binding
	wall, elapsedMS int64
	elapsed         func() time.Duration
	random          io.Reader
	active          bool
	epoch, sequence uint64
	codes           map[[32]byte]pending
	credentials     map[[32]byte]credential
	nonces          map[[32]byte]int64
}

func New(key [32]byte, binding Binding) (*State, error) {
	start := time.Now()
	return newState(key, binding, start.UnixMilli(), func() time.Duration { return time.Since(start) }, rand.Reader)
}

func newState(key [32]byte, binding Binding, wall int64, elapsed func() time.Duration, random io.Reader) (*State, error) {
	if !binding.valid() || wall < 0 || elapsed == nil || random == nil {
		return nil, ErrInvalid
	}
	return &State{key: key, binding: binding, wall: wall, elapsed: elapsed, random: random, active: true, epoch: 1,
		codes: make(map[[32]byte]pending), credentials: make(map[[32]byte]credential), nonces: make(map[[32]byte]int64)}, nil
}

func (s *State) retireLocked() {
	s.active = false
	if s.epoch < math.MaxUint64 {
		s.epoch++
	}
}

// nowLocked uses startup wall time plus nondecreasing monotonic elapsed time.
func (s *State) nowLocked() (int64, bool) {
	if !s.active {
		return 0, false
	}
	e := s.elapsed().Milliseconds()
	if e > s.elapsedMS {
		s.elapsedMS = e
	}
	if s.elapsedMS > math.MaxInt64-s.wall {
		s.retireLocked()
		return 0, false
	}
	return s.wall + s.elapsedMS, true
}

func (s *State) Now() (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now, ok := s.nowLocked()
	if !ok {
		return 0, ErrInvalid
	}
	return now, nil
}
func (s *State) Close() { s.mu.Lock(); defer s.mu.Unlock(); s.retireLocked() }

// Facts cannot be constructed outside this package. A verified body is owned by
// these facts; callers get a copy rather than access to mutable verified bytes.
type Facts struct {
	state *State
	epoch uint64
	proof Proof
	body  []byte
}

func (f *Facts) Body() []byte {
	if f == nil {
		return nil
	}
	return append([]byte(nil), f.body...)
}

// Admit verifies the MAC before locking or allocating replay state. The HTTP
// adapter is responsible for verify-once request context ownership and framing.
func (s *State) Admit(method, path string, p Proof, body []byte) (*Facts, error) {
	if len(body) > MaxRequest {
		return nil, ErrInvalid
	}
	owned := append([]byte(nil), body...)
	want, err := RequestMAC(s.key, s.binding, method, path, p.Timestamp, p.Nonce, owned)
	if err != nil || !EqualMAC(want, p.MAC) {
		return nil, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now, ok := s.nowLocked()
	if !ok || p.Timestamp <= now-60000 || p.Timestamp > math.MaxInt64-60000 {
		return nil, ErrInvalid
	}
	if p.Timestamp > now && p.Timestamp-now > 5000 {
		return nil, ErrInvalid
	}
	for nonce, expires := range s.nonces {
		if expires <= now {
			delete(s.nonces, nonce)
		}
	}
	if _, exists := s.nonces[p.Nonce]; exists || len(s.nonces) >= NonceLimit {
		return nil, ErrInvalid
	}
	s.nonces[p.Nonce] = p.Timestamp + 60000
	return &Facts{state: s, epoch: s.epoch, proof: p, body: owned}, nil
}

func (s *State) authorized(f *Facts) bool {
	return f != nil && f.state == s && s.active && f.epoch == s.epoch
}
func fullEntropy(random io.Reader) ([32]byte, error) {
	var b [32]byte
	_, err := io.ReadFull(random, b[:])
	if err != nil {
		return [32]byte{}, ErrInvalid
	}
	return b, nil
}
func liveCodes(codes map[[32]byte]pending, now int64) int {
	n := 0
	for _, p := range codes {
		if p.expires > now {
			n++
		}
	}
	return n
}

func (s *State) Issue(f *Facts) (string, error) {
	s.mu.Lock()
	now, ok := s.nowLocked()
	valid := ok && s.authorized(f)
	full := liveCodes(s.codes, now) >= PendingLimit
	s.mu.Unlock()
	if !valid {
		return "", ErrInvalid
	}
	if full {
		return "", ErrCapacity
	}
	for attempt := 0; attempt < 3; attempt++ {
		code, err := fullEntropy(s.random)
		if err != nil {
			return "", err
		}
		hash := sha256.Sum256(code[:])
		s.mu.Lock()
		now, ok = s.nowLocked()
		if !ok || !s.authorized(f) || now > math.MaxInt64-codeLife {
			s.mu.Unlock()
			return "", ErrInvalid
		}
		if old, exists := s.codes[hash]; exists && old.expires > now {
			s.mu.Unlock()
			continue
		}
		if liveCodes(s.codes, now) >= PendingLimit {
			s.mu.Unlock()
			return "", ErrCapacity
		}
		identity, ok := s.nextLocked()
		if !ok {
			s.mu.Unlock()
			return "", ErrInvalid
		}
		for id, p := range s.codes {
			if p.expires <= now {
				delete(s.codes, id)
			}
		}
		s.codes[hash] = pending{expires: now + codeLife, epoch: s.epoch, identity: identity}
		s.mu.Unlock()
		return Encode32(code), nil
	}
	return "", ErrInvalid
}

func expired(c credential, now int64) bool {
	return now-c.born >= absoluteLife || now-c.used >= idleLife
}
func (s *State) nextLocked() (uint64, bool) {
	if s.sequence == math.MaxUint64 {
		s.retireLocked()
		return 0, false
	}
	s.sequence++
	return s.sequence, true
}

func (s *State) Exchange(value string) (string, error) {
	code, err := Decode32(value)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(code[:])
	s.mu.Lock()
	now, ok := s.nowLocked()
	record, exists := s.codes[hash]
	valid := ok && exists && record.expires > now && record.epoch == s.epoch
	s.mu.Unlock()
	if !valid {
		return "", ErrInvalid
	}
	for attempt := 0; attempt < 3; attempt++ {
		bearer, err := fullEntropy(s.random)
		if err != nil {
			return "", err
		}
		id := sha256.Sum256(bearer[:])
		s.mu.Lock()
		now, ok = s.nowLocked()
		current, exists := s.codes[hash]
		if !ok || !exists || current != record || record.epoch != s.epoch || record.expires <= now {
			s.mu.Unlock()
			return "", ErrInvalid
		}
		if old, exists := s.credentials[id]; exists && !expired(old, now) {
			s.mu.Unlock()
			continue
		}
		sequence, ok := s.nextLocked()
		if !ok {
			s.mu.Unlock()
			return "", ErrInvalid
		}
		// All predicates and entropy succeeded before consuming, reclaiming or evicting.
		for id, c := range s.credentials {
			if expired(c, now) {
				delete(s.credentials, id)
			}
		}
		if len(s.credentials) >= CredentialLimit {
			var oldest [32]byte
			least := uint64(math.MaxUint64)
			for id, c := range s.credentials {
				if c.sequence < least {
					oldest = id
					least = c.sequence
				}
			}
			delete(s.credentials, oldest)
		}
		delete(s.codes, hash)
		s.credentials[id] = credential{born: now, used: now, sequence: sequence}
		s.mu.Unlock()
		return Encode32(bearer), nil
	}
	return "", ErrInvalid
}

// Credential validates before refreshing activity. Polls count as activity under
// the chosen policy, but cannot extend the eight-hour absolute lifetime.
func (s *State) Credential(value string) bool {
	decoded, err := Decode32(value)
	if err != nil {
		return false
	}
	hash := sha256.Sum256(decoded[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	now, ok := s.nowLocked()
	if !ok {
		return false
	}
	c, exists := s.credentials[hash]
	if !exists || expired(c, now) {
		return false
	}
	sequence, ok := s.nextLocked()
	if !ok {
		return false
	}
	c.used = now
	c.sequence = sequence
	s.credentials[hash] = c
	return true
}

func (s *State) Revoke(f *Facts) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.nowLocked()
	if !ok || !s.authorized(f) {
		return ErrInvalid
	}
	if s.epoch == math.MaxUint64 {
		s.retireLocked()
		return ErrInvalid
	}
	s.epoch++
	clear(s.codes)
	clear(s.credentials)
	return nil
}

// SignResponse accepts facts admitted before revoke/close: a successful revoke
// must still have an authenticated response, even after its epoch was advanced.
func (s *State) SignResponse(f *Facts, status int, body []byte) ([32]byte, error) {
	if f == nil || f.state != s {
		return [32]byte{}, ErrInvalid
	}
	return ResponseMAC(s.key, f.proof, status, body)
}

func (s *State) SignHello(challenge [32]byte, status int, body []byte) ([32]byte, error) {
	return HelloMAC(s.key, s.binding, challenge, status, body)
}
