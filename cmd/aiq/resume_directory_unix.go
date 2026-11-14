//go:build linux || darwin

package main

import "golang.org/x/sys/unix"

type unixResumeDir struct{ fd int }

func openResumeDir(path string) (resumeDirFD, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return &unixResumeDir{fd}, nil
}
func resumeStatFacts(s unix.Stat_t) resumeDirFacts {
	return resumeDirFacts{directory: s.Mode&unix.S_IFMT == unix.S_IFDIR, uid: s.Uid, gid: s.Gid, mode: uint32(s.Mode) & 07777}
}
func statResumeDir(path string) (resumeDirFacts, error) {
	var s unix.Stat_t
	if err := unix.Stat(path, &s); err != nil {
		return resumeDirFacts{}, err
	}
	return resumeStatFacts(s), nil
}
func (d *unixResumeDir) facts() (resumeDirFacts, error) {
	var s unix.Stat_t
	if err := unix.Fstat(d.fd, &s); err != nil {
		return resumeDirFacts{}, err
	}
	return resumeStatFacts(s), nil
}
func (d *unixResumeDir) enter() error { return unix.Fchdir(d.fd) }
func (d *unixResumeDir) close() error { return unix.Close(d.fd) }
