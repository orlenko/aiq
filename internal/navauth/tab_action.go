package navauth

import (
	"crypto/sha256"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// ActionTabProof is a separate capability domain from GET proofs. Its complete
// signed target holds the action and expected revision; the request has no body.
type ActionTabProof struct{ Proof TabProof }

var actionTarget = regexp.MustCompile(`^/api/nav/asks/action\?action=(dismiss|restore)&id=([1-9][0-9]{0,18})&revision=[0-9a-f]{64}$`)

func ActionTargetValid(target string) bool {
	if len(target) > 180 {
		return false
	}
	parts := actionTarget.FindStringSubmatch(target)
	if len(parts) != 3 {
		return false
	}
	_, err := strconv.ParseInt(parts[2], 10, 64)
	return err == nil
}
func (p ActionTabProof) String() string {
	return "AiqAction1 " + strings.TrimPrefix(p.Proof.String(), "AiqTab ")
}
func ParseActionTabProof(value string) (ActionTabProof, error) {
	if !strings.HasPrefix(value, "AiqAction1 ") {
		return ActionTabProof{}, ErrInvalid
	}
	p, err := ParseTabProof("AiqTab " + strings.TrimPrefix(value, "AiqAction1 "))
	return ActionTabProof{p}, err
}
func TabActionRequestMAC(key [32]byte, b Binding, method, target string, p ActionTabProof) ([32]byte, error) {
	if !b.valid() || method != "POST" || !ActionTargetValid(target) {
		return [32]byte{}, ErrInvalid
	}
	return digest(key, tabFrame("aiq-nav-tab-action-request-v1", "http://127.0.0.1:"+strconv.Itoa(b.Port), b.Build, method, target, Encode32(p.Proof.ID), Encode32(p.Proof.Nonce))), nil
}
func TabActionResponseMAC(key [32]byte, b Binding, p ActionTabProof, status int, body []byte) ([32]byte, error) {
	if !b.valid() || status < 200 || status > 599 || len(body) > 1024 {
		return [32]byte{}, ErrInvalid
	}
	sum := sha256.Sum256(body)
	return digest(key, tabFrame("aiq-nav-tab-action-response-v1", Encode32(p.Proof.MAC), strconv.Itoa(status), "application/json", b.Build, Encode32(sum))), nil
}
func (s *State) AdmitActionTab(method, target string, p ActionTabProof) (*TabFacts, error) {
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
		if EqualMAC(TabID(candidate), p.Proof.ID) && !expired(c, now) {
			key, selected = candidate, c
			matches++
		}
	}
	if matches != 1 {
		return nil, ErrInvalid
	}
	want, err := TabActionRequestMAC(key, s.binding, method, target, p)
	if err != nil || !EqualMAC(want, p.Proof.MAC) {
		return nil, ErrInvalid
	}
	if s.actionNonces == nil {
		s.actionNonces = make(map[[32]byte]int64)
	}
	for nonce, expires := range s.actionNonces {
		if expires <= now {
			delete(s.actionNonces, nonce)
		}
	}
	// Nonces remain consumed for this credential's whole possible lifetime.
	// The semantic revision is an additional transaction-level replay fence.
	nonceID := sha256.Sum256(tabFrame(Encode32(p.Proof.ID), Encode32(p.Proof.Nonce)))
	if _, exists := s.actionNonces[nonceID]; exists || len(s.actionNonces) >= NonceLimit {
		return nil, ErrInvalid
	}
	sequence, ok := s.nextLocked()
	if !ok {
		return nil, ErrInvalid
	}
	expires := int64(math.MaxInt64)
	if selected.born <= math.MaxInt64-absoluteLife {
		expires = selected.born + absoluteLife
	}
	s.actionNonces[nonceID] = expires
	selected.used, selected.sequence = now, sequence
	s.credentials[key] = selected
	return &TabFacts{state: s, key: key, proof: p.Proof, epoch: s.epoch, born: selected.born, action: true}, nil
}
func (f *TabFacts) Action() bool { return f != nil && f.action }
func (f *TabFacts) SignActionResponse(status int, body []byte) ([32]byte, error) {
	if f == nil || f.state == nil || !f.action {
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
	return TabActionResponseMAC(f.key, s.binding, ActionTabProof{f.proof}, status, body)
}
