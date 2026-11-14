package navclient

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"unicode/utf8"
)

// object refuses substitution by encoding/json before extracting any scalar.
// All schemas are closed and flat; decoded key spelling must be unique.
func object(raw []byte, names ...string) (map[string]any, bool) {
	if !utf8.Valid(raw) || !validStrings(raw) {
		return nil, false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	t, err := d.Token()
	if err != nil || t != json.Delim('{') {
		return nil, false
	}
	wanted := make(map[string]bool, len(names))
	for _, name := range names {
		wanted[name] = true
	}
	values := make(map[string]any, len(names))
	for d.More() {
		t, err = d.Token()
		key, isString := t.(string)
		if err != nil || !isString || !wanted[key] {
			return nil, false
		}
		delete(wanted, key)
		value, valueErr := d.Token()
		if valueErr != nil || value == nil {
			return nil, false
		}
		switch value.(type) {
		case string, bool, json.Number:
			values[key] = value
		default:
			return nil, false
		}
	}
	t, err = d.Token()
	if err != nil || t != json.Delim('}') || len(wanted) != 0 {
		return nil, false
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, false
	}
	return values, true
}

func integer(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := strconv.ParseInt(string(n), 10, 64)
	return i, err == nil && i >= 0 && strconv.FormatInt(i, 10) == string(n)
}

// JSON permits lone UTF-16 escapes that encoding/json replaces with U+FFFD.
// Validate every actual string (including keys) without that lossy conversion.
// The standard decoder remains responsible for all other JSON grammar.
func validStrings(raw []byte) bool {
	for i := 0; i < len(raw); i++ {
		if raw[i] != '"' {
			continue
		}
		i++
		for ; i < len(raw) && raw[i] != '"'; i++ {
			if raw[i] != '\\' {
				continue
			}
			i++
			if i >= len(raw) {
				return false
			}
			if raw[i] != 'u' {
				continue
			}
			u, ok := hex4(raw[i+1:])
			if !ok {
				return false
			}
			i += 4
			if u >= 0xdc00 && u <= 0xdfff {
				return false
			}
			if u >= 0xd800 && u <= 0xdbff {
				if len(raw)-i < 7 || raw[i+1] != '\\' || raw[i+2] != 'u' {
					return false
				}
				low, ok := hex4(raw[i+3:])
				if !ok || low < 0xdc00 || low > 0xdfff {
					return false
				}
				i += 6
			}
		}
		if i >= len(raw) {
			return false
		}
	}
	return true
}

func hex4(raw []byte) (uint16, bool) {
	if len(raw) < 4 {
		return 0, false
	}
	var n uint16
	for _, b := range raw[:4] {
		n <<= 4
		switch {
		case b >= '0' && b <= '9':
			n += uint16(b - '0')
		case b >= 'a' && b <= 'f':
			n += uint16(b-'a') + 10
		case b >= 'A' && b <= 'F':
			n += uint16(b-'A') + 10
		default:
			return 0, false
		}
	}
	return n, true
}
