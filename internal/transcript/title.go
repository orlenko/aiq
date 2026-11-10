package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"syscall"
)

// OpenRegular opens a transcript for reading, and refuses anything but a regular
// file. A hook supplies these paths, and the daemon reads them: a pipe would block it
// on open and a device could feed it forever. Opening without blocking, then checking
// what was opened, leaves no gap between the check and the read.
func OpenRegular(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err == nil && !fi.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a regular file", path)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// ReadRegular reads a small file through OpenRegular, up to MaxLine bytes.
func ReadRegular(path string) ([]byte, error) {
	f, err := OpenRegular(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, int64(MaxLine)))
}

// MaxLine is the longest transcript record aiq reads, terminal and web alike.
// It is set high: the large records are tool results carrying images, whose
// content the parsers skip anyway. A parser reads past a longer record and
// counts it in Skipped, so whoever shows the session can say one was left out.
var MaxLine = 32 << 20

// readLine reads one line into *buf, which keeps its storage for the next.
// A line longer than MaxLine is read to its end without being kept past
// MaxLine bytes, and over is set: line is then only its opening. A long line
// the file ends inside, before its newline, comes back empty and not over: like
// a short partial line, which fails to decode, it is a record still being
// written, or cut off, and says nothing.
func readLine(r *bufio.Reader, buf *[]byte) (line []byte, over bool, err error) {
	b := (*buf)[:0]
	for {
		var chunk []byte
		chunk, err = r.ReadSlice('\n')
		if !over {
			if len(b)+len(chunk) > MaxLine {
				chunk, over = chunk[:max(MaxLine-len(b), 0)], true
			}
			b = append(grow(b, len(chunk)), chunk...)
		}
		if err != bufio.ErrBufferFull {
			*buf = b
			if over && err == io.EOF {
				return b[:0], false, err
			}
			return b, over, err
		}
	}
}

// grow makes room in b for n more bytes, at least doubling its capacity and
// at most to MaxLine, so a long line is copied a few times and not once for
// every quarter more of it.
func grow(b []byte, n int) []byte {
	if len(b)+n <= cap(b) {
		return b
	}
	g := make([]byte, len(b), max(min(2*cap(b), MaxLine), len(b)+n))
	copy(g, b)
	return g
}

// openingString is the first string field named key in the opening of a
// record too long to decode, or "". A JSON string cannot hold the field's
// unescaped form, so a match is a field of some object in the record; the
// caller says which keys are unambiguous.
func openingString(opening []byte, key string) string {
	_, rest, ok := bytes.Cut(opening, []byte(`"`+key+`":"`))
	if !ok {
		return ""
	}
	v, _, _ := cutString(rest)
	return v
}

// ownString is the string field named key of the opening's outermost object,
// or "". Unlike openingString it looks only at that object's own keys, so a
// payload that holds the same key in an object of its own cannot pass for the
// record. A field the opening does not reach reads as "".
func ownString(opening []byte, key string) string {
	v := ownValue(opening, key)
	if len(v) == 0 || v[0] != '"' {
		return ""
	}
	s, _, _ := cutString(v[1:])
	return s
}

// ownTrue says whether the opening's outermost object has key set to true.
func ownTrue(opening []byte, key string) bool {
	return bytes.HasPrefix(ownValue(opening, key), []byte("true"))
}

// ownValue is what follows key's colon in the opening's outermost object, or
// nil.
func ownValue(opening []byte, key string) []byte {
	depth := 0
	for i := 0; i < len(opening); i++ {
		switch opening[i] {
		case '{', '[':
			depth++
		case '}', ']':
			if depth--; depth <= 0 {
				return nil // the outermost object has ended
			}
		case '"':
			end := stringEnd(opening, i+1)
			if end < 0 {
				return nil
			}
			after := bytes.TrimLeft(opening[end+1:], " \t")
			if depth == 1 && len(after) > 0 && after[0] == ':' && string(opening[i+1:end]) == key {
				return bytes.TrimLeft(after[1:], " \t")
			}
			i = end
		}
	}
	return nil
}

// stringEnd is the index of the quote that closes the JSON string starting at
// from, or -1 when the opening ends first.
func stringEnd(b []byte, from int) int {
	for i := from; i < len(b); i++ {
		switch b[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}
	return -1
}

// ClaudeTitle reads the latest saved display name without parsing the
// conversation. A rename can occur long after the session's launch.
func ClaudeTitle(path string) string {
	f, err := OpenRegular(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	title := ""
	r := bufio.NewReader(f)
	var buf []byte
	for {
		line, over, err := readLine(r, &buf)
		if !over && bytes.Contains(line, []byte(`"custom-title"`)) {
			var rec struct {
				Type  string `json:"type"`
				Title string `json:"customTitle"`
			}
			if json.Unmarshal(line, &rec) == nil && rec.Type == "custom-title" {
				title = rec.Title
			}
		}
		if err != nil {
			if err != io.EOF {
				return ""
			}
			return title
		}
	}
}

// CodexTitle reads the last name recorded for id in the account's index.
// The index is shared with the real home by account overlays.
func CodexTitle(index, id string) string {
	return codexNames([]string{index})[id]
}
