package navtoken

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"
)

const literalKey = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"
const literalOn = "on 123 -456 54321 " + literalKey + "\n"

func expectedKey() [32]byte {
	var k [32]byte
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

func TestLiteralRecords(t *testing.T) {
	for _, tc := range []struct {
		line       string
		state      Status
		pid, start int64
		port       int
		key        bool
	}{
		{literalOn, On, 123, -456, 54321, true},
		{"off 1 0\n", Off, 1, 0, 0, false},
		{"refused 9223372036854775807 -9223372036854775808\n", Refused, 9223372036854775807, -9223372036854775808, 0, false},
		{"failed 1 9223372036854775807\n", Failed, 1, 9223372036854775807, 0, false},
		{"on 9223372036854775807 -9223372036854775808 65535 AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\n", On, 9223372036854775807, -9223372036854775808, 65535, true},
	} {
		r, e := parse([]byte(tc.line))
		if e != nil {
			t.Fatal("valid literal refused")
		}
		port, present := r.Port()
		key, keyPresent := r.Key()
		if r.Status() != tc.state || r.PID() != tc.pid || r.StartedUnix() != tc.start || port != tc.port || present != tc.key || keyPresent != tc.key {
			t.Error("literal metadata mismatch")
		}
		if tc.line == literalOn && key != expectedKey() {
			t.Error("literal raw key mismatch")
		}
		if len(tc.line) > 90 && (len(tc.line) != 94 || key != [32]byte{}) {
			t.Error("canonical maximum/all-zero control invalid")
		}
		if tc.state == Refused && len(tc.line) != 49 {
			t.Error("inactive canonical maximum changed")
		}
	}
	bad := []string{"", strings.TrimSuffix(literalOn, "\n"), literalOn + "\n", literalOn + "extra", strings.Replace(literalOn, "on ", "ON ", 1), strings.Replace(literalOn, "123", "0123", 1), strings.Replace(literalOn, "123", "+123", 1), strings.Replace(literalOn, "123", "0", 1), strings.Replace(literalOn, "123", "-1", 1), strings.Replace(literalOn, "-456", "-0", 1), strings.Replace(literalOn, "-456", "+1", 1), strings.Replace(literalOn, "-456", "9223372036854775808", 1), strings.Replace(literalOn, "54321", "65536", 1), strings.Replace(literalOn, "54321", "0", 1), strings.Replace(literalOn, "54321", "01", 1), strings.Replace(literalOn, " ", "\t", 1), strings.Replace(literalOn, " ", "  ", 1), strings.Replace(literalOn, "\n", "\r\n", 1), strings.Replace(literalOn, literalKey, literalKey+"=", 1), strings.Replace(literalOn, literalKey, literalKey[:42]+"9", 1), "on 1 0\n", "off 1 0 1 " + literalKey + "\n", "off 1\x00 0\n", strings.Repeat("x", 128), strings.Repeat("x", 129)}
	for i, line := range bad {
		r, e := parse([]byte(line))
		if e != ErrInvalid || r.Status() != Invalid {
			t.Errorf("invalid vector %d admitted", i)
		}
		if _, ok := r.Key(); ok {
			t.Error("invalid record retained key")
		}
	}
}

type hostileReadError struct{ calls *int }

func (e hostileReadError) Error() string { *e.calls++; panic("Error called") }
func (e hostileReadError) Unwrap() error { *e.calls++; panic("Unwrap called") }
func (e hostileReadError) Is(error) bool { *e.calls++; panic("Is called") }

type controlledFile struct {
	*bytes.Reader
	readErr, closeErr error
	closes, readBytes int
}

func (f *controlledFile) Read(p []byte) (int, error) {
	n, e := f.Reader.Read(p)
	f.readBytes += n
	if f.readErr != nil {
		return n, f.readErr
	}
	return n, e
}
func (f *controlledFile) Close() error { f.closes++; return f.closeErr }

func TestBoundedReadAndCleanup(t *testing.T) {
	calls := 0
	hostile := hostileReadError{&calls}
	for _, tc := range []struct {
		name, line              string
		readErr, closeErr, want error
	}{
		{"valid", literalOn, nil, nil, nil},
		{"read error", literalOn, hostile, nil, ErrUnavailable},
		{"close error", literalOn, nil, hostile, ErrUnavailable},
		{"primary parse wins", "bad", nil, hostile, ErrInvalid},
		{"read and close", literalOn, hostile, hostile, ErrUnavailable},
		{"physical128", strings.Repeat("x", 128), nil, nil, ErrInvalid},
		{"physical129", strings.Repeat("x", 129), nil, nil, ErrInvalid},
		{"huge", strings.Repeat("x", 1<<20), nil, nil, ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &controlledFile{Reader: bytes.NewReader([]byte(tc.line)), readErr: tc.readErr, closeErr: tc.closeErr}
			r, e := readFile(f)
			if e != tc.want || f.closes != 1 || f.readBytes > 129 {
				t.Error("read/error/cleanup boundary failed")
			}
			if tc.want != nil {
				if _, ok := r.Key(); ok || r.Status() != Invalid {
					t.Error("failed read published usable record")
				}
			}
		})
	}
	if calls != 0 {
		t.Error("reader queried hostile error methods")
	}
	for _, path := range []string{"", string([]byte{255}), "invented\x00path"} {
		if r, e := Read(path); e != ErrUnavailable || r.Status() != Invalid {
			t.Error("invalid explicit path admitted")
		}
	}
}

type inlineRecord struct{ key [32]byte }
type singlePointerRecord struct{ key *[32]byte }

func (inlineRecord) Format(s fmt.State, _ rune)        { io.WriteString(s, redacted) }
func (singlePointerRecord) Format(s fmt.State, _ rune) { io.WriteString(s, redacted) }

func TestHistoricalFormattingWitnesses(t *testing.T) {
	key := expectedKey()
	decimal := fmt.Sprint(key)
	if !strings.Contains(fmt.Sprintf("%v", struct{ record inlineRecord }{inlineRecord{key}}), decimal) {
		t.Fatal("inline baseline did not expose decimal key")
	}
	for _, verb := range []string{"%s", "%q"} {
		inline := fmt.Sprintf(verb, struct{ record inlineRecord }{inlineRecord{key}})
		pointer := fmt.Sprintf(verb, struct{ record singlePointerRecord }{singlePointerRecord{&key}})
		inlineKey := string(key[:])
		if verb == "%q" {
			inlineKey = strconv.Quote(inlineKey)
		}
		if !strings.Contains(inline, inlineKey) || !strings.Contains(pointer, decimal) {
			t.Fatal("historical representation did not expose complete key")
		}
	}
}

func TestRecordRedactionAndKeyCopies(t *testing.T) {
	r, e := parse([]byte(literalOn))
	if e != nil {
		t.Fatal(e)
	}
	copyRecord := r
	calls := 0
	original := r.key
	r.key = func() [32]byte { calls++; return original() }
	forms := []any{r, &r, copyRecord, struct{ Record Record }{r}, struct{ record Record }{r}, []Record{r}, map[string]Record{"invented": r}, []any{struct{ record Record }{r}}}
	key := expectedKey()
	secrets := []string{literalKey, fmt.Sprint(key), hex.EncodeToString(key[:]), base64.RawURLEncoding.EncodeToString(key[:]), string(key[:]), strconv.Quote(string(key[:]))}
	for _, form := range forms {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%p", "%#x", "%020v", "%b", "%U"} {
			out := fmt.Sprintf(verb, form)
			for _, secret := range secrets {
				if strings.Contains(out, secret) {
					t.Fatal("record formatting exposed key")
				}
			}
		}
	}
	for _, form := range forms[:5] {
		b, err := json.Marshal(form)
		if err != nil {
			t.Fatal("record JSON failed")
		}
		for _, secret := range secrets {
			if strings.Contains(string(b), secret) {
				t.Error("record JSON exposed key")
			}
		}
	}
	if r.String() != redacted || r.GoString() != redacted || fmt.Sprintf("%v", r) != redacted {
		t.Error("direct marker changed")
	}
	if calls != 0 {
		t.Error("formatting invoked private key-copy function")
	}
	k, ok := r.Key()
	if !ok || k != key {
		t.Fatal("key accessor failed")
	}
	k[0] = 255
	k2, ok := copyRecord.Key()
	if !ok || k2 != key {
		t.Error("key extraction mutated copied record")
	}
	for _, line := range []string{"off 1 0\n", "refused 1 0\n", "failed 1 0\n"} {
		record, err := parse([]byte(line))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := record.Key(); ok {
			t.Error("non-on key present")
		}
	}
	var absent Record
	if _, ok := absent.Key(); ok {
		t.Error("zero record key present")
	}
}
