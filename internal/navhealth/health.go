// Package navhealth makes a bounded, strict observation of an explicitly named
// local daemon. Success is not authentication; callers must still verify the
// navigator's key-bound hello, expected build and authenticated directory.
package navhealth

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/daemon"
	"github.com/orlenko/aiq/internal/navtoken"
)

var ErrUnavailable = errors.New("navigator daemon identity could not be confirmed")

// Check performs no discovery, file access, retry, proof action or logging.
// The earlier parent deadline bounds this operation within a command budget.
func Check(parent context.Context, listen string, record navtoken.Record, identity string) error {
	return check(parent, listen, record, identity, nil)
}

func single(header http.Header, name string) (string, bool) {
	count, value := 0, ""
	for key, values := range header {
		if strings.EqualFold(key, name) {
			count += len(values)
			if len(values) == 1 {
				value = values[0]
			}
		}
	}
	return value, count == 1
}

func check(parent context.Context, listen string, record navtoken.Record, identity string, transport http.RoundTripper) (err error) {
	if parent == nil || daemon.CheckListen(listen) != nil || record.Status() != navtoken.On || record.PID() <= 0 || len(identity) != 64 {
		return ErrUnavailable
	}
	for _, c := range identity {
		if !(c >= 'a' && c <= 'f' || c >= '0' && c <= '9') {
			return ErrUnavailable
		}
	}
	if trace := httptrace.ContextClientTrace(parent); trace != nil && trace.Got1xxResponse != nil {
		return ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	if ctx.Err() != nil {
		return ErrUnavailable
	}
	if transport == nil {
		transport = &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext, DisableCompression: true, DisableKeepAlives: true, MaxResponseHeaderBytes: 65536}
	}
	if closer, ok := transport.(interface{ CloseIdleConnections() }); ok {
		defer closer.CloseIdleConnections()
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, e := http.NewRequestWithContext(ctx, "GET", "http://"+listen+"/healthz", nil)
	if e != nil {
		return ErrUnavailable
	}
	response, e := client.Do(request)
	closed := false
	if response != nil && response.Body != nil {
		defer func() {
			if !closed {
				if closeErr := response.Body.Close(); closeErr != nil {
					err = ErrUnavailable
				}
			}
		}()
	}
	if e != nil || response == nil || response.Body == nil || response.StatusCode != http.StatusOK || len(response.Trailer) != 0 {
		return ErrUnavailable
	}
	for name, values := range response.Header {
		if strings.EqualFold(name, "Content-Encoding") {
			return ErrUnavailable
		}
		if strings.EqualFold(name, "Trailer") {
			for _, value := range values {
				if strings.TrimSpace(value) != "" {
					return ErrUnavailable
				}
			}
		}
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 4))
	closeErr := response.Body.Close()
	closed = true
	if readErr != nil || closeErr != nil || ctx.Err() != nil || string(body) != "ok\n" || len(response.Trailer) != 0 {
		return ErrUnavailable
	}
	got, one := single(response.Header, "X-Aiq-Daemon")
	if !one || got != identity {
		return ErrUnavailable
	}
	got, one = single(response.Header, "X-Aiq-Nav")
	want := "pid=" + strconv.FormatInt(record.PID(), 10) + "; start=" + strconv.FormatInt(record.StartedUnix(), 10) + "; nav=1"
	if !one || got != want || ctx.Err() != nil {
		return ErrUnavailable
	}
	return nil
}
