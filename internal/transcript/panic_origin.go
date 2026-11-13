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

func capturePanic(value any) diagnostic.Origin {
	if e, ok := value.(*unreadableError); ok && e != nil && e.origin.Present() {
		return e.origin
	}
	return diagnostic.Capture(value)
}
