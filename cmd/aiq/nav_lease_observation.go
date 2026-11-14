package main

import (
	"context"
	"math"

	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/terminal"
)

func navLeaseLaunchID(ctx context.Context, snapshot *state.NavigatorSnapshot, lease *state.NavigatorLease, allowUnknownSID bool) int64 {
	if ctx == nil || snapshot == nil || lease == nil || lease.StartedAt <= 0 || lease.StartedAt > navConversationSafeInteger/1000 {
		return 0
	}
	var id int64
	for _, v := range snapshot.Launches {
		if ctx.Err() != nil {
			return 0
		}
		if v.Mode != state.ModeLong || v.Provider != lease.Provider || v.LeaseID != lease.ID || v.AccountID != lease.AccountID || !navSameDirectory(v.Cwd, lease.Cwd) || v.StartedMS/1000 != lease.StartedAt {
			continue
		}
		if v.SessionID != lease.SessionID && !(allowUnknownSID && lease.SessionID == "") {
			continue
		}
		if id != 0 {
			return 0
		}
		id = v.ID
	}
	return id
}

func (f *navFactory) observeNavLease(ctx context.Context, snapshot *state.NavigatorSnapshot, lease *state.NavigatorLease, launchID int64) (terminal.Observation, error) {
	if ctx == nil || snapshot == nil || lease == nil {
		return terminal.Unknown, nil
	}
	if err := ctx.Err(); err != nil {
		return terminal.Unknown, err
	}
	if lease.ID <= 0 || lease.PID <= 0 || lease.PID > math.MaxInt32 || lease.StartedAt <= 0 || launchID <= 0 || f.input.observe == nil || f.input.witness == nil || snapshot.HostIdentity() == "" {
		return terminal.Unknown, nil
	}
	observed := f.input.observe(ctx, terminal.ObservationTarget{Ref: lease.Pane, Workspace: lease.Workspace, PID: int(lease.PID), LeaseID: lease.ID, LaunchID: launchID})
	if err := ctx.Err(); err != nil {
		return terminal.Unknown, err
	}
	if observed != terminal.Running && observed != terminal.Ended {
		return terminal.Unknown, nil
	}
	match, err := f.input.witness(ctx, snapshot.HostIdentity(), *lease)
	if ctx.Err() != nil {
		return terminal.Unknown, ctx.Err()
	}
	if err != nil || !match {
		return terminal.Unknown, nil
	}
	return observed, nil
}
