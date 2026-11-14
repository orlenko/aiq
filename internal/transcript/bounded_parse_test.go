package transcript

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type boundedFixture struct {
	name, path, body, append string
	parse                    func(string) (*Session, error)
}

func boundedFixtures() []boundedFixture {
	return []boundedFixture{
		{"claude", "invented.jsonl", `{"type":"user","message":{"content":"first"}}` + "\n",
			`{"type":"user","message":{"content":"later"}}` + "\n", ParseClaude},
		{"agy", "brain/invented/.system_generated/logs/transcript.jsonl", `{"type":"USER_INPUT","content":"first"}` + "\n",
			`{"type":"USER_INPUT","content":"later"}` + "\n", ParseAgy},
		// The appended marker would suppress the first response-item prompt if
		// it escaped the bounded preliminary pass, even with a bounded main pass.
		{"codex", "rollout-invented.jsonl", `{"type":"session_meta","payload":{"id":"invented","source":"cli"}}` + "\n" +
			`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"first"}]}}` + "\n",
			`{"type":"event_msg","payload":{"type":"user_message","message":"later"}}` + "\n", ParseCodex},
	}
}

// All data and metadata come from invented temporary files. The per-call seam
// leaves production OpenRegular and concurrent tests untouched.
type boundedTestDescriptor struct {
	info                 os.FileInfo
	data                 *strings.Reader
	read                 func([]byte, int64) (int, error)
	stat                 func(int) (os.FileInfo, error)
	reads, stats, closes int
	ends                 []int64
	closeErr             error
}

func (d *boundedTestDescriptor) ReadAt(p []byte, off int64) (int, error) {
	d.reads++
	d.ends = append(d.ends, off+int64(len(p)))
	if d.read != nil {
		return d.read(p, off)
	}
	return d.data.ReadAt(p, off)
}

func (d *boundedTestDescriptor) Stat() (os.FileInfo, error) {
	d.stats++
	if d.stat != nil {
		return d.stat(d.stats)
	}
	return d.info, nil
}

func (d *boundedTestDescriptor) Close() error                           { d.closes++; return d.closeErr }
func (d *boundedTestDescriptor) open(string) (boundedDescriptor, error) { return d, nil }

func boundedDescriptorFixture(t *testing.T, name, body string) (*boundedTestDescriptor, Snapshot) {
	t.Helper()
	path := writeFile(t, filepath.Join(t.TempDir(), name), body)
	snap, err := Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return &boundedTestDescriptor{info: info, data: strings.NewReader(body)}, snap
}

type boundedSizedInfo struct {
	os.FileInfo
	size int64
}

func (i boundedSizedInfo) Size() int64 { return i.size }

func TestBoundedParseExtentAndLegacy(t *testing.T) {
	for _, fixture := range boundedFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			_, snap := boundedDescriptorFixture(t, fixture.path, fixture.body)
			// Grow after discovery. Opening a newer descriptor must still use
			// the old extent and timestamp, under the old flight key.
			writeFile(t, snap.Path, fixture.body+fixture.append)
			later := time.Unix(1_900_000_000, 0)
			if err := os.Chtimes(snap.Path, later, later); err != nil {
				t.Fatal(err)
			}
			s, err := boundedParse(snap, int64(len(fixture.body+fixture.append)))
			if err != nil || s == nil || s.Provider != fixture.name || len(s.Turns) != 1 || s.Turns[0].Prompt != "first" {
				t.Fatalf("bounded session = %+v, err = %v", s, err)
			}
			if fixture.name != "codex" && s.Updated.UnixNano() != snap.Mtime {
				t.Fatalf("fallback = %v, want snapshot mtime", s.Updated)
			}
			if _, err := boundedParse(snap, snap.Size-1); !errors.Is(err, ErrTooLarge) {
				t.Fatalf("bounded cap = %v", err)
			}
			legacy, err := fixture.parse(snap.Path)
			if err != nil || legacy == nil || legacy.Turns[len(legacy.Turns)-1].Prompt != "later" {
				t.Fatalf("uncapped session = %+v, err = %v", legacy, err)
			}
			if fixture.name != "codex" && !legacy.Updated.Equal(later) {
				t.Fatalf("legacy fallback = %v, want final descriptor mtime", legacy.Updated)
			}
		})
	}
}

func TestBoundedParseSmallCeiling(t *testing.T) {
	const ceiling = 512
	for _, fixture := range boundedFixtures() {
		for _, size := range []int{0, ceiling - 1, ceiling, ceiling + 1} {
			t.Run(fmt.Sprintf("%s/%d", fixture.name, size), func(t *testing.T) {
				body := ""
				if size > 0 {
					body = fixture.body + strings.Repeat(" ", size-len(fixture.body))
				}
				_, snap := boundedDescriptorFixture(t, fixture.path, body)
				s, err := boundedParse(snap, ceiling)
				if size > ceiling {
					if s != nil || !errors.Is(err, ErrTooLarge) {
						t.Fatalf("over ceiling: session = %+v, err = %v", s, err)
					}
				} else if err != nil || (s == nil) != (fixture.name == "codex" && size == 0) {
					t.Fatalf("within ceiling: session = %+v, err = %v", s, err)
				}
			})
		}
	}
}

func TestBoundedParseDescriptorValidation(t *testing.T) {
	fault := errors.New("invented stat failure")
	fixtures := append(boundedFixtures(), boundedFixture{name: "codex-no-id", path: "rollout-empty.jsonl", body: "{}\n"})
	for _, fixture := range fixtures {
		for _, phase := range []int{1, 2} {
			for _, kind := range []string{"stat", "shrink", "growth", "identity", "regular"} {
				t.Run(fmt.Sprintf("%s/%d/%s", fixture.name, phase, kind), func(t *testing.T) {
					d, snap := boundedDescriptorFixture(t, fixture.path, fixture.body)
					other, _ := boundedDescriptorFixture(t, "other.jsonl", fixture.body)
					dirInfo, err := os.Stat(t.TempDir())
					if err != nil {
						t.Fatal(err)
					}
					if kind == "identity" && snap.Dev == 0 && snap.Ino == 0 {
						t.Skip("descriptor identity unsupported on this platform")
					}
					ceiling := snap.Size + 10
					d.stat = func(n int) (os.FileInfo, error) {
						if n != phase {
							return d.info, nil
						}
						switch kind {
						case "stat":
							return nil, fault
						case "shrink":
							return boundedSizedInfo{d.info, snap.Size - 1}, nil
						case "growth":
							return boundedSizedInfo{d.info, ceiling + 1}, nil
						case "identity":
							return other.info, nil
						default:
							return boundedSizedInfo{dirInfo, snap.Size}, nil
						}
					}
					s, err := boundedParseWithOpen(snap, ceiling, d.open)
					want := errBoundedInput
					if phase == 1 && kind == "growth" {
						want = ErrTooLarge
					}
					if s != nil || !errors.Is(err, want) || d.closes != 1 || d.stats != phase || phase == 1 && d.reads != 0 {
						t.Fatalf("session = %+v, err = %v, reads/stats/closes = %d/%d/%d", s, err, d.reads, d.stats, d.closes)
					}
				})
			}
		}
	}
}

func TestBoundedParseRequestedSizeAndOpenFailure(t *testing.T) {
	for _, sizes := range [][2]int64{{-1, 10}, {0, -1}, {11, 10}} {
		_, err := boundedParseWithOpen(Snapshot{Size: sizes[0]}, sizes[1], func(string) (boundedDescriptor, error) {
			t.Fatal("inadmissible extent opened a descriptor")
			return nil, nil
		})
		if err == nil {
			t.Fatal("inadmissible extent succeeded")
		}
	}
	want := errors.New("invented open failure")
	if _, err := boundedParseWithOpen(Snapshot{}, 0, func(string) (boundedDescriptor, error) { return nil, want }); err != want {
		t.Fatalf("open error = %v", err)
	}
	if _, err := boundedParse(Snapshot{Path: t.TempDir()}, 10); err == nil {
		t.Fatal("production opener accepted a directory")
	}
}

func TestBoundedParseShortReadRegrowth(t *testing.T) {
	fixtures := append(boundedFixtures(), boundedFixture{name: "codex-no-id", path: "rollout-empty.jsonl", body: "{}\n"})
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			d, snap := boundedDescriptorFixture(t, fixture.path, fixture.body)
			d.read = func(p []byte, off int64) (int, error) {
				if d.reads == 1 {
					return copy(p, fixture.body[:len(fixture.body)-1]), io.EOF
				}
				return d.data.ReadAt(p, off) // healthy again, final Stat also adequate
			}
			s, err := boundedParseWithOpen(snap, snap.Size, d.open)
			if s != nil || err != errBoundedInput || d.stats != 2 || d.closes != 1 {
				t.Fatalf("session = %+v, err = %v, stats/closes = %d/%d", s, err, d.stats, d.closes)
			}
			// A fresh production request has a new observer and can recover.
			s, err = boundedParse(snap, snap.Size)
			if err != nil || (s == nil) != (fixture.name == "codex-no-id") {
				t.Fatalf("fresh session = %+v, err = %v", s, err)
			}
		})
	}
}

func TestBoundedParseCodexMarkerShortReadRegrowth(t *testing.T) {
	for _, nilSession := range []bool{false, true} {
		for _, readErr := range []error{nil, io.EOF, errors.New("invented marker read error")} {
			t.Run(fmt.Sprintf("nil=%t/%v", nilSession, readErr), func(t *testing.T) {
				meta := `{"type":"session_meta","payload":{"id":"invented","source":"cli"}}` + "\n"
				if nilSession {
					meta = `{"type":"session_meta","payload":{"id":"invented","source":{"subagent":"invented"}}}` + "\n"
				}
				prefix := meta + `{"type":"event_msg","payload":{"type":"user_message","message":"first"}}` + "\n"
				body := prefix + strings.Repeat(" ", 128)
				d, snap := boundedDescriptorFixture(t, "rollout-marker.jsonl", body)
				zeroOffsets := 0
				d.read = func(p []byte, off int64) (int, error) {
					if off == 0 {
						zeroOffsets++
					}
					if d.reads == 1 {
						return copy(p, prefix), readErr // contains swallows this error
					}
					return d.data.ReadAt(p, off)
				}
				// First establish the provider really swallows that first-pass
				// error and succeeds after seeking back over healthy regrown data.
				s, err := parseCodexReader(snap.Path, io.NewSectionReader(d, 0, snap.Size))
				if err != nil || (s == nil) != nilSession || zeroOffsets != 2 {
					t.Fatalf("provider control: session = %+v, err = %v, passes = %d", s, err, zeroOffsets)
				}
				d.reads, zeroOffsets = 0, 0
				s, err = boundedParseWithOpen(snap, snap.Size, d.open)
				if s != nil || err != errBoundedInput || zeroOffsets != 2 || d.stats != 2 || d.closes != 1 {
					t.Fatalf("bounded: session = %+v, err = %v, passes/stats/closes = %d/%d/%d", s, err, zeroOffsets, d.stats, d.closes)
				}
			})
		}
	}
}

func TestBoundedReaderLogicalEndpoint(t *testing.T) {
	for _, extent := range []int64{0, 3} {
		for _, eofWithFullRead := range []bool{false, true} {
			d := &boundedTestDescriptor{data: strings.NewReader("abc")}
			d.read = func(p []byte, off int64) (int, error) {
				n, err := d.data.ReadAt(p, off)
				if eofWithFullRead {
					err = io.EOF
				}
				return n, err
			}
			observer := &shortReadObserver{r: d}
			r := io.NewSectionReader(observer, 0, extent)
			if _, err := r.Read(nil); err != nil && err != io.EOF {
				t.Fatal(err)
			}
			got, err := io.ReadAll(r) // buffer larger than the remaining extent
			if err != nil || string(got) != "abc"[:extent] || observer.short {
				t.Fatalf("extent %d: got %q, err %v, short %t", extent, got, err, observer.short)
			}
			if _, err := r.Read(make([]byte, 3)); err != io.EOF || observer.short {
				t.Fatalf("endpoint error = %v, short = %t", err, observer.short)
			}
			for _, end := range d.ends {
				if end > extent {
					t.Fatalf("read extent = %d, want <= %d", end, extent)
				}
			}
		}
	}
}

func TestBoundedParseProviderErrorAndPanicPrecedence(t *testing.T) {
	for _, fixture := range boundedFixtures() {
		for _, panics := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/panic=%t", fixture.name, panics), func(t *testing.T) {
				d, snap := boundedDescriptorFixture(t, fixture.path, fixture.body)
				fault := errors.New("invented provider failure")
				d.stat = func(n int) (os.FileInfo, error) {
					if n > 1 {
						return nil, errors.New("invented final stat failure")
					}
					return d.info, nil
				}
				d.read = func(p []byte, off int64) (int, error) {
					if panics {
						panic(fault)
					}
					return copy(p, "{"), fault // also latches a physical short read
				}
				var recovered any
				var s *Session
				var err error
				func() {
					defer func() { recovered = recover() }()
					s, err = boundedParseWithOpen(snap, snap.Size, d.open)
				}()
				if s != nil || d.closes != 1 || d.stats != 1 || panics && recovered != fault || !panics && (err != fault || recovered != nil) {
					t.Fatalf("err = %v, panic = %v, stats/closes = %d/%d", err, recovered, d.stats, d.closes)
				}
			})
		}
	}
}

func TestBoundedParsePostOpenAppendAndPartialTail(t *testing.T) {
	withMaxLine(t, 128)
	for _, fixture := range boundedFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			// A long incomplete record must drain only to the admitted extent.
			body := fixture.body + `{"type":"unknown","content":"` + strings.Repeat("z", 100<<10)
			d, snap := boundedDescriptorFixture(t, fixture.path, body)
			d.read = func(p []byte, off int64) (int, error) {
				if d.reads == 1 {
					// Append after opening Stat and before the first read. Neither
					// the drain nor Codex marker search may reach the suffix.
					d.data = strings.NewReader(body + "\"}\n" + fixture.append)
				}
				return d.data.ReadAt(p, off)
			}
			d.stat = func(n int) (os.FileInfo, error) {
				if n > 1 {
					return boundedSizedInfo{d.info, d.data.Size()}, nil
				}
				return d.info, nil
			}
			s, err := boundedParseWithOpen(snap, snap.Size+1024, d.open)
			if err != nil || s == nil || len(s.Turns) != 1 || s.Turns[0].Prompt != "first" || s.Skipped != 0 || s.Turns[0].Skipped != 0 {
				t.Fatalf("session = %+v, err = %v", s, err)
			}
			for _, end := range d.ends {
				if end > snap.Size {
					t.Fatalf("read escaped extent: %d > %d", end, snap.Size)
				}
			}
			if d.closes != 1 {
				t.Fatalf("closes = %d", d.closes)
			}
		})
	}
}

func TestBoundedParseCodexLegitimateEarlyReturn(t *testing.T) {
	prefix := `{"type":"session_meta","payload":{"id":"invented","source":{"subagent":"invented"}}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"user_message","message":"first"}}` + "\n"
	body := prefix + strings.Repeat(" ", 2<<20)
	d, snap := boundedDescriptorFixture(t, "rollout-subagent.jsonl", body)
	d.read = func(p []byte, off int64) (int, error) {
		if off >= 1<<20 {
			t.Fatal("unnecessary drain after legitimate subagent return")
		}
		return d.data.ReadAt(p, off)
	}
	d.closeErr = errors.New("invented close error") // ignored, as by legacy wrappers
	s, err := boundedParseWithOpen(snap, snap.Size, d.open)
	if s != nil || err != nil || d.stats != 2 || d.closes != 1 {
		t.Fatalf("session = %+v, err = %v, stats/closes = %d/%d", s, err, d.stats, d.closes)
	}
}

func TestReaderFallbackCompatibility(t *testing.T) {
	for _, fixture := range boundedFixtures()[:2] {
		for _, timestamp := range []bool{false, true} {
			for _, empty := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/time=%t/empty=%t", fixture.name, timestamp, empty), func(t *testing.T) {
					body := fixture.body
					if timestamp {
						field := "timestamp"
						if fixture.name == "agy" {
							field = "created_at"
						}
						body = strings.Replace(body, "{", `{"`+field+`":"2026-01-02T03:04:05Z",`, 1)
					}
					if empty {
						body = ""
					}
					d, snap := boundedDescriptorFixture(t, fixture.path, body)
					calls := 0
					fallback := func() time.Time {
						calls++
						return descriptorMtime(func() (os.FileInfo, error) { return nil, errors.New("invented fallback failure") })
					}
					var s *Session
					var err error
					if fixture.name == "agy" {
						s, err = parseAgyReader(snap.Path, strings.NewReader(body), fallback)
					} else {
						s, err = parseClaudeReader(snap.Path, strings.NewReader(body), fallback)
					}
					wantCalls := 1
					if timestamp && !empty {
						wantCalls = 0
					}
					if err != nil || s == nil || calls != wantCalls || s.Updated.IsZero() != (wantCalls == 1) {
						t.Fatalf("session = %+v, err = %v, fallback calls = %d", s, err, calls)
					}
					// Public wrapper and actual bounded adapter share the same
					// body, IDs and output, including empty-file metadata fallback.
					legacy, err := fixture.parse(snap.Path)
					if err != nil {
						t.Fatal(err)
					}
					bounded, err := boundedParse(snap, snap.Size)
					if err != nil || !reflect.DeepEqual(legacy, bounded) {
						t.Fatalf("legacy/bounded differ: %+v / %+v, err = %v", legacy, bounded, err)
					}
					if wantCalls == 1 && !legacy.Updated.Equal(d.info.ModTime()) {
						t.Fatalf("legacy fallback = %v, want descriptor mtime", legacy.Updated)
					}
				})
			}
		}
	}
}

func TestReaderFallbackUsesFinalOpenedDescriptor(t *testing.T) {
	for _, fixture := range boundedFixtures()[:2] {
		t.Run(fixture.name, func(t *testing.T) {
			_, snap := boundedDescriptorFixture(t, fixture.path, fixture.body)
			f, err := OpenRegular(snap.Path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			fallback := func() time.Time {
				// Alter mtime only when the body has finished, then replace the
				// pathname. Fallback must observe the old descriptor's final time.
				final := time.Unix(1_850_000_000, 0)
				if err := os.Chtimes(snap.Path, final, final); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(snap.Path, snap.Path+".old"); err != nil {
					t.Fatal(err)
				}
				writeFile(t, snap.Path, "{}\n")
				return descriptorMtime(f.Stat)
			}
			var s *Session
			if fixture.name == "agy" {
				s, err = parseAgyReader(snap.Path, f, fallback)
			} else {
				s, err = parseClaudeReader(snap.Path, f, fallback)
			}
			if err != nil || s == nil || !s.Updated.Equal(time.Unix(1_850_000_000, 0)) {
				t.Fatalf("session = %+v, err = %v", s, err)
			}
		})
	}
}

func TestBoundedParseOpenedDescriptorSurvivesPathReplacement(t *testing.T) {
	fixture := boundedFixtures()[0]
	_, snap := boundedDescriptorFixture(t, fixture.path, fixture.body)
	s, err := boundedParseWithOpen(snap, snap.Size, func(path string) (boundedDescriptor, error) {
		f, err := OpenRegular(path)
		if err != nil {
			return nil, err
		}
		if err := os.Rename(path, path+".old"); err != nil {
			f.Close()
			t.Fatal(err)
		}
		writeFile(t, path, fixture.append)
		return f, nil
	})
	if err != nil || s == nil || s.Turns[0].Prompt != "first" || s.Updated.UnixNano() != snap.Mtime {
		t.Fatalf("session = %+v, err = %v", s, err)
	}
}

func TestBoundedParseCacheRecoveryClosesBeforeQuarantine(t *testing.T) {
	fixture := boundedFixtures()[0]
	d, snap := boundedDescriptorFixture(t, fixture.path, fixture.body)
	d.read = func([]byte, int64) (int, error) { panic("invented private panic payload") }
	c := newCache(defaultCacheConfig)
	// This explicitly composes descriptor faults with the existing cache test
	// hook; separate production tests cover the default bounded adapter.
	c.parse = func(string) (*Session, error) { return boundedParseWithOpen(snap, snap.Size, d.open) }
	_, err := c.runParse(snap)
	if !errors.Is(err, ErrUnreadable) || strings.Contains(err.Error(), "invented private panic payload") || d.closes != 1 {
		t.Fatalf("err = %v, closes = %d", err, d.closes)
	}
	c.mu.Lock()
	quarantined := c.quarantine[snap.Path]
	c.mu.Unlock()
	if quarantined.Snapshot != snap {
		t.Fatalf("quarantined snapshot = %+v, want original", quarantined)
	}
	if _, _, err := c.Session(context.Background(), snap.Path); !errors.Is(err, ErrUnreadable) || d.closes != 1 {
		t.Fatalf("quarantine lookup: err = %v, closes = %d", err, d.closes)
	}
}

func TestBoundedParseCanceledWaiterKeepsDescriptorAndSlot(t *testing.T) {
	for _, outcome := range []string{"session", "no-session", "validation", "provider-error", "panic"} {
		t.Run(outcome, func(t *testing.T) {
			fixture := boundedFixtures()[0]
			if outcome == "no-session" {
				fixture.path, fixture.body = "rollout-empty.jsonl", "{}\n"
			}
			d, snap := boundedDescriptorFixture(t, fixture.path, fixture.body)
			started, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			finish := func() { once.Do(func() { close(release) }) }
			defer finish()
			d.read = func(p []byte, off int64) (int, error) {
				if d.reads == 1 {
					close(started)
					<-release
				}
				switch outcome {
				case "panic":
					panic("invented descriptor fault")
				case "provider-error":
					return 0, errors.New("invented read failure")
				case "validation":
					return 0, io.EOF
				}
				return d.data.ReadAt(p, off)
			}
			cfg := defaultCacheConfig
			cfg.width = 1
			c := newCache(cfg)
			c.parse = func(string) (*Session, error) { return boundedParseWithOpen(snap, cfg.ceiling, d.open) }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { _, _, err := c.Session(ctx, snap.Path); result <- err }()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("bounded descriptor did not start reading")
			}
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("waiter error = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("waiter did not cancel")
			}
			// The worker is blocked inside ReadAt, so these observations are
			// synchronized and cannot race its subsequent Close.
			if d.closes != 0 || len(c.sem) != 1 {
				t.Fatalf("active descriptor closes = %d, occupied slots = %d", d.closes, len(c.sem))
			}
			finish()
			// Taking the only slot synchronizes with task cleanup on every
			// outcome, including panic: Close must already have happened.
			select {
			case c.sem <- struct{}{}:
			case <-time.After(time.Second):
				t.Fatal("completed bounded parse retained its slot")
			}
			defer func() { <-c.sem }()
			if d.closes != 1 {
				t.Fatalf("closes before slot reuse = %d", d.closes)
			}
			c.mu.Lock()
			defer c.mu.Unlock()
			if len(c.flights) != 0 {
				t.Fatalf("abandoned flight retained: %d", len(c.flights))
			}
			q, quarantined := c.quarantine[snap.Path]
			if quarantined != (outcome == "panic") || quarantined && q.Snapshot != snap {
				t.Fatalf("quarantine = %t, snapshot = %+v", quarantined, q)
			}
			el := c.full[snap.Path]
			if (el != nil) != (outcome == "session") {
				t.Fatalf("full entry present = %t for %s", el != nil, outcome)
			}
			if el != nil && el.Value.(*parsed).snap != snap {
				t.Fatal("result relabeled with a different snapshot")
			}
		})
	}
}

func TestBoundedParseShortReadNeverAdmitsRow(t *testing.T) {
	fixtures := []boundedFixture{boundedFixtures()[0], {name: "codex-no-id", path: "rollout-empty.jsonl", body: "{}\n"}}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			d, snap := boundedDescriptorFixture(t, fixture.path, fixture.body)
			d.read = func(p []byte, off int64) (int, error) {
				return copy(p, fixture.body[:len(fixture.body)-1]), io.EOF
			}
			c := newCache(defaultCacheConfig)
			c.parse = func(string) (*Session, error) { return boundedParseWithOpen(snap, c.cfg.ceiling, d.open) }
			if _, err := c.Summary(context.Background(), snap.Path); err != errBoundedInput {
				t.Fatalf("summary error = %v", err)
			}
			stats := c.Stats() // synchronization with terminal publication
			c.mu.Lock()
			quarantined := len(c.quarantine)
			c.mu.Unlock()
			if stats.Rows != 0 || stats.Parsed != 0 || quarantined != 0 || d.closes != 1 {
				t.Fatalf("stats = %+v, quarantine/closes = %d/%d", stats, quarantined, d.closes)
			}
			// Retrying healthy input at the same snapshot is allowed: no
			// negative row or quarantine was installed for the failed adapter.
			c.parse = nil // exercise the integrated production default on retry
			sum, err := c.Summary(context.Background(), snap.Path)
			want := error(nil)
			if fixture.name == "codex-no-id" {
				want = errNoSession
			}
			if err != want || want == nil && sum.ID == "" || c.Stats().Parses != 2 || c.Stats().Rows != 1 {
				t.Fatalf("healthy summary = %+v, err = %v, stats = %+v", sum, err, c.Stats())
			}
		})
	}
}
