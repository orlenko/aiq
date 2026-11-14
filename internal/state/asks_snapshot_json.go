package state

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Unlike Unmarshal([]string), reject null members and lossy surrogate repair.
func asksSnapshotStrings(text string) ([]string, error) {
	bad := ErrAsksSnapshotUnavailable
	if !utf8.ValidString(text) || !strings.HasPrefix(strings.TrimSpace(text), "[") {
		return nil, bad
	}
	var tokens []json.RawMessage
	if json.Unmarshal([]byte(text), &tokens) != nil {
		return nil, bad
	}
	out := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if len(token) < 2 || token[0] != '"' {
			return nil, bad
		}
		for i := 1; i < len(token)-1; i++ {
			if token[i] != '\\' {
				continue
			}
			i++
			if token[i] != 'u' {
				continue
			}
			n, _ := strconv.ParseUint(string(token[i+1:i+5]), 16, 16) // JSON syntax already checked.
			switch {
			case n >= 0xd800 && n <= 0xdbff:
				if i+11 > len(token)-1 || token[i+5] != '\\' || token[i+6] != 'u' {
					return nil, bad
				}
				low, _ := strconv.ParseUint(string(token[i+7:i+11]), 16, 16)
				if low < 0xdc00 || low > 0xdfff {
					return nil, bad
				}
				i += 10
			case n >= 0xdc00 && n <= 0xdfff:
				return nil, bad
			default:
				i += 4
			}
		}
		var value string
		if json.Unmarshal(token, &value) != nil {
			return nil, bad
		}
		out = append(out, strings.Clone(value))
	}
	return out, nil
}
