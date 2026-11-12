// Package terminal is the small lifecycle seam shared by aiq's tmux and
// native Herdr long sessions. Provider routing remains in the existing runner.
package terminal

import (
	"errors"
	"fmt"

	"github.com/orlenko/aiq/internal/herdr"
	"github.com/orlenko/aiq/internal/tmux"
)

func Current() string { return herdr.Current() }

// State preserves unknown/query failure as an error. It is never evidence
// that a live session disappeared and can safely be replaced.
func State(ref string) (exists, running bool, err error) {
	if !herdr.IsRef(ref) {
		return tmux.PaneState(ref)
	}
	r, err := herdr.Info(ref)
	if errors.Is(err, herdr.ErrEnded) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if r.State == "lost" || r.State == "stopping" {
		return true, false, fmt.Errorf("Herdr session is %s; explicit recovery is required", r.State)
	}
	return true, r.PID > 0, nil
}

func Info(ref string) (tmux.Pane, error) {
	if !herdr.IsRef(ref) {
		return tmux.PaneInfo(ref)
	}
	r, err := herdr.Info(ref)
	if err != nil {
		return tmux.Pane{}, err
	}
	return tmux.Pane{ID: r.Ref, Session: r.Session, PID: r.PID, Dead: r.State == "ended", StartCommand: "native Herdr agent"}, nil
}

func Respawn(ref string, pid int, dir string, args []string) error {
	if herdr.IsRef(ref) {
		return herdr.Replace(ref, pid, dir, args)
	}
	return tmux.Respawn(ref, dir, tmux.Quote(args))
}

func Diagnostic(ref string) string {
	if !herdr.IsRef(ref) {
		return tmux.PaneDiag(ref) + " | screen: " + tmux.Tail(ref, 6)
	}
	r, err := herdr.Info(ref)
	if err != nil {
		return "Herdr state unavailable"
	}
	return fmt.Sprintf("Herdr %s, generation %d, pid %d", r.State, r.Generation, r.PID)
}
