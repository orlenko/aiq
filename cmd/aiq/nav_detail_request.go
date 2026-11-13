package main

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/orlenko/aiq/internal/transcript"
)

var errNavDetailInvalidRequest = errors.New("invalid conversation request")

// A validated condition stays request-local; it is never part of a view key.
type navDetailCondition struct {
	raw      string
	wildcard bool
	valid    bool
}

func navDetailConditions(values []string) (navDetailCondition, error) {
	total := 0
	for i, value := range values {
		if i > 0 {
			total++
		}
		if total > 8192 || len(value) > 8192-total {
			return navDetailCondition{}, errNavDetailInvalidRequest
		}
		total += len(value)
	}
	raw := strings.Join(values, ",")
	c := navDetailCondition{raw: raw}
	_, valid, wildcard := navDetailScanCondition(raw, "")
	c.valid, c.wildcard = valid, wildcard
	return c, nil
}

func (c navDetailCondition) match(fingerprint string) bool {
	if !c.valid {
		return false
	}
	if c.wildcard {
		return true
	}
	matched, valid, _ := navDetailScanCondition(c.raw, fingerprint)
	return valid && matched
}

// Validate the WHOLE list before using any matching member. ETag backslashes
// and commas inside quotes are opaque bytes, not CSV or JSON escapes.
func navDetailScanCondition(raw, fingerprint string) (matched, valid, wildcard bool) {
	s := strings.Trim(raw, " \t")
	if s == "*" {
		return false, true, true
	}
	seen := false
	for len(s) > 0 {
		s = strings.TrimLeft(s, " \t,") // HTTP list syntax permits empty members.
		if s == "" {
			break
		}
		if strings.HasPrefix(s, "W/") {
			s = s[2:]
		}
		if len(s) == 0 || s[0] != '"' {
			return false, false, false
		}
		s = s[1:]
		end := 0
		for end < len(s) && s[end] != '"' {
			b := s[end]
			if !(b == 0x21 || b >= 0x23 && b <= 0x7e || b >= 0x80) {
				return false, false, false
			}
			end++
		}
		if end == len(s) {
			return false, false, false
		}
		seen = true
		matched = matched || s[:end] == fingerprint
		s = strings.TrimLeft(s[end+1:], " \t")
		if s != "" && s[0] != ',' {
			return false, false, false
		}
	}
	return matched, seen, false
}

type navDetailQuery struct {
	provider, id string
	last         int
	markup       bool
	anchor       int64
	condition    navDetailCondition
}

func navDetailDecode(req *http.Request) (navDetailQuery, navDetailCode, bool) {
	q := navDetailQuery{last: 50, markup: true}
	if req == nil || req.URL == nil {
		return q, navDetailInvalid, false
	}
	u := req.URL
	if u.RawPath != "" || u.Fragment != "" || u.Opaque != "" || strings.Contains(u.Path, "%") || u.ForceQuery && u.RawQuery == "" {
		return q, navDetailInvalid, false
	}
	rest, prefix := strings.CutPrefix(u.Path, "/api/nav/sessions/")
	if !prefix {
		return q, navDetailNotFound, false
	}
	var split bool
	q.provider, q.id, split = strings.Cut(rest, "/")
	if !split {
		return q, navDetailNotFound, false
	}
	if !transcript.IsFullID(q.provider, q.id) {
		return q, navDetailInvalid, false
	}
	target := u.Path
	if u.RawQuery != "" {
		target += "?" + u.RawQuery
	}
	if req.RequestURI != "" && req.RequestURI != target {
		return q, navDetailInvalid, false
	}
	if req.Method != http.MethodGet {
		return q, navDetailMethod, false
	}
	if len(u.RawQuery) > 4096 {
		return q, navDetailInvalid, false
	}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return q, navDetailInvalid, false
	}
	for key, value := range values {
		if len(value) != 1 || key != "last" && key != "markup" && key != "at" {
			return q, navDetailInvalid, false
		}
	}
	if value, exists := values["last"]; exists {
		if !attachDigits(value[0], 5) {
			return q, navDetailInvalid, false
		}
		n, err := strconv.Atoi(value[0])
		if err != nil || n < 1 || n > 10000 {
			return q, navDetailInvalid, false
		}
		q.last = n
	}
	if value, exists := values["markup"]; exists {
		if value[0] != "0" && value[0] != "1" {
			return q, navDetailInvalid, false
		}
		q.markup = value[0] == "1"
	}
	if value, exists := values["at"]; exists {
		if !attachDigits(value[0], 13) {
			return q, navDetailInvalid, false
		}
		q.anchor, err = strconv.ParseInt(value[0], 10, 64)
		if err != nil || q.anchor < 1 || q.anchor > 9999999999999 {
			return q, navDetailInvalid, false
		}
	}
	q.condition, err = navDetailConditions(req.Header.Values("If-None-Match"))
	if err != nil {
		return q, navDetailInvalid, false
	}
	return q, navDetailOK, true
}
