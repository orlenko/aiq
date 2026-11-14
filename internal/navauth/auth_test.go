package navauth

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/testenv"
)

func TestMain(m *testing.M) { testenv.Main(m) }

var binding = Binding{Port: 54321, Build: "0.0.0+fixture"}
var inventedKey = [32]byte{1, 2, 3}

func TestLiteralProtocolVectors(t *testing.T) {
	// Literal vectors independently calculated with Python struct/hashlib/hmac;
	// the test does not construct its expected frame with the production encoder.
	var key, nonce [32]byte
	for i := range key {
		key[i] = byte(i)
		nonce[i] = byte(i + 32)
	}
	body := []byte(`{"action":"issue"}`)
	hash := sha256.Sum256(body)
	wantFrame := "6169712d6e61762d617574682f3100000000090000000772657175657374000000013100000004504f53540000000e2f6170692f6e61762f6c6f67696e0000000535343332310000000d302e302e302b666978747572650000000d3137303030303030303030303000000020202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f00000020b74841c413e582346cd80a21e6879f3d508aedbb5fa328ca61bac8b3b8677fc1"
	got := frame("request", []byte("1"), []byte("POST"), []byte("/api/nav/login"), []byte("54321"), []byte(binding.Build), []byte("1700000000000"), nonce[:], hash[:])
	if hex.EncodeToString(got) != wantFrame {
		t.Fatal("literal request frame mismatch")
	}
	request, err := RequestMAC(key, binding, "POST", "/api/nav/login", 1700000000000, nonce, body)
	if err != nil || hex.EncodeToString(request[:]) != "3747db5d2b0e8b3407c2167b47bdfdc51dc9611c6fbdb407afd66adc21704a2d" {
		t.Fatal("literal request MAC mismatch", err)
	}
	hello, err := HelloMAC(key, binding, nonce, 200, []byte(`{"nav_version":1,"build":"0.0.0+fixture","server_time_ms":1700000000000}`))
	if err != nil || hex.EncodeToString(hello[:]) != "3e1e4b8909cf058a94c6ed81f6e59bc78f89c213f2bc4cc765090f1327434c3c" {
		t.Fatal("literal hello MAC mismatch", err)
	}
	reply, err := ResponseMAC(key, Proof{Nonce: nonce, MAC: request}, 200, []byte(`{"ok":true}`))
	if err != nil || hex.EncodeToString(reply[:]) != "a41a393c27ba88b0db1fcd2159b38f3a2ef2d7c48b5e7eceaa005111ce8c0b3f" {
		t.Fatal("literal response MAC mismatch", err)
	}
	for _, change := range []struct {
		b            Binding
		method, path string
		stamp        int64
		nonce        [32]byte
		body         []byte
	}{
		{Binding{54322, binding.Build}, "POST", "/api/nav/login", 1700000000000, nonce, body},
		{Binding{54321, "0.0.0+other"}, "POST", "/api/nav/login", 1700000000000, nonce, body},
		{binding, "POST", "/api/nav/revoke", 1700000000000, nonce, body},
		{binding, "POST", "/api/nav/login", 1700000000001, nonce, body},
		{binding, "POST", "/api/nav/login", 1700000000000, [32]byte{}, body},
		{binding, "POST", "/api/nav/login", 1700000000000, nonce, append(append([]byte(nil), body...), ' ')},
	} {
		mac, e := RequestMAC(key, change.b, change.method, change.path, change.stamp, change.nonce, change.body)
		if e != nil || EqualMAC(mac, request) {
			t.Fatal("changed field did not change MAC")
		}
	}
	statusReply, _ := ResponseMAC(key, Proof{Nonce: nonce, MAC: request}, 201, []byte(`{"ok":true}`))
	otherRequest, _ := ResponseMAC(key, Proof{Nonce: nonce, MAC: hello}, 200, []byte(`{"ok":true}`))
	if EqualMAC(reply, statusReply) || EqualMAC(reply, otherRequest) || EqualMAC(reply, request) || EqualMAC(hello, request) {
		t.Fatal("status, request or domain reflection")
	}
	if bytes.Equal(frame("request", []byte("ab"), []byte("c")), frame("request", []byte("a"), []byte("bc"))) {
		t.Fatal("ambiguous field boundaries")
	}
}

func TestStrictEncodingAndProof(t *testing.T) {
	p := Proof{Timestamp: math.MaxInt64, Nonce: [32]byte{7}, MAC: [32]byte{8}}
	got, err := ParseProof(p.String())
	if err != nil || got != p {
		t.Fatal("round trip", err)
	}
	for _, s := range []string{"", p.String() + " ", " " + p.String(), strings.Replace(p.String(), "v1 ", "v1  ", 1), strings.Replace(p.String(), "v1 ", "v2 ", 1), strings.Replace(p.String(), "9223372036854775807", "+1", 1), strings.Replace(p.String(), "9223372036854775807", "01", 1), strings.Replace(p.String(), "9223372036854775807", "-1", 1), strings.Replace(p.String(), "9223372036854775807", "9223372036854775808", 1), strings.Replace(p.String(), " ", "\t", 1), p.String() + "," + p.String()} {
		if _, err := ParseProof(s); err == nil {
			t.Fatal("accepted noncanonical header")
		}
	}
	for _, s := range []string{Encode32(p.Nonce) + "=", Encode32(p.Nonce) + "\n", strings.Repeat("A", 42) + "B", strings.Repeat("/", 43), strings.Repeat("A", 44)} {
		if _, err := Decode32(s); err == nil {
			t.Fatal("accepted noncanonical value")
		}
	}
	for _, b := range []Binding{{0, "x"}, {65536, "x"}, {54321, strings.Repeat("x", 257)}, {54321, string([]byte{255})}} {
		if _, err := RequestMAC(inventedKey, b, "GET", "/api/nav/info", 0, [32]byte{}, nil); err == nil {
			t.Fatal("invalid binding")
		}
	}
	for _, route := range [][2]string{{"get", "/api/nav/info"}, {"GET", "/api/nav/login"}, {"POST", "/api/nav/info"}, {"POST", "/api/nav/login?"}} {
		if _, err := RequestMAC(inventedKey, binding, route[0], route[1], 0, [32]byte{}, nil); err == nil {
			t.Fatal("invalid route")
		}
	}
	if _, err := RequestMAC(inventedKey, binding, "POST", "/api/nav/login", 0, [32]byte{}, make([]byte, MaxRequest+1)); err == nil {
		t.Fatal("request cap")
	}
	for _, status := range []int{99, 600} {
		if _, err := HelloMAC(inventedKey, binding, [32]byte{}, status, nil); err == nil {
			t.Fatal("status")
		}
	}
	if _, err := ResponseMAC(inventedKey, Proof{}, 200, make([]byte, MaxResponse+1)); err == nil {
		t.Fatal("response cap")
	}
}

type entropy struct {
	mu      sync.Mutex
	counter uint64
}

func (e *entropy) Read(p []byte) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.counter++
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], e.counter)
	hash := sha256.Sum256(b[:])
	return copy(p, hash[:]), nil
}
func setup(t *testing.T) (*State, *atomic.Int64) {
	t.Helper()
	clock := new(atomic.Int64)
	s, err := newState(inventedKey, binding, 100000, func() time.Duration { return time.Duration(clock.Load()) * time.Millisecond }, new(entropy))
	if err != nil {
		t.Fatal(err)
	}
	return s, clock
}
func proof(t *testing.T, s *State, stamp int64, number uint64) Proof {
	t.Helper()
	var nonce [32]byte
	binary.BigEndian.PutUint64(nonce[:], number)
	mac, err := RequestMAC(s.key, s.binding, "POST", "/api/nav/login", stamp, nonce, []byte(`{"action":"issue"}`))
	if err != nil {
		t.Fatal(err)
	}
	return Proof{Timestamp: stamp, Nonce: nonce, MAC: mac}
}
func admit(t *testing.T, s *State) *Facts {
	t.Helper()
	now, err := s.Now()
	if err != nil {
		t.Fatal(err)
	}
	f, err := s.Admit("POST", "/api/nav/login", proof(t, s, now, uint64(len(s.nonces)+1)), []byte(`{"action":"issue"}`))
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func credentialFor(t *testing.T, s *State, f *Facts) string {
	t.Helper()
	code, err := s.Issue(f)
	if err != nil {
		t.Fatal(err)
	}
	bearer, err := s.Exchange(code)
	if err != nil {
		t.Fatal(err)
	}
	return bearer
}

func TestReplayAndClockBoundaries(t *testing.T) {
	for _, tc := range []struct {
		stamp int64
		valid bool
	}{{40000, false}, {40001, true}, {105000, true}, {105001, false}} {
		s, _ := setup(t)
		p := proof(t, s, tc.stamp, 1)
		_, err := s.Admit("POST", "/api/nav/login", p, []byte(`{"action":"issue"}`))
		if (err == nil) != tc.valid {
			t.Fatalf("boundary %d", tc.stamp)
		}
	}
	s, clock := setup(t)
	p := proof(t, s, 105000, 1)
	body := []byte(`{"action":"issue"}`)
	f, err := s.Admit("POST", "/api/nav/login", p, body)
	if err != nil {
		t.Fatal(err)
	}
	body[0] = 'x'
	copyBody := f.Body()
	copyBody[0] = 'y'
	if string(f.Body()) != `{"action":"issue"}` {
		t.Fatal("facts body aliases caller")
	}
	clock.Store(64999)
	p = proof(t, s, 164999, 1)
	if _, err = s.Admit("POST", "/api/nav/login", p, []byte(`{"action":"issue"}`)); err == nil {
		t.Fatal("future-edge nonce expired early")
	}
	clock.Store(65000)
	p = proof(t, s, 165000, 1)
	if _, err = s.Admit("POST", "/api/nav/login", p, []byte(`{"action":"issue"}`)); err != nil {
		t.Fatal("expiry equality not reclaimed")
	}
	clock.Store(-100)
	if now, _ := s.Now(); now != 165000 {
		t.Fatal("clock reopened")
	}
	bad := proof(t, s, 165000, 2)
	bad.MAC[0] ^= 1
	before := len(s.nonces)
	if _, err = s.Admit("POST", "/api/nav/login", bad, nil); err == nil || len(s.nonces) != before {
		t.Fatal("invalid MAC allocated state")
	}
	s.wall = math.MaxInt64
	s.elapsedMS = 1
	if _, err = s.Now(); err == nil || s.active {
		t.Fatal("clock overflow did not retire")
	}
}

func TestNonceCapacityAndRevokeRetention(t *testing.T) {
	s, _ := setup(t)
	f := admit(t, s)
	for i := uint64(2); len(s.nonces) < NonceLimit; i++ {
		var n [32]byte
		binary.BigEndian.PutUint64(n[:], i)
		s.nonces[n] = 160000
	}
	p := proof(t, s, 100000, NonceLimit+1)
	if _, err := s.Admit("POST", "/api/nav/login", p, []byte(`{"action":"issue"}`)); err == nil || len(s.nonces) != NonceLimit {
		t.Fatal("nonce capacity evicted history")
	}
	if err := s.Revoke(f); err != nil {
		t.Fatal(err)
	}
	if len(s.nonces) != NonceLimit {
		t.Fatal("revoke lost replay history")
	}
	if _, err := s.Issue(f); err == nil {
		t.Fatal("old facts survived revoke")
	}
	if _, err := s.SignResponse(f, 200, []byte(`{"ok":true}`)); err != nil {
		t.Fatal("revoke response not signed")
	}
}

func TestCredentialLifetimeAndLRU(t *testing.T) {
	s, clock := setup(t)
	f := admit(t, s)
	var values []string
	for i := 0; i < CredentialLimit; i++ {
		values = append(values, credentialFor(t, s, f))
	}
	if !s.Credential(values[0]) {
		t.Fatal("valid credential")
	}
	newest := credentialFor(t, s, f)
	if s.Credential(values[1]) || !s.Credential(values[0]) || !s.Credential(newest) {
		t.Fatal("LRU eviction")
	}
	clock.Store(idleLife - 1)
	if !s.Credential(newest) {
		t.Fatal("idle just before expiry")
	}
	clock.Store(2*idleLife - 1)
	if s.Credential(newest) {
		t.Fatal("idle equality refreshed expired credential")
	}
	s, clock = setup(t)
	f = admit(t, s)
	value := credentialFor(t, s, f)
	for elapsed := int64(60000); elapsed < absoluteLife; elapsed += 60000 {
		clock.Store(elapsed)
		if !s.Credential(value) {
			t.Fatal("successful poll did not extend idle")
		}
	}
	clock.Store(absoluteLife)
	if s.Credential(value) {
		t.Fatal("poll extended absolute life")
	}
}

func TestPendingCapacityExpiryAndExactlyOneExchange(t *testing.T) {
	s, clock := setup(t)
	f := admit(t, s)
	var code string
	for i := 0; i < PendingLimit; i++ {
		var err error
		code, err = s.Issue(f)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Issue(f); err != ErrCapacity || len(s.codes) != PendingLimit {
		t.Fatal("pending capacity changed live codes")
	}
	var successes atomic.Int64
	var group sync.WaitGroup
	for i := 0; i < 16; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := s.Exchange(code); err == nil {
				successes.Add(1)
			}
		}()
	}
	group.Wait()
	if successes.Load() != 1 {
		t.Fatal("code not consumed exactly once")
	}
	code, _ = s.Issue(f)
	clock.Store(codeLife)
	if _, err := s.Exchange(code); err == nil {
		t.Fatal("code expiry equality")
	}
}

type pausedEntropy struct {
	start, release chan struct{}
	once           sync.Once
	next           io.Reader
}

func (p *pausedEntropy) Read(b []byte) (int, error) {
	p.once.Do(func() { close(p.start); <-p.release })
	return p.next.Read(b)
}
func TestEntropyAcrossRevokeAndClose(t *testing.T) {
	for _, operation := range []string{"issue", "exchange"} {
		for _, retirement := range []string{"revoke", "close"} {
			t.Run(operation+"/"+retirement, func(t *testing.T) {
				s, _ := setup(t)
				f := admit(t, s)
				code, err := s.Issue(f)
				if err != nil {
					t.Fatal(err)
				}
				pause := &pausedEntropy{make(chan struct{}), make(chan struct{}), sync.Once{}, new(entropy)}
				s.random = pause
				result := make(chan error, 1)
				go func() {
					var err error
					if operation == "issue" {
						_, err = s.Issue(f)
					} else {
						_, err = s.Exchange(code)
					}
					result <- err
				}()
				<-pause.start
				if retirement == "close" {
					s.Close()
				} else {
					if err := s.Revoke(f); err != nil {
						t.Fatal(err)
					}
				}
				close(pause.release)
				if err := <-result; err == nil || len(s.credentials) != 0 {
					t.Fatal("retired operation published")
				}
			})
		}
	}
}

func TestEntropyFailureCollisionAndExhaustionPreserveTables(t *testing.T) {
	s, _ := setup(t)
	f := admit(t, s)
	code, err := s.Issue(f)
	if err != nil {
		t.Fatal(err)
	}
	value := credentialFor(t, s, f)
	oldSequence := s.sequence
	for _, r := range []io.Reader{bytes.NewReader(nil), bytes.NewReader(make([]byte, 31))} {
		s.random = r
		if _, err = s.Exchange(code); err == nil || len(s.codes) != 1 || len(s.credentials) != 1 || s.sequence != oldSequence {
			t.Fatal("failed entropy changed tables")
		}
	}
	decoded, _ := Decode32(value)
	s.random = bytes.NewReader(bytes.Repeat(decoded[:], 3))
	if _, err = s.Exchange(code); err == nil || len(s.codes) != 1 || len(s.credentials) != 1 || s.sequence != oldSequence {
		t.Fatal("credential collision changed tables")
	}
	decoded, _ = Decode32(code)
	s.random = bytes.NewReader(bytes.Repeat(decoded[:], 3))
	if _, err = s.Issue(f); err == nil || len(s.codes) != 1 || s.sequence != oldSequence {
		t.Fatal("code collision changed tables")
	}
	s.random = new(entropy)
	s.sequence = math.MaxUint64
	if s.Credential(value) || s.active {
		t.Fatal("sequence wrapped")
	}
	s, _ = setup(t)
	f = admit(t, s)
	s.epoch = math.MaxUint64
	f.epoch = s.epoch
	if err = s.Revoke(f); err == nil || s.active {
		t.Fatal("epoch wrapped")
	}
}

func TestExchangeCommitExpiryAndPendingIdentity(t *testing.T) {
	for _, scenario := range []string{"expired", "reissued"} {
		t.Run(scenario, func(t *testing.T) {
			s, clock := setup(t)
			f := admit(t, s)
			code, err := s.Issue(f)
			if err != nil {
				t.Fatal(err)
			}
			decoded, _ := Decode32(code)
			id := sha256.Sum256(decoded[:])
			original := s.codes[id]
			pause := &pausedEntropy{make(chan struct{}), make(chan struct{}), sync.Once{}, &entropy{counter: 1000}}
			s.random = pause
			result := make(chan error, 1)
			go func() { _, err := s.Exchange(code); result <- err }()
			<-pause.start
			if scenario == "expired" {
				clock.Store(codeLife)
			} else {
				// Same digest, epoch AND expiry can recur at frozen time. A new pending
				// incarnation must not be consumed by the suspended old exchange.
				s.mu.Lock()
				delete(s.codes, id)
				s.mu.Unlock()
				s.random = bytes.NewReader(decoded[:])
				again, err := s.Issue(f)
				if err != nil || again != code {
					t.Fatal("reissue", err)
				}
				if s.codes[id].identity == original.identity {
					t.Fatal("pending identity reused")
				}
			}
			close(pause.release)
			if err := <-result; err == nil || len(s.credentials) != 0 {
				t.Fatal("invalid old exchange committed")
			}
			if scenario == "reissued" && len(s.codes) != 1 {
				t.Fatal("new pending code consumed")
			}
		})
	}
}

func TestConcurrentProofAdmissionAndOldState(t *testing.T) {
	s, _ := setup(t)
	p := proof(t, s, 100000, 1)
	var successes atomic.Int64
	var group sync.WaitGroup
	for i := 0; i < 16; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := s.Admit("POST", "/api/nav/login", p, []byte(`{"action":"issue"}`)); err == nil {
				successes.Add(1)
			}
		}()
	}
	group.Wait()
	if successes.Load() != 1 || len(s.nonces) != 1 {
		t.Fatal("proof admission not atomic")
	}
	f := admit(t, s)
	value := credentialFor(t, s, f)
	s.Close()
	if s.Credential(value) {
		t.Fatal("closed listener accepted old credential")
	}
	replacement, _ := setup(t)
	if _, err := replacement.Issue(f); err == nil {
		t.Fatal("old facts transferred to replacement")
	}
	if _, err := replacement.SignResponse(f, 200, nil); err == nil {
		t.Fatal("foreign facts signed")
	}
}

func TestExpiredCredentialReclaimedBeforeLiveEviction(t *testing.T) {
	s, clock := setup(t)
	f := admit(t, s)
	first := credentialFor(t, s, f)
	clock.Store(1)
	var live []string
	for i := 1; i < CredentialLimit; i++ {
		live = append(live, credentialFor(t, s, f))
	}
	clock.Store(idleLife)
	if !s.Credential(live[0]) {
		t.Fatal("live credential before equality")
	}
	credentialFor(t, s, f)
	if s.Credential(first) {
		t.Fatal("expired record revived")
	}
	for _, value := range live {
		if !s.Credential(value) {
			t.Fatal("live credential evicted before expired reclaim")
		}
	}
}
