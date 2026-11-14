package main

import (
	"fmt"
	"hash/fnv"
	"strings"
)

// foldedConvoCut keeps the first cut's terminal identity separate from its
// complete copy text. Segments preserve each cut's original paint boundary.
// first.text is still original input; raw is the normalized plain copy body.
type foldedConvoCut struct {
	first             convoCut
	raw               string
	firstHash         uint64
	occurrence        int
	segments          []string
	firstCut, lastCut int // inclusive ordinals in the original cuts
}

// foldConvoCuts consumes the trusted byte offsets produced by emit/track.
// Work and storage are local to this call; continuations never attach across
// pane parts. Joining once avoids copying a growing prefix for every fragment.
func foldConvoCuts(text string, cuts []convoCut) []foldedConvoCut {
	var out []foldedConvoCut
	var chunks [][]string
	seen := map[string]int{}
	prev := 0
	for ordinal, ct := range cuts {
		raw := strings.TrimRight(sanitize(ct.text), "\n")
		last := len(out) - 1
		if ct.kind == "part" {
			if last >= 0 {
				out[last].lastCut = ordinal
				chunks[last] = append(chunks[last], "\n\n", raw)
			}
			prev = ct.end
			continue
		}
		segment := strings.TrimSuffix(text[prev:ct.end], "\n")
		prev = ct.end
		if last >= 0 && (ct.kind == "more" || ct.kind == "none") {
			out[last].lastCut = ordinal
			out[last].segments = append(out[last].segments, segment)
			if ct.kind == "more" {
				chunks[last] = append(chunks[last], "\n\n", raw)
			}
			continue
		}
		h := fnv.New64a()
		h.Write([]byte(raw))
		firstHash := h.Sum64()
		seed := fmt.Sprintf("%s\x00%s\x00%x", ct.key, ct.kind, firstHash)
		seen[seed]++
		out = append(out, foldedConvoCut{first: ct, firstHash: firstHash, occurrence: seen[seed], segments: []string{segment}, firstCut: ordinal, lastCut: ordinal})
		chunks = append(chunks, []string{raw})
	}
	for i := range out {
		out[i].raw = strings.Join(chunks[i], "")
	}
	return out
}
