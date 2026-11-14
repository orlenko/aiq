package main

import (
	"math"
	"unsafe"
)

// A typed conservative retained-graph estimate, not a heap/RSS measurement.
// Sizeof is compile-time layout arithmetic; no unsafe pointer is inspected.
func navDetailRetainedBytes(v *navDetailView) (int64, bool) {
	if v == nil {
		return 0, false
	}
	var n int64
	add := func(size uint64) bool {
		if size > uint64(math.MaxInt64-n) {
			return false
		}
		n += int64(size)
		return true
	}
	text := func(values ...string) bool {
		for _, value := range values {
			if !add(uint64(len(value))) {
				return false
			}
		}
		return true
	}
	array := func(count int, element uintptr) bool {
		if count < 0 || element != 0 && uint64(count) > uint64(math.MaxInt64)/uint64(element) {
			return false
		}
		return add(uint64(count) * uint64(element))
	}
	e := &v.envelope
	s := &e.Session
	if !add(uint64(unsafe.Sizeof(*v))) || !add(uint64(cap(v.body))) ||
		!text(v.fingerprint, e.Build, e.Selection, e.TailKey, s.Provider, s.ID, s.Cwd, s.Title, s.Model, s.Branch) ||
		!array(cap(e.Messages), unsafe.Sizeof(navConversationMessage{})) {
		return 0, false
	}
	// Unused backing slots can retain nested allocations too. Traverse the
	// complete capacity at every level, not just the serialized length.
	for _, m := range e.Messages[:cap(e.Messages)] {
		if !text(m.ID, m.Key, m.Kind, m.Sub, m.Source, m.Account, m.Raw) ||
			!array(cap(m.Groups), unsafe.Sizeof(navConversationGroup{})) {
			return 0, false
		}
		for _, g := range m.Groups[:cap(m.Groups)] {
			if !text(g.Head.Mode, g.Head.Who, g.Head.ClockMode, g.Head.ClockText) ||
				!array(cap(g.Base), unsafe.Sizeof("")) || !text(g.Base[:cap(g.Base)]...) ||
				!array(cap(g.Lines), unsafe.Sizeof(navConversationLine{})) {
				return 0, false
			}
			for _, line := range g.Lines[:cap(g.Lines)] {
				if !text(line.Kind) || !array(cap(line.Spans), unsafe.Sizeof(navConversationSpan{})) {
					return 0, false
				}
				for _, span := range line.Spans[:cap(line.Spans)] {
					if !text(span.Text) || !array(cap(span.Classes), unsafe.Sizeof("")) || !text(span.Classes[:cap(span.Classes)]...) {
						return 0, false
					}
				}
			}
		}
	}
	return n, true
}
