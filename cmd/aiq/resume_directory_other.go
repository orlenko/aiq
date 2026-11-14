//go:build !linux && !darwin

package main

import "errors"

func openResumeDir(string) (resumeDirFD, error) {
	return nil, errors.New("resume directory examination is unavailable on this platform")
}
func statResumeDir(string) (resumeDirFacts, error) {
	return resumeDirFacts{}, errors.New("resume directory examination is unavailable on this platform")
}
