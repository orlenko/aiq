package navauth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestConditionalTabIndependentLiteralVectors(t *testing.T) {
	// Explicit tags below keep the public fixture's framing bytes independent.
	raw, err := os.ReadFile("testdata/tab_conditional_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Credential, Nonce, Build, Target string
		Port                             int
		KeyHex                           string `json:"key_hex"`
		Cases                            []struct {
			Condition, Body, ETag, Authorization, Response string
			Status                                         int
			RequestFrameHex                                string `json:"request_frame_hex"`
			ResponseFrameHex                               string `json:"response_frame_hex"`
		}
	}
	if json.Unmarshal(raw, &v) != nil || len(v.Cases) != 3 {
		t.Fatal("incomplete independent vectors")
	}
	token, _ := Decode32(v.Credential)
	key := sha256.Sum256(token[:])
	if hex.EncodeToString(key[:]) != v.KeyHex {
		t.Fatal("literal key derivation")
	}
	nonce, _ := Decode32(v.Nonce)
	b := Binding{v.Port, v.Build}
	for _, c := range v.Cases {
		p := ConditionalTabProof{TabProof{ID: TabID(key), Nonce: nonce}}
		p.Proof.MAC, err = TabConditionalRequestMAC(key, b, "GET", v.Target, c.Condition, p)
		if err != nil || p.String() != c.Authorization {
			t.Fatal("literal request mismatch")
		}
		parsed, e := ParseConditionalTabProof(c.Authorization)
		if e != nil || parsed != p {
			t.Fatal("canonical v2 parse")
		}
		if _, e := ParseTabProof(c.Authorization); e == nil {
			t.Fatal("v2 admitted as v1 scheme")
		}
		if got := hex.EncodeToString(tabFrame("aiq-nav-tab-request-v2", "http://127.0.0.1:54321", v.Build, "GET", v.Target, Encode32(p.Proof.ID), Encode32(p.Proof.Nonce), c.Condition)); got != c.RequestFrameHex {
			t.Fatal("request UTF8 frame mismatch")
		}
		mac, e := TabConditionalResponseMAC(key, b, p, c.Status, c.ETag, []byte(c.Body))
		if e != nil || Encode32(mac) != c.Response {
			t.Fatal("literal response mismatch")
		}
		changed, e := TabConditionalRequestMAC(key, b, "GET", v.Target, c.Condition+" ", p)
		if e != nil || EqualMAC(changed, p.Proof.MAC) {
			t.Fatal("condition not bound")
		}
		changed, e = TabConditionalResponseMAC(key, b, p, c.Status, "\""+strings.Repeat("b", 64)+"\"", []byte(c.Body))
		if e != nil || EqualMAC(changed, mac) {
			t.Fatal("response ETag not bound")
		}
		legacy, _ := TabResponseMAC(key, b, p.Proof, c.Status, []byte(c.Body))
		if EqualMAC(legacy, mac) {
			t.Fatal("response cross-version collision")
		}
	}
	for _, condition := range []string{strings.Repeat("x", 8193), "\r", "\n", "\x00", string([]byte{255})} {
		if _, e := TabConditionalRequestMAC(key, b, "GET", v.Target, condition, ConditionalTabProof{}); e != ErrInvalid {
			t.Fatal("invalid condition framing")
		}
	}
	for _, tag := range []string{"x", "W/\"" + strings.Repeat("a", 64) + "\"", "\"" + strings.Repeat("A", 64) + "\"", strings.Repeat("a", 64)} {
		if _, e := TabConditionalResponseMAC(key, b, ConditionalTabProof{}, 200, tag, nil); e != ErrInvalid {
			t.Fatal("noncanonical response tag")
		}
	}
	if _, e := TabConditionalResponseMAC(key, b, ConditionalTabProof{}, 304, "", nil); e != ErrInvalid {
		t.Fatal("tagless304")
	}
	if _, e := TabConditionalResponseMAC(key, b, ConditionalTabProof{}, 304, "\""+strings.Repeat("a", 64)+"\"", []byte("x")); e != ErrInvalid {
		t.Fatal("bodyful304")
	}
}

func TestConditionalTabAdmissionVersionAndRetirement(t *testing.T) {
	for _, mode := range []string{"revoke", "close", "idle", "absolute", "evict"} {
		t.Run(mode, func(t *testing.T) {
			s, clock := setup(t)
			issuer := admit(t, s)
			token := credentialFor(t, s, issuer)
			raw, _ := Decode32(token)
			key := sha256.Sum256(raw[:])
			target := "/api/nav/sessions?dir=%2Finvented"
			condition := "\"" + strings.Repeat("a", 64) + "\""
			p := ConditionalTabProof{TabProof{ID: TabID(key), Nonce: [32]byte{52}}}
			p.Proof.MAC, _ = TabConditionalRequestMAC(key, binding, "GET", target, condition, p)
			for _, bad := range []struct{ method, target, condition string }{{"POST", target, condition}, {"GET", target + "x", condition}, {"GET", target, ""}} {
				if _, err := s.AdmitConditionalTab(bad.method, bad.target, bad.condition, p); err == nil {
					t.Fatal("changed request admitted")
				}
			}
			if _, err := s.AdmitTab("GET", target, p.Proof); err == nil {
				t.Fatal("v2 MAC admitted as v1")
			}
			facts, err := s.AdmitConditionalTab("GET", target, condition, p)
			if err != nil || !facts.Conditional() {
				t.Fatal("valid conditional proof refused")
			}
			if _, err := facts.SignResponse(200, []byte(`{}`)); err == nil {
				t.Fatal("conditional facts used legacy signer")
			}
			if _, err := facts.SignConditionalResponse(304, condition, nil); err != nil {
				t.Fatal("live conditional304 refused")
			}
			legacy := p.Proof
			legacy.MAC, _ = TabRequestMAC(key, binding, "GET", target, legacy)
			old, err := s.AdmitTab("GET", target, legacy)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := old.SignConditionalResponse(200, "", []byte(`{}`)); err == nil {
				t.Fatal("legacy facts used conditional signer")
			}
			switch mode {
			case "revoke":
				if s.Revoke(issuer) != nil {
					t.Fatal("revoke")
				}
			case "close":
				s.Close()
			case "idle":
				clock.Store(idleLife)
			case "absolute":
				clock.Store(absoluteLife)
			case "evict":
				s.mu.Lock()
				delete(s.credentials, key)
				s.mu.Unlock()
			}
			if _, err := facts.SignConditionalResponse(304, condition, nil); err == nil {
				t.Fatal("retired conditional response signed")
			}
			if _, err := s.AdmitConditionalTab("GET", target, condition, p); err == nil {
				t.Fatal("retired conditional request admitted")
			}
		})
	}
}

func FuzzConditionalTabProofCanonical(f *testing.F) {
	for _, seed := range []string{"", "AiqTab2 " + strings.Repeat("A", 43) + "." + strings.Repeat("A", 43) + "." + strings.Repeat("A", 43), "AiqTab " + strings.Repeat("x", 131), "\xff\x00"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		p, err := ParseConditionalTabProof(s)
		if err != nil {
			if err != ErrInvalid {
				t.Fatal("input-dependent error")
			}
			return
		}
		if p.String() != s {
			t.Fatal("noncanonical proof round trip")
		}
		if _, err := ParseTabProof(s); err == nil {
			t.Fatal("cross-version scheme")
		}
	})
}
