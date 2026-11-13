package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"slices"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/transcript"
)

// Private presentation facts, not roles or evidence of event authorship.
// promptSource describes only the enclosing prompt's parser classification.
type convoFacts struct {
	turn            int
	at, turnStarted time.Time
	promptSource    transcript.Source
	sourceKnown     bool
	sub             string
	head            convoHead
}

type convoHead struct {
	mode, who, clockMode string
	at                   time.Time
	durationKnown        bool
	duration             time.Duration
	tools                int
}

func turnConvoFacts(i int, t transcript.Turn) convoFacts {
	known := t.Source <= transcript.Notice && !(t.PromptSkipped && t.Source == transcript.Human)
	return convoFacts{turn: i, turnStarted: t.Started, promptSource: t.Source, sourceKnown: known}
}

func finalConvoHead(provider string, t transcript.Turn) convoHead {
	h := convoHead{mode: "final", who: provider, tools: max(0, t.Tools)}
	if !t.Started.IsZero() && !t.Ended.IsZero() {
		h.durationKnown, h.duration = true, t.Ended.Sub(t.Started)
	}
	return h
}

// Known means supplied by the caller, not inferred from a path or lease.
type convoAccountFact struct {
	known bool
	label string
}

type convoProjectionOptions struct {
	context    convoRenderContext
	start      int
	live, hold bool
	accounts   []convoAccountFact // original turn indices, including hidden turns
	// Used only by the styled adapter; never shared between invocations.
	paintCapture *convoPaintCapture
}

type convoProjectionPart struct {
	convoFacts
	kind, text string
}

// Never serialize this private type wholesale: rawKey may contain prompt bytes.
// A later transport adapter selects its own public fields explicitly.
type convoProjectionMessage struct {
	convoFacts
	id, rawKey, kind, raw string
	account               convoAccountFact
	parts                 []convoProjectionPart
}

var (
	errConvoProjectionSession = errors.New("missing conversation session")
	errConvoProjectionStart   = errors.New("invalid conversation start")
	errConvoProjectionRender  = errors.New("conversation rendering failed")
)

// renderConvoProjection owns its working session, printer and returned slices.
// It performs no discovery, clock reads, parser work or CLI default resolution.
// Trusted callbacks in context must obey convoRenderContext's purity contract.
func renderConvoProjection(s *transcript.Session, opts convoProjectionOptions) ([]convoProjectionMessage, error) {
	if s == nil {
		return nil, errConvoProjectionSession
	}
	if opts.start < 0 || opts.start > len(s.Turns) {
		return nil, errConvoProjectionStart
	}
	owned := *s
	owned.Turns = slices.Clone(s.Turns)
	for i := range owned.Turns {
		t := &owned.Turns[i]
		t.Items = slices.Clone(t.Items)
		t.Steers = slices.Clone(t.Steers)
		t.Earlier = slices.Clone(t.Earlier)
	}
	accounts := slices.Clone(opts.accounts)
	for i := range accounts {
		accounts[i].label = oneLine(accounts[i].label)
	}
	if opts.hold {
		holdLastTurn(&owned)
	}
	p := newConvoPrinterWithContext(owned.Provider, true, false, opts.context)
	p.capture = opts.paintCapture
	p.live, p.hold, p.track = opts.live, opts.hold, true
	p.skip(owned.Turns[:opts.start])
	text := p.emit(&owned)
	if p.renderFailed {
		return nil, errConvoProjectionRender
	}
	return projectConvoCuts(text, p.cuts, accounts), nil
}

// The same folder supplies terminal paint boundaries and normalized raw bodies.
// Web identities deliberately count complete bodies separately from terminal IDs.
func projectConvoCuts(text string, cuts []convoCut, accounts []convoAccountFact) []convoProjectionMessage {
	type seed struct {
		key, kind string
		hash      uint64
	}
	seen := map[seed]int{}
	var out []convoProjectionMessage
	for _, folded := range foldConvoCuts(text, cuts) {
		first := folded.first
		h := fnv.New64a()
		_, _ = h.Write([]byte(folded.raw))
		key := seed{first.key, first.kind, h.Sum64()}
		seen[key]++
		frame := fmt.Sprintf("%s\x00%s\x00%x\x00%d", key.key, key.kind, key.hash, seen[key])
		digest := sha256.Sum256([]byte(frame))
		msg := convoProjectionMessage{
			convoFacts: normalizedConvoFacts(first.facts),
			id:         hex.EncodeToString(digest[:16]), rawKey: first.key, kind: first.kind, raw: folded.raw,
			parts: make([]convoProjectionPart, 0, folded.lastCut-folded.firstCut+1),
		}
		if i := first.facts.turn; i >= 0 && i < len(accounts) {
			msg.account = accounts[i]
		}
		for _, cut := range cuts[folded.firstCut : folded.lastCut+1] {
			msg.parts = append(msg.parts, convoProjectionPart{
				convoFacts: normalizedConvoFacts(cut.facts), kind: cut.kind,
				text: strings.TrimRight(sanitize(cut.text), "\n"),
			})
		}
		out = append(out, msg)
	}
	return out
}

func normalizedConvoFacts(f convoFacts) convoFacts {
	f.head.who = oneLine(f.head.who)
	return f
}
