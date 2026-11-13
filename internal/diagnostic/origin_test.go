package diagnostic

import (
	"errors"
	"fmt"
	"math"
	"runtime"
	"strings"
	"testing"

	"github.com/orlenko/aiq/internal/testenv"
)

func TestMain(m *testing.M) { testenv.Main(m) }

func caught(fn func()) (origin Origin) {
	defer func() {
		if v := recover(); v != nil {
			origin = Capture(v)
		}
	}()
	fn()
	return
}

func TestCaptureOriginalOperation(t *testing.T) {
	for _, test := range []struct {
		name string
		fn   func()
		line uint32
	}{
		{"explicit", explicitLeaf, 3}, {"nested-inline", nestedLeaf, 3},
		{"runtime-bounds", func() { boundsLeaf(2) }, 5}, {"nil-call", nilLeaf, 7},
	} {
		t.Run(test.name, func(t *testing.T) {
			o := caught(test.fn)
			file, line, known := o.Location()
			if !o.Present() || !known || file != "internal/diagnostic/leaf_test.go" || line != test.line {
				t.Fatalf("operation location = %q:%d known=%v present=%v", file, line, known, o.Present())
			}
		})
	}
}

func TestExternalLeafAndMissingBoundaryStayUnknown(t *testing.T) {
	for _, o := range []Origin{caught(func() { strings.Repeat("fixture", -1) }), Capture(nil)} {
		if _, _, known := o.Location(); !o.Present() || known {
			t.Fatal("external or missing boundary acquired a caller location")
		}
	}
}

//go:noinline
func deep(n int) {
	if n == 0 {
		explicitLeaf()
	}
	deep(n - 1)
}

func TestTruncatedStackIsUnknown(t *testing.T) {
	o := caught(func() { deep(100) })
	if _, _, known := o.Location(); !o.Present() || known || o.reason != truncated {
		t.Fatal("truncated stack attributed a leaf")
	}
}

type hostile struct{ calls *int }

func (h hostile) Error() string          { *h.calls++; panic("Error invoked") }
func (h hostile) String() string         { *h.calls++; panic("String invoked") }
func (h hostile) Unwrap() error          { *h.calls++; panic("Unwrap invoked") }
func (h hostile) Is(error) bool          { *h.calls++; panic("Is invoked") }
func (h hostile) Format(fmt.State, rune) { *h.calls++; panic("Format invoked") }

func TestHostileValuesAndTrustedRepanic(t *testing.T) {
	calls := 0
	o := caught(func() { panic(hostile{&calls}) })
	if !o.Present() || calls != 0 {
		t.Fatal("panic methods invoked or origin absent")
	}
	if FromError(hostile{&calls}).Present() || calls != 0 {
		t.Fatal("arbitrary error queried")
	}
	err := Failure(Unshowable, o)
	recaught := caught(func() { panic(err) })
	if recaught != o || calls != 0 {
		t.Fatal("re-panic changed first origin")
	}
	var nilCarrier *failure
	if FromError(nilCarrier).Present() {
		t.Fatal("nil carrier supplied an origin")
	}
	if !caught(func() { panic(nilCarrier) }).Present() {
		t.Fatal("typed nil panic was lost")
	}
}

func TestFrameAllowlistRequiresPackageAndSafeSource(t *testing.T) {
	good := runtime.Frame{Function: "main.operation", File: "/invented/build/cmd/aiq/operation.go", Line: 101}
	o := fromFrame(good)
	if f, l, k := o.Location(); !k || f != "cmd/aiq/operation.go" || l != 101 {
		t.Fatal("normal main spelling unsupported")
	}
	for _, bad := range []runtime.Frame{
		{Function: "main.operation", File: "/invented/build/other/operation.go", Line: 101},
		{Function: "external.operation", File: good.File, Line: 101},
		{Function: good.Function, File: "/invented/cmd/aiq/../operation.go", Line: 101},
		{Function: good.Function, File: "/invented/cmd/aiq/evil\n.go", Line: 101},
		{Function: good.Function, File: "/invented/cmd/aiq/" + strings.Repeat("x", 129) + ".go", Line: 101},
		{Function: good.Function, File: good.File, Line: 0},
	} {
		if _, _, known := fromFrame(bad).Location(); known {
			t.Fatal("unsafe or mismatched frame accepted")
		}
	}
	tooLarge := uint64(math.MaxUint32) + 1
	if uint64(int(tooLarge)) == tooLarge {
		good.Line = int(tooLarge)
		if _, _, known := fromFrame(good).Location(); known {
			t.Fatal("overflowed line accepted")
		}
	}
}

func TestFixedCarrierCompatibilityAndFirstUnknown(t *testing.T) {
	if Failure(Unshowable, Origin{}) != Base(Unshowable) {
		t.Fatal("ordinary sentinel identity changed")
	}
	if Base(Projection).Error() != "conversation rendering failed" || Base(Unshowable).Error() != "transcript: could not be shown" {
		t.Fatal("legacy error text changed")
	}
	o := caught(explicitLeaf)
	for _, kind := range []FailureKind{Projection, Unshowable, 255} {
		err := Failure(kind, o)
		if !errors.Is(err, Base(kind)) || err.Error() != Base(kind).Error() || FromError(err) != o {
			t.Fatal("carrier classification/text/origin changed")
		}
		if strings.Contains(err.Error(), "invented") || strings.Contains(err.Error(), ".go") {
			t.Fatal("error text acquired private details")
		}
	}
	var latch Latch
	first := Origin{reason: unknown}
	latch.Note(first)
	latch.Note(o)
	latch.Note(Origin{})
	if latch.Origin() != first {
		t.Fatal("unknown first origin was replaced")
	}
	if (Origin{}).Present() {
		t.Fatal("zero origin is not absent")
	}
}
