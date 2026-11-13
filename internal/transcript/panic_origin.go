package transcript

import "github.com/orlenko/aiq/internal/diagnostic"

// PanicOrigin extracts only the two closed diagnostic adapters. It never walks
// a caller's error graph or calls its formatting, Is or Unwrap methods.
func PanicOrigin(err error) diagnostic.Origin {
	if e, ok := err.(*unreadableError); ok && e != nil {
		return e.origin
	}
	return diagnostic.FromError(err)
}

// CapturePanic preserves either closed diagnostic carrier before inspecting a
// raw panic's stack. Call it immediately from the recovering defer. It never
// calls a caller's error methods or retains its error or panic value.
func CapturePanic(value any) diagnostic.Origin { return capturePanic(value) }

func capturePanic(value any) diagnostic.Origin {
	if e, ok := value.(*unreadableError); ok && e != nil && e.origin.Present() {
		return e.origin
	}
	return diagnostic.Capture(value)
}
