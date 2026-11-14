package navauth

import (
	"crypto/sha256"
	"strconv"
	"strings"
	"unicode/utf8"
)

// TabProof is a read-only browser request proof. None of its fields is a key.
// The credential digest is an effective signing key and never crosses the wire.
type TabProof struct{ ID, Nonce, MAC [32]byte }

func (p TabProof) String() string {
	return "AiqTab " + Encode32(p.ID) + "." + Encode32(p.Nonce) + "." + Encode32(p.MAC)
}
func ParseTabProof(value string) (TabProof, error) {
	var p TabProof
	if len(value) != 7+3*43+2 || !strings.HasPrefix(value, "AiqTab ") {
		return p, ErrInvalid
	}
	parts := strings.Split(value[7:], ".")
	if len(parts) != 3 {
		return p, ErrInvalid
	}
	var err error
	if p.ID, err = Decode32(parts[0]); err != nil {
		return TabProof{}, err
	}
	if p.Nonce, err = Decode32(parts[1]); err != nil {
		return TabProof{}, err
	}
	if p.MAC, err = Decode32(parts[2]); err != nil {
		return TabProof{}, err
	}
	return p, nil
}
func tabFrame(fields ...string) []byte {
	var out []byte
	for _, s := range fields {
		out = strconv.AppendInt(out, int64(len(s)), 10)
		out = append(out, ':')
		out = append(out, s...)
	}
	return out
}
func TabID(key [32]byte) [32]byte { return digest(key, []byte("aiq-nav-tab-id-v1")) }
func TabRequestMAC(key [32]byte, b Binding, method, target string, p TabProof) ([32]byte, error) {
	if !b.valid() || method != "GET" || !strings.HasPrefix(target, "/api/nav/") || len(target) > 16384 || !utf8.ValidString(target) || strings.ContainsAny(target, "\r\n\x00#") {
		return [32]byte{}, ErrInvalid
	}
	return digest(key, tabFrame("aiq-nav-tab-request-v1", "http://127.0.0.1:"+strconv.Itoa(b.Port), b.Build, method, target, Encode32(p.ID), Encode32(p.Nonce))), nil
}
func TabResponseMAC(key [32]byte, b Binding, p TabProof, status int, body []byte) ([32]byte, error) {
	if !b.valid() || status < 200 || status > 599 || len(body) > 16<<20 {
		return [32]byte{}, ErrInvalid
	}
	hash := sha256.Sum256(body)
	return digest(key, tabFrame("aiq-nav-tab-response-v1", Encode32(p.MAC), strconv.Itoa(status), "application/json", b.Build, Encode32(hash))), nil
}

// TabFacts belongs to the admitting generation and one credential incarnation.
// It exposes no key, identifier or credential; only a complete response signer.
type TabFacts struct {
	state       *State
	key         [32]byte
	proof       TabProof
	epoch       uint64
	born        int64
	conditional bool
}

func (s *State) AdmitTab(method, target string, p TabProof) (*TabFacts, error) {
	return s.admitTab(method, target, "", p, false)
}
func (s *State) admitTab(method, target, condition string, p TabProof, conditional bool) (*TabFacts, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now, ok := s.nowLocked()
	if !ok {
		return nil, ErrInvalid
	}
	var key [32]byte
	var selected credential
	matches := 0
	for candidate, c := range s.credentials {
		id := TabID(candidate)
		if EqualMAC(id, p.ID) && !expired(c, now) {
			key = candidate
			selected = c
			matches++
		}
	}
	if matches != 1 {
		return nil, ErrInvalid
	}
	var mac [32]byte
	var err error
	if conditional {
		mac, err = TabConditionalRequestMAC(key, s.binding, method, target, condition, ConditionalTabProof{p})
	} else {
		mac, err = TabRequestMAC(key, s.binding, method, target, p)
	}
	if err != nil || !EqualMAC(mac, p.MAC) {
		return nil, ErrInvalid
	}
	sequence, ok := s.nextLocked()
	if !ok {
		return nil, ErrInvalid
	}
	selected.used = now
	selected.sequence = sequence
	s.credentials[key] = selected
	return &TabFacts{state: s, key: key, proof: p, epoch: s.epoch, born: selected.born, conditional: conditional}, nil
}
func (f *TabFacts) SignResponse(status int, body []byte) ([32]byte, error) {
	if f == nil || f.state == nil || f.conditional {
		return [32]byte{}, ErrInvalid
	}
	s := f.state
	s.mu.Lock()
	defer s.mu.Unlock()
	now, ok := s.nowLocked()
	c, exists := s.credentials[f.key]
	if !ok || !exists || f.epoch != s.epoch || c.born != f.born || expired(c, now) {
		return [32]byte{}, ErrInvalid
	}
	return TabResponseMAC(f.key, s.binding, f.proof, status, body)
}
