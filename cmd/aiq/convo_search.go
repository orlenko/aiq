package main

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const convoSearchLimit = 256 // runes; a query is local to this open viewer

// convoSearch leaves the conversation's selection and viewport alone until Enter.
// IDs keep a chosen result stable when --follow rebuilds the message list.
type convoSearch struct {
	query   string
	matches []int
	sel     int
	id      string
}

// convoSearchMode is also used by the independent key reader: query text and Escape
// must not quit a screen whose output is blocked. Its transitions do not depend on hits.
func convoSearchMode(active bool, e asksInput) bool {
	if e.paste || e.mouse {
		return active
	}
	if active {
		return e.key.code != keyEnter && (e.key.code != keyBack || e.erase)
	}
	return e.key.r == '/'
}

func (c *convoScreen) refreshSearch() {
	s := c.search
	if s == nil {
		return
	}
	s.matches = s.matches[:0]
	query := strings.ToLower(s.query)
	if query != "" {
		for i, m := range c.msgs {
			if strings.Contains(strings.ToLower(m.raw), query) {
				s.matches = append(s.matches, i)
			}
		}
	}
	s.sel = clamp(s.sel, 0, max(len(s.matches)-1, 0))
	for i, msg := range s.matches {
		if c.msgs[msg].id == s.id {
			s.sel = i
			break
		}
	}
	c.searchRemember()
}

func (c *convoScreen) searchRemember() {
	s := c.search
	s.id = ""
	if len(s.matches) > 0 {
		s.id = c.msgs[s.matches[s.sel]].id
	}
}

func (c *convoScreen) searchInput(e asksInput) bool {
	s := c.search
	switch {
	case e.paste:
		c.message = "Pasted text does nothing here"
	case e.mouse:
		if e.button == 64 || e.button == 65 {
			d := 1
			if e.button == 64 {
				d = -1
			}
			s.sel = clamp(s.sel+d, 0, max(len(s.matches)-1, 0))
			c.searchRemember()
		}
	case e.key.code == keyQuit:
		return true
	case e.erase:
		_, n := utf8.DecodeLastRuneInString(s.query)
		s.query = s.query[:len(s.query)-n]
		s.sel, s.id = 0, ""
		c.refreshSearch()
	case e.key.code == keyBack:
		c.search = nil
	case e.key.code == keyEnter:
		if len(s.matches) > 0 {
			c.sel = s.matches[s.sel]
			c.top = c.searchRow(c.sel, s.query)
			if c.top < 0 {
				c.top = c.msgs[c.sel].from
				c.message = "Match in message text: " + convoSearchSnippet(c.msgs[c.sel].raw, s.query)
			}
			c.clampTop()
		} else if s.query != "" {
			c.message = "No matching messages"
		}
		c.search = nil
	case e.text != 0:
		if !unicode.IsControl(e.text) && sanitize(string(e.text)) == string(e.text) {
			if utf8.RuneCountInString(s.query) < convoSearchLimit {
				s.query += string(e.text)
				s.sel, s.id = 0, ""
				c.refreshSearch()
			} else {
				c.message = fmt.Sprintf("Search is limited to %d characters", convoSearchLimit)
			}
		}
	case e.key.code == keyDown || e.key.code == keyUp:
		d := 1
		if e.key.code == keyUp {
			d = -1
		}
		s.sel = clamp(s.sel+d, 0, max(len(s.matches)-1, 0))
		c.searchRemember()
	}
	c.settle()
	return false
}

// searchRow finds a painted match, including a phrase wrapped at spaces or a long
// word wrapped between glyphs. Some narration/Markdown exists only in copy text;
// -1 lets Enter explicitly identify that owning-message fallback.
func (c *convoScreen) searchRow(msg int, query string) int {
	m, query := c.msgs[msg], strings.ToLower(query)
	first := max(m.from, m.body)
	var text strings.Builder
	var starts, rows []int
	find := func() int {
		at := strings.Index(text.String(), query)
		if at < 0 {
			return -1
		}
		i := 0
		for i+1 < len(starts) && starts[i+1] <= at {
			i++
		}
		return rows[i]
	}
	for row := first; row < m.to; row++ {
		r := c.rows[row]
		if !r.wrap && len(rows) > 0 {
			if hit := find(); hit >= 0 {
				return hit
			}
			text.Reset()
			starts, rows = starts[:0], rows[:0]
		}
		if r.wrap {
			text.WriteString(r.gap)
		}
		starts, rows = append(starts, text.Len()), append(rows, row)
		text.WriteString(strings.ToLower(kitPlain(r.text)))
	}
	if len(rows) > 0 {
		return find()
	}
	return -1
}

// convoSearchSnippet uses rune offsets because Unicode lowercasing can change byte
// lengths. Copy text is sanitized already; oneLine also protects direct callers.
func convoSearchSnippet(raw, query string) string {
	at := strings.Index(strings.ToLower(raw), strings.ToLower(query))
	if at < 0 {
		return ""
	}
	index := utf8.RuneCountInString(strings.ToLower(raw)[:at])
	runes := []rune(raw)
	from := max(index-16, 0)
	to := min(index+utf8.RuneCountInString(query)+48, len(runes))
	text := oneLine(string(runes[from:to]))
	if from > 0 {
		text = "…" + text
	}
	if to < len(runes) {
		text += "…"
	}
	return text
}

func (c *convoScreen) searchFrame(th kitTheme, mk kitMarks) []string {
	s := c.search
	var out []string
	for _, line := range c.head {
		out = append(out, kitLine([]kitSeg{{text: convoExpandTabs(line), raw: true}}, "", c.w))
	}
	start := max(s.sel-c.height()+1, 0)
	for row := 0; row < c.height(); row++ {
		index := start + row
		var segs []kitSeg
		if index < len(s.matches) {
			m := c.msgs[s.matches[index]]
			mark := "  "
			if index == s.sel {
				mark = mk.selected + " "
			}
			segs = []kitSeg{{style: th.accent, text: mark}, {style: th.dim, text: m.kind + ": "},
				{text: convoSearchSnippet(m.raw, s.query)}}
		} else if row == 0 {
			label := "No matching messages"
			if s.query == "" {
				label = "Type a substring to search loaded messages"
			}
			segs = []kitSeg{{style: th.dim, text: " " + label}}
		}
		out = append(out, kitLine(segs, "", c.w))
	}
	query := []rune(s.query)
	clipped := false
	for len(query) > 0 && cells(string(query)) > max(c.w-4, 0) {
		query, clipped = query[1:], true
	}
	prefix := " /"
	if clipped {
		prefix += "…"
	}
	label := fmt.Sprintf(" %d matches · arrows choose · Enter focus · Esc cancel", len(s.matches))
	if c.message != "" {
		label = " " + c.message
	}
	out = append(out, kitLine([]kitSeg{{style: th.accent, text: prefix + string(query)}}, "", c.w),
		kitLine([]kitSeg{{style: th.dim, text: label}}, "", c.w))
	return out[:min(len(out), c.h)]
}
