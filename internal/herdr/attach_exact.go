package herdr

import (
	"errors"
	"io"
)

var (
	ErrExactAttachUnavailable = errors.New("recorded native terminal unavailable")
	ErrExactAttachChanged     = errors.New("recorded native terminal changed")
)

// PrepareAttachExact observes one registered child, then returns an action
// which rechecks that child and uses the originally pinned Core. It never
// resolves a replacement by workspace or follows a generationless family.
func PrepareAttachExact(target, workspace string, pid int) (func() error, error) {
	family, generation, err := ParseRef(target)
	if err != nil || generation == 0 || workspace == "" || pid <= 0 {
		return nil, ErrExactAttachChanged
	}
	matches := func(r Record) bool {
		return r.Ref == family && r.Generation == generation && r.State == "running" &&
			r.PID == pid && r.Workspace == workspace && r.LeaseID > 0 && r.LaunchID > 0 &&
			r.Core.ID != "" && r.Core.Terminal != "" && r.Core.PID > 0 && r.Core.Socket != ""
	}
	r, err := Info(target)
	if err != nil {
		return nil, ErrExactAttachUnavailable
	}
	if !matches(r) {
		return nil, ErrExactAttachChanged
	}
	core := r.Core
	return func() error {
		current, err := Info(target)
		if err != nil {
			return ErrExactAttachUnavailable
		}
		if !matches(current) || current.Core.ID != core.ID || current.Core.Terminal != core.Terminal || current.Core.PID != core.PID {
			return ErrExactAttachChanged
		}
		// A later registry spelling cannot retarget this prepared endpoint.
		// Core verification still proves the original endpoint before viewing.
		if err := core.attachWithDiagnostics(io.Discard); err != nil {
			return ErrExactAttachUnavailable
		}
		return nil
	}, nil
}
