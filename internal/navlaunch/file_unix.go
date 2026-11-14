//go:build linux || darwin

package navlaunch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// File holds the cooperative namespace lock until Finish. A successful or
// uncertain process start retains the document for asynchronous browser reads.
type File struct {
	dir, lock, file *os.File
	data, name      string
	identity        unix.Stat_t
	dirIdentity     unix.Stat_t
}

func stat(f *os.File) (unix.Stat_t, error) {
	var s unix.Stat_t
	err := unix.Fstat(int(f.Fd()), &s)
	return s, err
}
func private(s unix.Stat_t, kind uint32) bool {
	return uint32(s.Mode)&unix.S_IFMT == kind && int(s.Uid) == os.Getuid() && s.Mode&0077 == 0
}
func same(a, b unix.Stat_t) bool { return a.Dev == b.Dev && a.Ino == b.Ino }
func directoryAt(parent int, name string) (*os.File, error) {
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrUnavailable
	}
	return os.NewFile(uintptr(fd), name), nil
}

// A trusted sticky parent protects the next root/current-user entry from
// foreign replacement. Symlink ancestry and other mutable parents are refused.
func openData(ctx context.Context, data string) (*os.File, error) {
	if ctx == nil || ctx.Err() != nil || !filepath.IsAbs(data) || filepath.Clean(data) != data {
		return nil, ErrUnavailable
	}
	current, err := directoryAt(unix.AT_FDCWD, "/")
	if err != nil {
		return nil, ErrUnavailable
	}
	for _, part := range strings.Split(strings.TrimPrefix(data, "/"), "/") {
		if part == "" {
			continue
		}
		parent, e := stat(current)
		if e != nil || (parent.Uid != 0 && int(parent.Uid) != os.Getuid()) || (parent.Mode&0022 != 0 && parent.Mode&unix.S_ISVTX == 0) || ctx.Err() != nil {
			current.Close()
			return nil, ErrUnavailable
		}
		next, e := directoryAt(int(current.Fd()), part)
		current.Close()
		if e != nil {
			return nil, ErrUnavailable
		}
		child, e := stat(next)
		if e != nil || (child.Uid != 0 && int(child.Uid) != os.Getuid()) {
			next.Close()
			return nil, ErrUnavailable
		}
		current = next
	}
	s, e := stat(current)
	if e != nil || !private(s, unix.S_IFDIR) || ctx.Err() != nil {
		current.Close()
		return nil, ErrUnavailable
	}
	return current, nil
}

func openPrivate(parent int, name string) (*os.File, error) {
	flags := unix.O_RDWR | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
	fd, err := unix.Openat(parent, name, flags|unix.O_CREAT|unix.O_EXCL, 0600)
	if err == nil {
		if e := unix.Fchmod(fd, 0600); e != nil {
			unix.Close(fd)
			return nil, ErrUnavailable
		}
	} else if errors.Is(err, unix.EEXIST) {
		fd, err = unix.Openat(parent, name, flags, 0)
	}
	if err != nil {
		return nil, ErrUnavailable
	}
	f := os.NewFile(uintptr(fd), name)
	s, err := stat(f)
	if err != nil || !private(s, unix.S_IFREG) {
		f.Close()
		return nil, ErrUnavailable
	}
	return f, nil
}

func Prepare(ctx context.Context, data string) (_ *File, err error) {
	d, err := openData(ctx, data)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer d.Close()
	if e := unix.Mkdirat(int(d.Fd()), "ui-launch", 0700); e != nil && !errors.Is(e, unix.EEXIST) {
		return nil, ErrUnavailable
	}
	dir, err := directoryAt(int(d.Fd()), "ui-launch")
	if err != nil {
		return nil, ErrUnavailable
	}
	f := &File{dir: dir, data: data}
	defer func() {
		if err != nil {
			f.Finish(false)
		}
	}()
	f.dirIdentity, err = stat(dir)
	if err != nil || !private(f.dirIdentity, unix.S_IFDIR) {
		return nil, ErrUnavailable
	}
	f.lock, err = openPrivate(int(dir.Fd()), ".lock")
	if err != nil {
		return nil, ErrUnavailable
	}
	for {
		if ctx.Err() != nil {
			return nil, ErrUnavailable
		}
		e := unix.Flock(int(f.lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if e == nil {
			break
		}
		if !errors.Is(e, unix.EWOULDBLOCK) && !errors.Is(e, unix.EAGAIN) {
			return nil, ErrUnavailable
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ErrUnavailable
		case <-timer.C:
		}
	}
	if err = f.prune(ctx, time.Now()); err != nil {
		return nil, ErrUnavailable
	}
	var random [32]byte
	if _, err = rand.Read(random[:]); err != nil {
		return nil, ErrUnavailable
	}
	f.name = hex.EncodeToString(random[:]) + ".html"
	fd, e := unix.Openat(int(dir.Fd()), f.name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if e != nil {
		f.name = ""
		return nil, ErrUnavailable
	}
	f.file = os.NewFile(uintptr(fd), f.name)
	// Capture identity before any later failure can require cleanup.
	f.identity, err = stat(f.file)
	if err != nil {
		f.name = ""
		return nil, ErrUnavailable
	}
	if e = unix.Fchmod(fd, 0600); e != nil {
		return nil, ErrUnavailable
	}
	s, e := stat(f.file)
	if e != nil || !private(s, unix.S_IFREG) || s.Mode&0777 != 0600 || ctx.Err() != nil {
		return nil, ErrUnavailable
	}
	return f, nil
}

func canonicalName(name string) bool {
	if len(name) != 69 || !strings.HasSuffix(name, ".html") {
		return false
	}
	for _, c := range name[:64] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func (f *File) remove(name string, expected unix.Stat_t) {
	var now unix.Stat_t
	if unix.Fstatat(int(f.dir.Fd()), name, &now, unix.AT_SYMLINK_NOFOLLOW) == nil && same(now, expected) && private(now, unix.S_IFREG) {
		_ = unix.Unlinkat(int(f.dir.Fd()), name, 0)
	}
}
func (f *File) prune(ctx context.Context, now time.Time) error {
	for count := 0; count < 256; {
		if ctx.Err() != nil {
			return ErrUnavailable
		}
		names, err := f.dir.Readdirnames(32)
		if err != nil && err != io.EOF {
			return ErrUnavailable
		}
		for _, name := range names {
			count++
			if ctx.Err() != nil {
				return ErrUnavailable
			}
			if !canonicalName(name) {
				continue
			}
			fd, e := unix.Openat(int(f.dir.Fd()), name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if e != nil {
				continue
			}
			candidate := os.NewFile(uintptr(fd), name)
			s, e := stat(candidate)
			info, ie := candidate.Stat()
			if e == nil && ie == nil && private(s, unix.S_IFREG) && s.Mode&0777 == 0600 && !info.ModTime().After(now.Add(-10*time.Minute)) {
				buf := make([]byte, len(marker))
				n, re := io.ReadFull(candidate, buf)
				if re == nil && n == len(marker) && string(buf) == marker && ctx.Err() == nil {
					f.remove(name, s)
				}
			}
			candidate.Close()
		}
		if err == io.EOF {
			return nil
		}
	}
	return nil
}

func (f *File) Publish(ctx context.Context, body []byte) (string, error) {
	if f == nil || f.file == nil || ctx == nil || ctx.Err() != nil || len(body) > 65536 || !strings.HasPrefix(string(body), marker) {
		return "", ErrUnavailable
	}
	n, err := f.file.Write(body)
	if err != nil || n != len(body) {
		return "", ErrUnavailable
	}
	if f.file.Sync() != nil {
		return "", ErrUnavailable
	}
	err = f.file.Close()
	f.file = nil
	if err != nil || ctx.Err() != nil {
		return "", ErrUnavailable
	}
	// Recheck the external pathname against the anchored directory and file.
	d, e := openData(ctx, f.data)
	if e != nil {
		return "", ErrUnavailable
	}
	defer d.Close()
	var s unix.Stat_t
	if unix.Fstatat(int(d.Fd()), "ui-launch", &s, unix.AT_SYMLINK_NOFOLLOW) != nil || !same(s, f.dirIdentity) || !private(s, unix.S_IFDIR) {
		return "", ErrUnavailable
	}
	if unix.Fstatat(int(f.dir.Fd()), f.name, &s, unix.AT_SYMLINK_NOFOLLOW) != nil || !same(s, f.identity) || !private(s, unix.S_IFREG) {
		return "", ErrUnavailable
	}
	return filepath.Join(f.data, "ui-launch", f.name), nil
}

// Finish is idempotent. Only the captured inode may be removed; a replacement
// entry is left alone. The stable lock entry itself is never removed.
func (f *File) Finish(retain bool) {
	if f == nil || f.dir == nil {
		return
	}
	if f.file != nil {
		f.file.Close()
		f.file = nil
	}
	if !retain && f.name != "" {
		f.remove(f.name, f.identity)
	}
	if f.lock != nil {
		f.lock.Close()
		f.lock = nil
	}
	f.dir.Close()
	f.dir = nil
}
