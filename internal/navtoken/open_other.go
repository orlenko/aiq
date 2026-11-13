//go:build !unix && !windows

package navtoken

import "io"

func openValidated(string) (io.ReadCloser, error) { return nil, ErrUnavailable }
