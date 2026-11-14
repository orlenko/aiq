package navauth

import (
	"crypto/sha256"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ConditionalTabProof has an explicit wire version; it cannot be admitted as a
// v1 read by changing the scheme name. No credential or signing key is exposed.
type ConditionalTabProof struct{ Proof TabProof }

func (p ConditionalTabProof) String() string {
	return "AiqTab2 " + strings.TrimPrefix(p.Proof.String(), "AiqTab ")
}
func ParseConditionalTabProof(value string) (ConditionalTabProof, error) {
	if !strings.HasPrefix(value, "AiqTab2 ") {
		return ConditionalTabProof{}, ErrInvalid
	}
	p, err := ParseTabProof("AiqTab " + strings.TrimPrefix(value, "AiqTab2 "))
	if err != nil {
		return ConditionalTabProof{}, err
	}
	return ConditionalTabProof{p}, nil
}
func TabConditionalRequestMAC(key [32]byte, b Binding, method, target, condition string, p ConditionalTabProof) ([32]byte, error) {
	if !b.valid() || method != "GET" || !strings.HasPrefix(target, "/api/nav/") || len(target) > 16384 || !utf8.ValidString(target) || strings.ContainsAny(target, "\r\n\x00#") || len(condition) > 8192 || !utf8.ValidString(condition) || strings.ContainsAny(condition, "\r\n\x00") {
		return [32]byte{}, ErrInvalid
	}
	return digest(key, tabFrame("aiq-nav-tab-request-v2", "http://127.0.0.1:"+strconv.Itoa(b.Port), b.Build, method, target, Encode32(p.Proof.ID), Encode32(p.Proof.Nonce), condition)), nil
}
func tabCanonicalETag(value string) bool {
	if len(value) != 66 || value[0] != '"' || value[65] != '"' {
		return false
	}
	for _, c := range value[1:65] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func TabConditionalResponseMAC(key [32]byte, b Binding, p ConditionalTabProof, status int, etag string, body []byte) ([32]byte, error) {
	if !b.valid() || status < 200 || status > 599 || len(body) > 16<<20 || etag != "" && !tabCanonicalETag(etag) || status == 304 && (len(body) != 0 || etag == "") {
		return [32]byte{}, ErrInvalid
	}
	mime := "application/json"
	if status == 304 {
		mime = ""
	}
	hash := sha256.Sum256(body)
	return digest(key, tabFrame("aiq-nav-tab-response-v2", Encode32(p.Proof.MAC), strconv.Itoa(status), mime, b.Build, etag, Encode32(hash))), nil
}
func (s *State) AdmitConditionalTab(method, target, condition string, p ConditionalTabProof) (*TabFacts, error) {
	return s.admitTab(method, target, condition, p.Proof, true)
}
func (f *TabFacts) Conditional() bool { return f != nil && f.conditional }
func (f *TabFacts) SignConditionalResponse(status int, etag string, body []byte) ([32]byte, error) {
	if f == nil || f.state == nil || !f.conditional {
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
	return TabConditionalResponseMAC(f.key, s.binding, ConditionalTabProof{f.proof}, status, etag, body)
}
