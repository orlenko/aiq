package herdr

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strconv"
)

// TargetLabel is the terminal label that names one exact aiq child. The
// native app focuses a terminal only when its live address, root pid and this
// label all match a target the bridge has just verified.
const TargetLabel = "aiq.target"

// WitnessLabel is TargetLabel's value for one child generation: random per
// terminal (the ref is), different for every generation, and not the ref
// itself. A terminal on another core cannot carry it; a kept replacement
// shell keeps the label but not the root pid.
func WitnessLabel(ref string, generation uint64) string {
	sum := sha256.Sum256([]byte("aiq-herdr-target-v1\x00" + ref + "@" + strconv.FormatUint(generation, 10)))
	return hex.EncodeToString(sum[:16])
}

// Target is a verified, exact terminal for one child: what the native app
// compares before it focuses anything. Socket paths never leave this package.
type Target struct {
	CoreID   string
	Terminal string
	PID      int
	Label    string
}

// Reasons a target is not exported.
const (
	TargetEnded        = "ended"
	TargetUnknown      = "unknown"
	TargetLabelMissing = "label_missing"
)

// ResolveTarget verifies through ref's pinned core, read-only, that the exact
// child generation in ref still runs in a live terminal at the recorded
// address, with the recorded root pid and its witness label. It starts no
// core and changes nothing. On failure it returns one of the reasons above.
func ResolveTarget(ref string) (Target, string) {
	family, generation, err := ParseRef(ref)
	if err != nil || generation == 0 {
		return Target{}, TargetUnknown
	}
	r, err := load(family)
	if errors.Is(err, ErrEnded) {
		return Target{}, TargetEnded
	}
	if err != nil {
		return Target{}, TargetUnknown
	}
	if r.Generation != generation {
		return Target{}, TargetEnded // this child was replaced
	}
	if r.State != "running" || !terminalID.MatchString(r.Core.Terminal) || r.Core.ID == "" || r.Core.PID <= 0 {
		return Target{}, TargetUnknown
	}
	return verifyTarget(r.Core, WitnessLabel(family, generation))
}

func verifyTarget(c Core, label string) (Target, string) {
	var hello struct {
		Generation int `json:"generation"`
		Core       struct {
			ID string `json:"id"`
		} `json:"core"`
	}
	if err := exchange(c.Socket, "hello", map[string]any{}, &hello, coreTimeout); err != nil {
		return Target{}, TargetUnknown
	}
	if hello.Generation != 1 || hello.Core.ID == "" || hello.Core.ID != c.ID {
		return Target{}, TargetUnknown
	}
	var t struct {
		coreTerminal
		Labels map[string]string `json:"labels"`
	}
	if err := exchange(c.Socket, "terminal.get", map[string]any{"terminal": c.Terminal}, &t, coreTimeout); err != nil {
		return Target{}, TargetUnknown
	}
	if t.State != "live" || t.Address != c.Terminal || t.PID != c.PID {
		return Target{}, TargetEnded
	}
	if label != "" && t.Labels[TargetLabel] != label {
		return Target{}, TargetLabelMissing
	}
	return Target{CoreID: c.ID, Terminal: c.Terminal, PID: c.PID, Label: label}, ""
}

// labelTarget writes this child's witness label. Labels merge in core, so the
// existing aiq.status label is kept. A failure leaves the target unexported
// (label_missing); it never stops the child.
func (c Core) labelTarget(ref string, generation uint64) error {
	return c.call("terminal.label", map[string]any{"terminal": c.Terminal, "labels": map[string]string{TargetLabel: WitnessLabel(ref, generation)}}, nil)
}

// VerifyTerminal checks, read-only, that core socket still has core id with
// a live terminal at address whose root process is pid. It is the check for
// a terminal aiq recorded but did not start (an interactive session), so
// there is no witness label: a replacement shell fails the pid check.
func VerifyTerminal(socket, id, address string, pid int) (Target, string) {
	c := Core{Socket: socket, ID: id, Terminal: address, PID: pid}
	if !filepath.IsAbs(socket) || id == "" || !terminalID.MatchString(address) || pid <= 0 {
		return Target{}, TargetUnknown
	}
	return verifyTarget(c, "")
}
