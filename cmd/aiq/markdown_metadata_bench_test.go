package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

type markdownMetadataCostCase struct {
	name, raw string
	width     int
}

func markdownMetadataCostCases() []markdownMetadataCostCase {
	table := "| head | value |\n|---|---|\n" + strings.Repeat("| long **nested *emphasis* text** | 日本語 日本語 |\n", 12)
	return []markdownMetadataCostCase{
		{"long", strings.Repeat("word  **bold** `code` 日本語 ", 200), 80},
		{"table", table, 45}, {"fallback", table, 1},
		{"quote", strings.Repeat("> quote [link](u) **bold**\n---\n", 40), 27},
	}
}

var markdownMetadataTextSink string
var markdownMetadataRowsSink []string

// Both versions perform the same terminal string and screen-row rendering.
// Digests outside timing make spacing/reset/row equivalence observable.
func BenchmarkMarkdownMetadataCost(b *testing.B) {
	for _, tc := range markdownMetadataCostCases() {
		b.Run(tc.name, func(b *testing.B) {
			md := markdownContext{palette: darkPalette}
			b.ReportAllocs()
			b.SetBytes(int64(len(tc.raw)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				markdownMetadataTextSink = md.renderMarkdown(tc.raw, tc.width, "", false)
				markdownMetadataRowsSink = md.renderMarkdownRows(tc.raw, tc.width, "")
			}
			b.StopTimer()
			payload, err := json.Marshal([]any{markdownMetadataTextSink, markdownMetadataRowsSink})
			if err != nil {
				b.Fatal(err)
			}
			b.Logf("MD_COST_DIGEST %s %x", tc.name, sha256.Sum256(payload))
		})
	}
}

// New primitive conversion is different work from the paired terminal case.
var webMarkdownMetadataSink []webMarkdownLine

func BenchmarkWebMarkdownMetadataCost(b *testing.B) {
	for _, tc := range markdownMetadataCostCases() {
		for _, markup := range []bool{false, true} {
			b.Run(fmt.Sprintf("%s/%v", tc.name, markup), func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(tc.raw)))
				for i := 0; i < b.N; i++ {
					webMarkdownMetadataSink = webMarkdownLines(tc.raw, tc.width, markup)
				}
			})
		}
	}
}
