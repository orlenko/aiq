package main

import (
	"github.com/orlenko/aiq/internal/herdr"
	"github.com/orlenko/aiq/internal/state"
)

// recordInteractiveTerminal notes the Herdr terminal an interactive session
// starts in, so the native app can go to it. It records only a terminal
// whose core answers and whose root process shares this process's TTY
// (herdr.CurrentCore); outside a direct Herdr terminal that check returns
// before any I/O. It writes nothing into the terminal, and a failure never
// stops the launch: the session is then simply not offered as a target.
func recordInteractiveTerminal(st *state.Store, lease state.Lease) {
	c, err := herdr.CurrentCore()
	if err != nil {
		return
	}
	_ = st.SetLeaseTerminal(state.LeaseTerminal{LeaseID: lease.ID, PID: int64(lease.PID), Socket: c.Socket, Core: c.ID, Address: c.Terminal, TerminalPID: int64(c.PID)})
}
