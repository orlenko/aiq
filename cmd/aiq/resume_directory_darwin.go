package main

import "errors"

func (d *unixResumeDir) accessList(string) (bool, error) {
	return false, errors.New("private groups are confirmed on Linux only")
}
