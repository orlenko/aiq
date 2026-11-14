package navauth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestTabLiteralVectors(t *testing.T) {
	var v struct {
		Credential, Nonce, Build, Target, Body, Authorization, Response string
		Port                                                            int
		KeyHex                                                          string `json:"key_hex"`
		RequestFrame                                                    string `json:"request_frame_hex"`
	}
	raw, err := os.ReadFile("testdata/tab_vectors.json")
	if err != nil || json.Unmarshal(raw, &v) != nil {
		t.Fatal("literal vector")
	}
	token, _ := Decode32(v.Credential)
	key := sha256.Sum256(token[:])
	if hex.EncodeToString(key[:]) != v.KeyHex {
		t.Fatal("key derivation")
	}
	nonce, _ := Decode32(v.Nonce)
	p := TabProof{ID: TabID(key), Nonce: nonce}
	b := Binding{v.Port, v.Build}
	p.MAC, err = TabRequestMAC(key, b, "GET", v.Target, p)
	if err != nil || p.String() != v.Authorization {
		t.Fatal("literal browser request proof")
	}
	if hex.EncodeToString(tabFrame("aiq-nav-tab-request-v1", "http://127.0.0.1:54321", v.Build, "GET", v.Target, Encode32(p.ID), Encode32(p.Nonce))) != v.RequestFrame {
		t.Fatal("literal UTF8 frame")
	}
	parsed, err := ParseTabProof(v.Authorization)
	if err != nil || parsed != p {
		t.Fatal("proof parse")
	}
	mac, err := TabResponseMAC(key, b, p, 200, []byte(v.Body))
	if err != nil || Encode32(mac) != v.Response {
		t.Fatal("literal response proof")
	}
	for _, h := range []string{"", "Bearer " + v.Credential, v.Authorization + " ", strings.Replace(v.Authorization, "AiqTab ", "aiqtab ", 1), v.Authorization + ".x", v.Authorization[:len(v.Authorization)-1] + "="} {
		if _, err := ParseTabProof(h); err == nil {
			t.Fatal("noncanonical browser proof")
		}
	}
	for _, change := range []struct {
		b              Binding
		method, target string
		p              TabProof
	}{{Binding{54322, v.Build}, "GET", v.Target, p}, {Binding{54321, "other"}, "GET", v.Target, p}, {b, "GET", v.Target + "x", p}, {b, "GET", v.Target, TabProof{ID: p.ID, Nonce: [32]byte{1}}}} {
		m, e := TabRequestMAC(key, change.b, change.method, change.target, change.p)
		if e != nil || EqualMAC(m, p.MAC) {
			t.Fatal("changed request field")
		}
	}
	if _, err := TabRequestMAC(key, b, "POST", v.Target, p); err == nil {
		t.Fatal("read proof authorized a mutation")
	}
	for _, c := range []struct {
		status int
		body   string
	}{{201, v.Body}, {200, v.Body + " "}} {
		m, e := TabResponseMAC(key, b, p, c.status, []byte(c.body))
		if e != nil || EqualMAC(m, mac) {
			t.Fatal("changed response field")
		}
	}
}
func TestTabAdmissionAndRetirement(t *testing.T) {
	for _, mode := range []string{"revoke", "close", "idle", "absolute", "evict"} {
		t.Run(mode, func(t *testing.T) {
			s, clock := setup(t)
			f := admit(t, s)
			token := credentialFor(t, s, f)
			raw, _ := Decode32(token)
			key := sha256.Sum256(raw[:])
			p := TabProof{ID: TabID(key), Nonce: [32]byte{1}}
			p.MAC, _ = TabRequestMAC(key, binding, "GET", "/api/nav/sessions", p)
			bad := p
			bad.MAC[0] ^= 1
			if _, e := s.AdmitTab("GET", "/api/nav/sessions", bad); e == nil {
				t.Fatal("bad signature admitted")
			}
			admitted, e := s.AdmitTab("GET", "/api/nav/sessions", p)
			if e != nil {
				t.Fatal("valid signature refused")
			}
			if _, e = admitted.SignResponse(200, []byte(`{}`)); e != nil {
				t.Fatal("live response refused")
			}
			switch mode {
			case "revoke":
				if s.Revoke(f) != nil {
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
			if _, e := admitted.SignResponse(200, []byte(`{"invented":true}`)); e == nil {
				t.Fatal("retired response signed")
			}
			if _, e := s.AdmitTab("GET", "/api/nav/sessions", p); e == nil {
				t.Fatal("retired read admitted")
			}
		})
	}
}
func TestTabProofDoesNotAuthorizeOtherScopesOrGenerations(t *testing.T) {
	s, _ := setup(t)
	token := credentialFor(t, s, admit(t, s))
	raw, _ := Decode32(token)
	key := sha256.Sum256(raw[:])
	p := TabProof{ID: TabID(key), Nonce: [32]byte{2}}
	p.MAC, _ = TabRequestMAC(key, binding, "GET", "/api/nav/sessions?dir=%2Fa", p)
	for _, c := range []struct{ method, target string }{{"POST", "/api/nav/sessions?dir=%2Fa"}, {"GET", "/api/nav/sessions?dir=%2Fb"}, {"GET", "/api/nav/asks"}} {
		if _, e := s.AdmitTab(c.method, c.target, p); e == nil {
			t.Fatal("proof changed scope")
		}
	}
	next, _ := setup(t)
	if _, e := next.AdmitTab("GET", "/api/nav/sessions?dir=%2Fa", p); e == nil {
		t.Fatal("replacement generation accepted old read")
	}
}

func FuzzTabProofCanonical(f *testing.F) {
	raw, err := os.ReadFile("testdata/tab_vectors.json")
	if err != nil {
		f.Fatal(err)
	}
	var v struct{ Authorization string }
	if json.Unmarshal(raw, &v) != nil {
		f.Fatal("invented vector")
	}
	for _, s := range []string{"", v.Authorization, v.Authorization + " ", strings.ToLower(v.Authorization), "Bearer invented", string([]byte{255, 0}), strings.Repeat("x", 1000)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, value string) {
		p, err := ParseTabProof(value)
		if err != nil {
			if err != ErrInvalid {
				t.Fatal("input-dependent error")
			}
			return
		}
		if len(value) != 138 || p.String() != value {
			t.Fatal("accepted noncanonical proof")
		}
	})
}
