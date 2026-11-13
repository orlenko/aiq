package main

import (
	"fmt"
	"strings"
	"testing"
)

var projectionBenchSink []convoProjectionMessage

// Matched fragments exercise the complete-body hash and owned per-cut facts
// without measuring a renderer or file discovery.
func BenchmarkConvoProjectionFragments(b *testing.B) {
	for _, n := range []int{1000, 2000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			text, cuts := fragmentFixture(n)
			want := "root" + strings.Repeat("\n\n"+strings.Repeat("x", 32), n)
			b.ReportAllocs()
			b.SetBytes(int64(len(want)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				projectionBenchSink = projectConvoCuts(text, cuts, nil)
			}
			b.StopTimer()
			if len(projectionBenchSink) != 1 || projectionBenchSink[0].raw != want || len(projectionBenchSink[0].parts) != n+1 {
				b.Fatal("fragment projection differs")
			}
		})
	}
}
