// Package navauth implements the navigator's private, per-listener authentication
// protocol. It performs no file, network, logging or browser operations.
package navauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	Protocol    = 1
	MaxRequest  = 4096
	MaxResponse = 65536
)

// Errors deliberately contain no presented credentials, request bodies or paths.
var ErrInvalid = errors.New("navigator authentication refused")

// Binding identifies one listener and build; the restart key is separate.
type Binding struct {
	Port  int
	Build string
}

func (b Binding) valid() bool {
	return b.Port > 0 && b.Port <= 65535 && len(b.Build) <= 256 && utf8.ValidString(b.Build)
}

// Encode32 returns the sole accepted spelling of a key, nonce, code or MAC.
func Encode32(v [32]byte) string { return base64.RawURLEncoding.EncodeToString(v[:]) }

func Decode32(s string) ([32]byte, error) {
	var value [32]byte
	if len(s) != 43 {
		return value, ErrInvalid
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil || len(b) != len(value) {
		return value, ErrInvalid
	}
	copy(value[:], b)
	if Encode32(value) != s {
		return [32]byte{}, ErrInvalid
	}
	return value, nil
}

// Proof is the decoded value of exactly one X-Aiq-Proof header. The caller must
// reject duplicate header fields before parsing it.
type Proof struct {
	Timestamp int64
	Nonce     [32]byte
	MAC       [32]byte
}

func (p Proof) String() string {
	return "v1 " + strconv.FormatInt(p.Timestamp, 10) + " " + Encode32(p.Nonce) + " " + Encode32(p.MAC)
}

func ParseProof(header string) (Proof, error) {
	var p Proof
	if len(header) > 112 {
		return p, ErrInvalid
	}
	parts := strings.Split(header, " ")
	if len(parts) != 4 || parts[0] != "v1" {
		return p, ErrInvalid
	}
	t, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || t < 0 || strconv.FormatInt(t, 10) != parts[1] {
		return p, ErrInvalid
	}
	nonce, err := Decode32(parts[2])
	if err != nil {
		return p, err
	}
	mac, err := Decode32(parts[3])
	if err != nil {
		return p, err
	}
	return Proof{Timestamp: t, Nonce: nonce, MAC: mac}, nil
}

// frame implements aiq-nav-auth/1. Binary fields stay raw, and every field has a
// byte length; neither delimiter ambiguity nor base64 spelling enters the MAC.
func frame(tag string, fields ...[]byte) []byte {
	b := append([]byte("aiq-nav-auth/1"), 0)
	b = binary.BigEndian.AppendUint32(b, uint32(1+len(fields)))
	b = binary.BigEndian.AppendUint32(b, uint32(len(tag)))
	b = append(b, tag...)
	for _, field := range fields {
		b = binary.BigEndian.AppendUint32(b, uint32(len(field)))
		b = append(b, field...)
	}
	return b
}

func digest(key [32]byte, message []byte) [32]byte {
	m := hmac.New(sha256.New, key[:])
	_, _ = m.Write(message)
	var result [32]byte
	copy(result[:], m.Sum(nil))
	return result
}

func supported(method, path string) bool {
	return method == "GET" && path == "/api/nav/info" ||
		method == "POST" && (path == "/api/nav/login" || path == "/api/nav/revoke")
}

// RequestMAC authenticates exact owned request bytes. Route framing/body rules
// (including content type, no queries and closed JSON) are the HTTP adapter's duty.
func RequestMAC(key [32]byte, binding Binding, method, path string, timestamp int64, nonce [32]byte, body []byte) ([32]byte, error) {
	if !binding.valid() || !supported(method, path) || timestamp < 0 || len(body) > MaxRequest {
		return [32]byte{}, ErrInvalid
	}
	hash := sha256.Sum256(body)
	return digest(key, frame("request", []byte("1"), []byte(method), []byte(path),
		[]byte(strconv.Itoa(binding.Port)), []byte(binding.Build), []byte(strconv.FormatInt(timestamp, 10)), nonce[:], hash[:])), nil
}

func validReply(status int, body []byte) bool {
	return status >= 100 && status <= 599 && len(body) <= MaxResponse
}

func HelloMAC(key [32]byte, binding Binding, challenge [32]byte, status int, body []byte) ([32]byte, error) {
	if !binding.valid() || !validReply(status, body) {
		return [32]byte{}, ErrInvalid
	}
	hash := sha256.Sum256(body)
	return digest(key, frame("hello", challenge[:], []byte("1"), []byte(binding.Build),
		[]byte(strconv.Itoa(binding.Port)), []byte(strconv.Itoa(status)), hash[:])), nil
}

func ResponseMAC(key [32]byte, request Proof, status int, body []byte) ([32]byte, error) {
	if !validReply(status, body) {
		return [32]byte{}, ErrInvalid
	}
	hash := sha256.Sum256(body)
	return digest(key, frame("response", request.Nonce[:], request.MAC[:], []byte(strconv.Itoa(status)), hash[:])), nil
}

// EqualMAC performs constant-time comparison of fixed-size decoded MAC values.
func EqualMAC(a, b [32]byte) bool { return hmac.Equal(a[:], b[:]) }
