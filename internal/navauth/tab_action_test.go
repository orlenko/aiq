package navauth

import (
	"crypto/sha256"
	"strings"
	"testing"
)

func actionProof(t *testing.T, s *State, token, target string, nonce byte) ActionTabProof {
	t.Helper()
	raw, _ := Decode32(token)
	key := sha256.Sum256(raw[:])
	p := ActionTabProof{TabProof{ID: TabID(key), Nonce: [32]byte{nonce}}}
	var err error
	p.Proof.MAC, err = TabActionRequestMAC(key, binding, "POST", target, p)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func TestActionTabScopeReplayAndRetirement(t *testing.T) {
	s, _ := setup(t)
	token := credentialFor(t, s, admit(t, s))
	target := "/api/nav/asks/action?action=dismiss&id=1&revision=" + strings.Repeat("a", 64)
	p := actionProof(t, s, token, target, 3)
	parsed, err := ParseActionTabProof(p.String())
	if err != nil || parsed != p {
		t.Fatal("action roundtrip", err)
	}
	if _, err = s.AdmitTab("GET", target, p.Proof); err == nil {
		t.Fatal("action admitted as read")
	}
	for _, change := range []struct{ method, target string }{{"GET", target}, {"POST", strings.Replace(target, "dismiss", "restore", 1)}, {"POST", strings.Replace(target, "id=1", "id=2", 1)}, {"POST", target + "&extra=1"}} {
		if _, err = s.AdmitActionTab(change.method, change.target, p); err == nil {
			t.Fatal("changed action admitted")
		}
	}
	facts, err := s.AdmitActionTab("POST", target, p)
	if err != nil {
		t.Fatal("action refused", err)
	}
	if _, err = s.AdmitActionTab("POST", target, p); err == nil {
		t.Fatal("replay admitted")
	}
	if _, err = facts.SignResponse(200, []byte(`{}`)); err == nil {
		t.Fatal("action response in read domain")
	}
	if _, err = facts.SignActionResponse(200, []byte(`{}`)); err != nil {
		t.Fatal("live action response", err)
	}
	s.Close()
	if _, err = facts.SignActionResponse(200, []byte(`{}`)); err == nil {
		t.Fatal("retired action signed")
	}
}
func TestActionTabCapacityAndMalformedTargets(t *testing.T) {
	s, _ := setup(t)
	token := credentialFor(t, s, admit(t, s))
	target := "/api/nav/asks/action?action=dismiss&id=1&revision=" + strings.Repeat("a", 64)
	for _, bad := range []string{strings.Replace(target, "id=1", "id=0", 1), strings.Replace(target, "id=1", "id=01", 1), strings.Replace(target, "id=1", "id=9223372036854775808", 1), strings.Replace(target, "dismiss", "delete", 1), target + "#x", strings.Replace(target, "?action=dismiss&id=1", "?id=1&action=dismiss", 1)} {
		if ActionTargetValid(bad) {
			t.Fatal("noncanonical target", bad)
		}
	}
	s.actionNonces = make(map[[32]byte]int64)
	for i := 0; i < NonceLimit; i++ {
		var n [32]byte
		n[0] = byte(i)
		n[1] = byte(i >> 8)
		s.actionNonces[n] = s.wall + absoluteLife
	}
	if _, err := s.AdmitActionTab("POST", target, actionProof(t, s, token, target, 3)); err == nil || len(s.actionNonces) != NonceLimit {
		t.Fatal("capacity accepted")
	}
	for n := range s.actionNonces {
		s.actionNonces[n] = s.wall
	}
	if _, err := s.AdmitActionTab("POST", target, actionProof(t, s, token, target, 3)); err != nil || len(s.actionNonces) != 1 {
		t.Fatal("expired capacity not reclaimed", err)
	}
}
