// Package navtoken reads an explicitly named navigator state file. It performs
// no discovery, logging, writes, network requests or daemon-liveness checks.
package navtoken

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/orlenko/aiq/internal/navauth"
)

const maxRecord = 128

var (
	ErrUnavailable = errors.New("navigator state unavailable")
	ErrUnsafe      = errors.New("navigator state is not a private regular file")
	ErrInvalid     = errors.New("navigator state is invalid")
)

type Status uint8

const (
	Invalid Status = iota
	On
	Off
	Refused
	Failed
)

// Record owns its metadata. Its secret exists only inside a parser-created,
// immutable key-copy closure; ordinary nested fmt cannot walk that environment.
type Record struct {
	status       Status
	pid, started int64
	port         uint16
	key          func() [32]byte
}

func (r Record) Status() Status     { return r.status }
func (r Record) PID() int64         { return r.pid }
func (r Record) StartedUnix() int64 { return r.started }
func (r Record) Port() (int, bool)  { return int(r.port), r.status == On && r.key != nil }
func (r Record) Key() ([32]byte, bool) {
	if r.status != On || r.key == nil {
		return [32]byte{}, false
	}
	return r.key(), true
}

const redacted = "[navigator token redacted]"

func (Record) String() string                 { return redacted }
func (Record) GoString() string               { return redacted }
func (Record) Format(state fmt.State, _ rune) { _, _ = io.WriteString(state, redacted) }
func (Record) MarshalJSON() ([]byte, error)   { return []byte(`"[navigator token redacted]"`), nil }

// Read returns shape-checked state, NOT authenticated or live daemon identity.
// Any failure returns a zero record and a fixed error without the path or cause.
func Read(path string) (Record, error) {
	if !utf8.ValidString(path) || strings.IndexByte(path, 0) >= 0 {
		return Record{}, ErrUnavailable
	}
	f, err := openValidated(path)
	if err != nil {
		return Record{}, err
	}
	return readFile(f)
}

func readFile(f io.ReadCloser) (out Record, err error) {
	defer func() {
		closeErr := f.Close()
		if err == nil && closeErr != nil {
			err = ErrUnavailable
		}
		if err != nil {
			out = Record{}
		}
	}()
	raw, readErr := io.ReadAll(io.LimitReader(f, maxRecord+1))
	if readErr != nil {
		return Record{}, ErrUnavailable
	}
	return parse(raw)
}

func decimal(text string) (int64, bool) {
	n, err := strconv.ParseInt(text, 10, 64)
	return n, err == nil && strconv.FormatInt(n, 10) == text
}
func parse(raw []byte) (Record, error) {
	if len(raw) == 0 || len(raw) > maxRecord || raw[len(raw)-1] != '\n' {
		return Record{}, ErrInvalid
	}
	fields := strings.Split(string(raw[:len(raw)-1]), " ")
	if len(fields) != 3 && len(fields) != 5 {
		return Record{}, ErrInvalid
	}
	var r Record
	switch fields[0] {
	case "on":
		r.status = On
	case "off":
		r.status = Off
	case "refused":
		r.status = Refused
	case "failed":
		r.status = Failed
	default:
		return Record{}, ErrInvalid
	}
	if (r.status == On) != (len(fields) == 5) {
		return Record{}, ErrInvalid
	}
	var ok bool
	r.pid, ok = decimal(fields[1])
	if !ok || r.pid <= 0 {
		return Record{}, ErrInvalid
	}
	r.started, ok = decimal(fields[2])
	if !ok {
		return Record{}, ErrInvalid
	}
	if r.status == On {
		port, valid := decimal(fields[3])
		if !valid || port < 1 || port > 65535 {
			return Record{}, ErrInvalid
		}
		r.port = uint16(port)
		key, err := navauth.Decode32(fields[4])
		if err != nil {
			return Record{}, ErrInvalid
		}
		owned := key
		r.key = func() [32]byte { return owned }
	}
	return r, nil
}
