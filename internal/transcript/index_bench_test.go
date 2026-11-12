package transcript

import (
	"fmt"
	"path/filepath"
	"testing"
)

// The Codex scan once the heads are known: the walk and a stat of each
// rollout, no open. Generated rollouts only.
func BenchmarkIndexRescan(b *testing.B) {
	root := b.TempDir()
	for i := 0; i < 5000; i++ {
		id := fmt.Sprintf("%08x-0000-4000-8000-%012x", 2, i)
		day := filepath.Join(root, "sessions", "2026", "09", fmt.Sprintf("%02d", 1+i%28))
		path := filepath.Join(day, "rollout-2026-09-01T10-00-00-"+id+".jsonl")
		line := `{"timestamp":"2026-09-01T10:00:00Z","type":"session_meta","payload":{"id":"` + id + `","cwd":"/w/` + fmt.Sprint(i%400) + `","source":"cli"}}`
		if err := writeLinesErr(path, line); err != nil {
			b.Fatal(err)
		}
	}
	x := NewIndex(Roots{CodexSessions: []string{filepath.Join(root, "sessions")}})
	prev := x.scanCodex(nil)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		prev = x.scanCodex(prev)
	}
	b.ReportMetric(float64(len(prev.(*codexScan).all)), "rollouts")
}
