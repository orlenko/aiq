package main

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// These fixtures deliberately do not use the route's DTO or size helper to
// construct the expected encoding.
func TestNavJSONSizeIndependentEncoding(t *testing.T) {
	type row struct {
		Label string `json:"label"`
		Count int64  `json:"count"`
		Flags []bool `json:"flags"`
	}
	type envelope struct {
		Rows  []row    `json:"rows"`
		Empty []string `json:"empty"`
		Nil   []string `json:"nil"`
	}
	values := []any{nil, true, false, int(0), int(-12), int64(math.MinInt64), int64(math.MaxInt64), "", "日本語", "\ufffd", string([]byte{0xff}), string([]byte{0xc0, 0xaf}), "\u2028\u2029<>&\"\\\n\r\t\b\f", []string(nil), []string{}, []string{"x", "y"}, envelope{Rows: []row{{Label: "<title>\xff", Count: math.MinInt64, Flags: []bool{true, false}}}, Empty: []string{}}}
	for b := 0; b < 256; b++ {
		values = append(values, string([]byte{byte(b)}))
	}
	for _, v := range values {
		encoded, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		want := int64(len(encoded))
		for _, limit := range []int64{want, want + 1, math.MaxInt64} {
			if got, ok := navJSONSize(v, limit); !ok || got != want {
				t.Fatalf("%T size at limit %d: %d/%v, want %d", v, limit, got, ok, want)
			}
		}
		if _, ok := navJSONSize(v, want-1); ok {
			t.Fatalf("%T accepted one byte below encoded size", v)
		}
	}
	// Keep independent literals for both supported invalid-byte encodings. A
	// valid replacement rune remains three UTF-8 bytes under either codec.
	invalid := string([]byte{0xff, 0xfe})
	b, err := json.Marshal(invalid)
	if err != nil || (string(b) != `"\ufffd\ufffd"` && string(b) != "\"\ufffd\ufffd\"") {
		t.Fatalf("unexpected invalid-byte encoding %q: %v", b, err)
	}
	want := int64(len(b))
	if n, ok := navJSONSize(invalid, want); !ok || n != want {
		t.Fatalf("invalid-byte size %d/%v, want %d", n, ok, want)
	}
	if _, ok := navJSONSize(invalid, want-1); ok {
		t.Fatal("invalid bytes accepted one byte below encoded size")
	}
	for _, tc := range []struct {
		text, encoded string
		size          int64
	}{
		{"\ufffd", "\"\ufffd\"", 5},
		{"<>&", `"\u003c\u003e\u0026"`, 20},
		{"\u2028\u2029", `"\u2028\u2029"`, 14},
	} {
		b, _ := json.Marshal(tc.text)
		if string(b) != tc.encoded {
			t.Fatal("incorrect independent literal")
		}
		if n, ok := navJSONSize(tc.text, tc.size); !ok || n != tc.size {
			t.Fatalf("literal size %d/%v, want %d", n, ok, tc.size)
		}
	}
	if _, ok := navJSONSize(strings.Repeat("<", 1024), 1024); ok {
		t.Fatal("escaped expansion accepted")
	}
}

type navJSONCustom struct{ calls *int }

func (v navJSONCustom) MarshalJSON() ([]byte, error) { *v.calls++; return []byte(`"unexpected"`), nil }

type navJSONText struct{ calls *int }

func (v navJSONText) MarshalText() ([]byte, error) { *v.calls++; return []byte("unexpected"), nil }

type navJSONEmbedded struct{ X string }

func TestNavJSONSizeRejectsOpenEncoding(t *testing.T) {
	calls := 0
	for _, v := range []any{
		map[string]string{"x": "y"}, []byte("base64"), navJSONCustom{&calls}, &navJSONCustom{&calls}, navJSONText{&calls},
		struct {
			X string `json:"x,omitempty"`
		}{}, struct{ navJSONEmbedded }{},
		struct {
			X any `json:"x"`
		}{X: "interface"}, []any{"interface"},
		struct {
			X navJSONCustom `json:"x"`
		}{X: navJSONCustom{&calls}},
	} {
		if _, ok := navJSONSize(v, 1<<20); ok {
			t.Fatalf("accepted open encoding %T", v)
		}
	}
	if calls != 0 {
		t.Fatal("preflight executed custom encoding")
	}
	if _, ok := navJSONSize("x", -1); ok {
		t.Fatal("negative bound accepted")
	}
}
