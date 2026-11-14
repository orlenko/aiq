package diagnostic

import "errors"

// FailureKind is a closed classification, never an arbitrary wrapped cause.
type FailureKind uint8

const (
	Projection FailureKind = iota + 1
	Unshowable
	Unreadable
	Inspection
)

var (
	projectionBase = errors.New("conversation rendering failed")
	unshowableBase = errors.New("transcript: could not be shown")
	unreadableBase = errors.New("transcript could not be read")
	inspectionBase = errors.New("transcript: cached inspection failed")
	invalidBase    = errors.New("diagnostic failure")
)

func Base(kind FailureKind) error {
	switch kind {
	case Projection:
		return projectionBase
	case Unshowable:
		return unshowableBase
	case Unreadable:
		return unreadableBase
	case Inspection:
		return inspectionBase
	default:
		return invalidBase
	}
}

type failure struct {
	kind   FailureKind
	origin Origin
}

func (f *failure) Error() string { return Base(f.kind).Error() }
func (f *failure) Unwrap() error { return Base(f.kind) }

// Failure returns the original singleton for a nonpanic failure.
func Failure(kind FailureKind, origin Origin) error {
	if !origin.Present() {
		return Base(kind)
	}
	return &failure{kind: kind, origin: origin}
}

// FromError inspects only this package's concrete carrier, never a caller's
// Error, Unwrap, Is, String or Format methods or an arbitrary error graph.
func FromError(err error) Origin {
	if f, ok := err.(*failure); ok && f != nil {
		return f.origin
	}
	return Origin{}
}
