package transcript

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeTitleSkipsLargeTurnsAndPartialRecords(t *testing.T) {
	p := filepath.Join(t.TempDir(), "session.jsonl")
	data := `{"type":"custom-title","customTitle":"Harp"}` + "\n" +
		`{"type":"assistant","message":{"content":"` + strings.Repeat("x", 2<<20) + `"}}` + "\n" +
		`{"type":"custom-title","customTitle":`
	if err := os.WriteFile(p, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if got := ClaudeTitle(p); got != "Harp" {
		t.Fatalf("title = %q", got)
	}
	if got := ClaudeTitle(p + ".missing"); got != "" {
		t.Fatalf("missing title = %q", got)
	}
}

// readLine keeps a line of MaxLine bytes, newline and all, and is over at one
// more; *buf keeps the storage it grew for the next line.
func TestReadLineCap(t *testing.T) {
	withMaxLine(t, 100<<10)
	exact := strings.Repeat("x", MaxLine-1) + "\n"
	more := strings.Repeat("x", MaxLine) + "\n"
	r := bufio.NewReaderSize(strings.NewReader(exact+more+"short\n"), 1<<16)
	var buf []byte
	if line, over, err := readLine(r, &buf); string(line) != exact || over || err != nil {
		t.Fatalf("exactly MaxLine: %d bytes, over %v, %v", len(line), over, err)
	}
	kept := &buf[:1][0]
	if line, over, err := readLine(r, &buf); string(line) != more[:MaxLine] || !over || err != nil {
		t.Fatalf("one more: %d bytes, over %v, %v", len(line), over, err)
	}
	if line, over, err := readLine(r, &buf); string(line) != "short\n" || over || err != nil || &buf[:1][0] != kept {
		t.Fatalf("next: %q, over %v, %v; storage kept %v", line, over, err, &buf[:1][0] == kept)
	}
}

// A long line is read with a few copies of what it holds, and not dozens.
func TestReadLineGrowsGeometrically(t *testing.T) {
	data := []byte(strings.Repeat("x", 16<<20) + "\n")
	allocs := testing.AllocsPerRun(3, func() {
		var buf []byte
		line, over, err := readLine(bufio.NewReaderSize(bytes.NewReader(data), 1<<16), &buf)
		if len(line) != len(data) || over || err != nil {
			t.Fatalf("%d bytes, over %v, %v", len(line), over, err)
		}
	})
	if allocs > 16 {
		t.Errorf("%v allocations for one 16 MB line", allocs)
	}
}
