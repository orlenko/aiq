//go:build !darwin && !linux

package herdr

import "errors"

type Options struct {
	Core                    Core
	Workspace, Session, Dir string
	Args                    []string
	Ready                   func()
	Hook                    func(int, uint64, Hook) []byte
}

func Run(Options) (int, error) {
	return 1, errors.New("native Herdr supervision requires macOS or Linux")
}
func Reclaim(Record) (bool, error) {
	return false, errors.New("native Herdr supervision requires macOS or Linux")
}
