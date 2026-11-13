package transcript

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestCacheDefaultRejectsGrowthBeforeOpen(t *testing.T) {
	for _, fixture := range boundedFixtures() {
		for _, detail := range []bool{false, true} {
			name := fixture.name + "/list"
			if detail {
				name = fixture.name + "/detail"
			}
			t.Run(name, func(t *testing.T) {
				path := writeFile(t, filepath.Join(t.TempDir(), fixture.path), fixture.body)
				cfg := defaultCacheConfig
				cfg.ceiling = int64(len(fixture.body))
				c := newCache(cfg)
				c.beforeAdmit = func(string) { appendFile(t, path, fixture.append) }
				result := sharedCall(c, context.Background(), path, detail)
				if !errors.Is(result.err, ErrTooLarge) {
					t.Fatalf("production parse accepted queued growth: %v", result.err)
				}
				if st := c.Stats(); st.Parses != 1 || st.Parsed != 0 || st.Rows != 0 {
					t.Fatalf("rejected input admitted: %+v", st)
				}
				c.mu.Lock()
				_, quarantined := c.quarantine[path]
				c.mu.Unlock()
				if quarantined || len(c.sem) != 0 || len(c.flights) != 0 {
					t.Fatal("ordinary validation error retained quarantine or work")
				}
			})
		}
	}
}
