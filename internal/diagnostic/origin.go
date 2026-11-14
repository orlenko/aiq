// Package diagnostic carries bounded, value-only panic origins. It does not log.
package diagnostic

import (
	"math"
	"runtime"
	"strings"
)

// Origin distinguishes no panic from a panic whose leaf could not be identified.
// Its zero value is absent. It retains neither a panic nor runtime stack objects.
type Origin struct {
	file   [128]byte
	line   uint32
	size   uint8
	reason uint8
}

const (
	unknown uint8 = iota + 1
	unsupported
	truncated
	known
)

func (o Origin) Present() bool { return o.reason != 0 }

// Location returns only an allowlisted repository-relative file and line.
func (o Origin) Location() (string, uint32, bool) {
	if o.reason != known {
		return "", 0, false
	}
	return string(o.file[:o.size]), o.line, true
}

// Capture must be called immediately in the recovering defer. The recovered
// value is never formatted or queried through an interface supplied by a caller.
func Capture(value any) (out Origin) {
	out = Origin{reason: unknown}
	defer func() {
		if recover() != nil {
			out = Origin{reason: unknown}
		}
	}()
	if f, ok := value.(*failure); ok && f != nil && f.origin.Present() {
		return f.origin
	}
	var pcs [64]uintptr
	n := runtime.Callers(1, pcs[:])
	if n == len(pcs) {
		return Origin{reason: truncated}
	}
	frames := runtime.CallersFrames(pcs[:n])
	var candidate runtime.Frame
	boundary, selected := false, false
	for count := 0; ; count++ {
		if count == 128 {
			return Origin{reason: truncated}
		}
		frame, more := frames.Next()
		if !boundary {
			boundary = panicBoundary(frame.Function)
		} else if !selected && !strings.HasPrefix(frame.Function, "runtime.") {
			candidate, selected = frame, true
		}
		if !more {
			break
		}
	}
	if !selected {
		return out
	}
	return fromFrame(candidate)
}

// Go's explicit, bounds/arithmetic and signal panics all pass gopanic; the
// finite trampoline spellings also cover frames emitted by supported runtimes.
func panicBoundary(function string) bool {
	switch function {
	case "runtime.gopanic", "runtime.sigpanic", "runtime.panicmem", "runtime.panicmemAddr",
		"runtime.panicdivide", "runtime.panicoverflow", "runtime.panicIndex", "runtime.panicIndexU",
		"runtime.goPanicIndex", "runtime.goPanicIndexU", "runtime.goPanicSliceAlen", "runtime.goPanicSliceAlenU",
		"runtime.goPanicSliceAcap", "runtime.goPanicSliceAcapU", "runtime.goPanicSliceB", "runtime.goPanicSliceBU":
		return true
	}
	return false
}

func fromFrame(f runtime.Frame) Origin {
	bad := Origin{reason: unsupported}
	if f.Line <= 0 || uint64(f.Line) > math.MaxUint32 {
		return bad
	}
	file := strings.ReplaceAll(f.File, "\\", "/")
	for _, dir := range []string{"cmd/aiq", "internal/transcript", "internal/daemon", "internal/diagnostic"} {
		pkg := "github.com/orlenko/aiq/" + dir + "."
		if !strings.HasPrefix(f.Function, pkg) && !(dir == "cmd/aiq" && strings.HasPrefix(f.Function, "main.")) {
			continue
		}
		at := strings.LastIndex(file, "/"+dir+"/")
		if at < 0 {
			return bad
		}
		rel := file[at+1:]
		name := strings.TrimPrefix(rel, dir+"/")
		if len(rel) > len(bad.file) || !strings.HasSuffix(name, ".go") || strings.Contains(name, "..") || name == ".go" {
			return bad
		}
		for _, c := range []byte(name) {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
				return bad
			}
		}
		o := Origin{line: uint32(f.Line), size: uint8(len(rel)), reason: known}
		copy(o.file[:], rel)
		return o
	}
	return bad
}

// Latch belongs to one serial operation. Unknown is a first origin too.
// Callers sharing an operation across goroutines must synchronize externally.
type Latch struct{ first Origin }

func (l *Latch) Note(o Origin) {
	if !l.first.Present() && o.Present() {
		l.first = o
	}
}
func (l *Latch) Origin() Origin { return l.first }
