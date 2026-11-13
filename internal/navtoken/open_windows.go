package navtoken

import "io"

// Windows unlock stays unavailable until native validation is complete.
func openValidated(string) (io.ReadCloser, error) { return nil, ErrUnavailable }
