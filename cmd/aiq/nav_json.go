package main

import (
	"encoding"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The active JSON codec can escape an invalid UTF-8 byte or emit the replacement
// rune directly. Probe only a fixed built-in string, once, outside request work.
var navJSONInvalidUTF8Width = func() int64 {
	b, _ := json.Marshal("\xff")
	return int64(len(b) - 2) // exclude quotes
}()

// navJSONSize preflights our closed transport DTOs without allocating escaped
// strings. Unsupported representations fail closed; this is not a general JSON
// encoder. In particular, dependencies cannot supply custom marshal methods.
func navJSONSize(value any, limit int64) (int64, bool) {
	if limit < 0 {
		return 0, false
	}
	m := navJSONMeter{limit: limit}
	ok := m.value(reflect.ValueOf(value))
	return m.used, ok
}

type navJSONMeter struct{ used, limit int64 }

func (m *navJSONMeter) add(n int64) bool {
	if n < 0 || n > m.limit-m.used {
		return false
	}
	m.used += n
	return true
}

func (m *navJSONMeter) text(s string) bool {
	if !m.add(2) { // quotes
		return false
	}
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			n := int64(1)
			switch c {
			case '"', '\\', '\b', '\f', '\n', '\r', '\t':
				n = 2
			case '<', '>', '&':
				n = 6
			default:
				if c < 0x20 {
					n = 6
				}
			}
			if !m.add(n) {
				return false
			}
			i++
			continue
		}
		r, width := utf8.DecodeRuneInString(s[i:])
		n := int64(width)
		if r == utf8.RuneError && width == 1 {
			n = navJSONInvalidUTF8Width
		} else if r == '\u2028' || r == '\u2029' {
			n = 6
		}
		if !m.add(n) {
			return false
		}
		i += width
	}
	return true
}

var (
	navJSONMarshaler = reflect.TypeFor[json.Marshaler]()
	navTextMarshaler = reflect.TypeFor[encoding.TextMarshaler]()
)

func (m *navJSONMeter) value(v reflect.Value) bool {
	if !v.IsValid() {
		return m.add(4)
	}
	t := v.Type()
	if t.Implements(navJSONMarshaler) || t.Implements(navTextMarshaler) ||
		reflect.PointerTo(t).Implements(navJSONMarshaler) || reflect.PointerTo(t).Implements(navTextMarshaler) {
		return false
	}
	switch v.Kind() {
	case reflect.String:
		return m.text(v.String())
	case reflect.Bool:
		if v.Bool() {
			return m.add(4)
		}
		return m.add(5)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return m.add(int64(len(strconv.FormatInt(v.Int(), 10))))
	case reflect.Slice:
		// []byte has a different wire representation in encoding/json.
		if t.Elem().Kind() == reflect.Uint8 {
			return false
		}
		if v.IsNil() {
			return m.add(4)
		}
		if !m.add(2) {
			return false
		}
		for i := 0; i < v.Len(); i++ {
			if i > 0 && !m.add(1) || !m.value(v.Index(i)) {
				return false
			}
		}
		return true
	case reflect.Struct:
		if !m.add(2) {
			return false
		}
		count := 0
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			// Even an unexported embedded struct can promote exported JSON
			// fields. This closed DTO subset deliberately rejects embedding.
			if f.Anonymous {
				return false
			}
			if f.PkgPath != "" { // nonembedded unexported fields are omitted
				continue
			}
			name, present := f.Tag.Lookup("json")
			if name == "-" {
				continue
			}
			if !present || name == "" || strings.Contains(name, ",") {
				return false
			}
			if count > 0 && !m.add(1) || !m.text(name) || !m.add(1) || !m.value(v.Field(i)) {
				return false
			}
			count++
		}
		return true
	}
	return false
}
