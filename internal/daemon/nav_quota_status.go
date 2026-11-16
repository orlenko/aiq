package daemon

import "time"

// PollStatus is a copy of the daemon's poll bookkeeping: what /api/state shows
// as last_poll, polling, last_error and poll_interval. LastPoll is zero when no
// poll has finished since this daemon started.
type PollStatus struct {
	LastPoll        time.Time
	LastError       string
	Polling         bool
	IntervalSeconds int64
}

// PollStatus copies the poll bookkeeping under the server lock and does nothing
// else while holding it: no store read, no probe, no poll request.
func (s *Server) PollStatus() PollStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return PollStatus{LastPoll: s.lastPoll, LastError: s.lastErr, Polling: s.polling, IntervalSeconds: s.cfg.Poll.IntervalSeconds}
}
