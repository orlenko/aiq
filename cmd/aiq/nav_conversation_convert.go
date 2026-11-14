package main

import (
	"strings"

	"github.com/orlenko/aiq/internal/transcript"
)

func navConversationMessages(styled []convoStyledMessage, o navConversationOptions, start, end int) ([]navConversationMessage, error) {
	out := make([]navConversationMessage, 0, len(styled))
	type turnIdentity struct{ raw, public string }
	keys := make(map[int]turnIdentity)
	ordinal := 0
	for _, item := range styled {
		m := item.message
		if !navConversationMessageID(m.id) || !navConversationWord(m.kind, "you", "steer", "answer", "line", "notice", "skipped") ||
			!navConversationWord(m.sub, "prompt", "skipped_prompt", "steer", "reply", "said", "final", "earlier", "continuation", "skipped", "none") {
			return nil, transcript.ErrUnshowable
		}
		at, err := navConversationTime(m.at)
		if err != nil {
			return nil, err
		}
		v := navConversationMessage{ID: strings.Clone(m.id), Kind: strings.Clone(m.kind), Sub: strings.Clone(m.sub),
			Turn: m.turn, At: at, Source: "unknown", Raw: strings.Clone(m.raw), Groups: make([]navConversationGroup, 0, len(item.groups))}
		if m.turn == -1 {
			if start != 0 || m.kind != "skipped" || m.sub != "skipped" || m.rawKey != "" || m.sourceKnown || m.account.known || !m.at.IsZero() {
				return nil, transcript.ErrUnshowable
			}
			v.Key = navConversationKey("aiq.nav.preamble.v1", o.provider, o.id, "0", m.id)
		} else {
			if m.turn < start || m.turn >= end || !navConversationNumber(m.turn) {
				return nil, transcript.ErrUnshowable
			}
			key, exists := keys[m.turn]
			if exists && key.raw != m.rawKey {
				return nil, transcript.ErrUnshowable
			}
			if !exists {
				key = turnIdentity{m.rawKey, navConversationTurnKey(o, start, m.turn, m.rawKey)}
				keys[m.turn] = key
			}
			v.Key = key.public
			if m.sourceKnown {
				switch m.promptSource {
				case transcript.Human:
					v.Source = "human"
				case transcript.Aiq:
					v.Source = "aiq"
				case transcript.Peer:
					v.Source = "peer"
				case transcript.Notice:
					v.Source = "notice"
				default:
					return nil, transcript.ErrUnshowable
				}
				v.SourceKnown = true
			}
			v.AccountKnown = m.account.known
			if v.AccountKnown {
				v.Account = strings.Clone(m.account.label)
			}
		}
		for _, group := range item.groups {
			if group.ordinal != ordinal || !navConversationNumber(group.ordinal) {
				return nil, transcript.ErrUnshowable
			}
			ordinal++
			head, err := navConversationHeadValue(group.head, o)
			if err != nil {
				return nil, err
			}
			base, ok := navConversationClasses(group.base)
			if !ok {
				return nil, transcript.ErrUnshowable
			}
			g := navConversationGroup{Ordinal: group.ordinal, Visible: group.visible, LeadingBreak: group.leadingBreak,
				EmptyBodyLine: group.emptyBodyLine, Head: head, Base: base, Lines: make([]navConversationLine, 0, len(group.lines))}
			for _, line := range group.lines {
				if !navConversationWord(line.kind, "text", "table", "rule", "quote", "code") || !navConversationNumber(line.tableRows) {
					return nil, transcript.ErrUnshowable
				}
				l := navConversationLine{Kind: strings.Clone(line.kind), TableRows: line.tableRows, Spans: make([]navConversationSpan, 0, len(line.spans))}
				for _, span := range line.spans {
					classes, ok := navConversationClasses(span.classes)
					if !ok {
						return nil, transcript.ErrUnshowable
					}
					l.Spans = append(l.Spans, navConversationSpan{Classes: classes, Decorative: span.decorative, Text: strings.Clone(span.text)})
				}
				g.Lines = append(g.Lines, l)
			}
			v.Groups = append(v.Groups, g)
		}
		out = append(out, v)
	}
	return out, nil
}

func navConversationHeadValue(h convoHead, o navConversationOptions) (navConversationHead, error) {
	if !navConversationWord(h.mode, "", "prompt", "steer", "notice", "reply", "earlier", "final") ||
		!navConversationWord(h.clockMode, "", "relative", "reply") || !navConversationNumber(h.tools) {
		return navConversationHead{}, transcript.ErrUnshowable
	}
	at, err := navConversationTime(h.at)
	if err != nil {
		return navConversationHead{}, err
	}
	v := navConversationHead{Mode: strings.Clone(h.mode), Who: strings.Clone(h.who), ClockMode: strings.Clone(h.clockMode),
		At: at, DurationKnown: h.durationKnown, Tools: h.tools}
	if h.durationKnown {
		v.DurationMS = h.duration.Milliseconds()
	}
	switch h.clockMode {
	case "relative":
		v.ClockText = strings.Clone(whenLabelIn(h.at, o.now, o.location))
	case "reply":
		v.ClockText = strings.Clone(clockLabelIn(h.at, o.location))
	}
	return v, nil
}

func navConversationWord(value string, allowed ...string) bool {
	for _, word := range allowed {
		if value == word {
			return true
		}
	}
	return false
}

func navConversationClasses(in []string) ([]string, bool) {
	out := make([]string, 0, len(in))
	for _, class := range in {
		if !navConversationWord(class, "b", "dim", "i", "u", "inverse", "band", "you", "agent", "muted", "accent", "code") {
			return nil, false
		}
		out = append(out, strings.Clone(class))
	}
	return out, true
}

func navConversationMessageID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
