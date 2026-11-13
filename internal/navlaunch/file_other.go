//go:build !linux && !darwin

package navlaunch

import "context"

type File struct{}

func Prepare(context.Context, string) (*File, error)          { return nil, ErrUnavailable }
func (*File) Publish(context.Context, []byte) (string, error) { return "", ErrUnavailable }
func (*File) Finish(bool)                                     {}
