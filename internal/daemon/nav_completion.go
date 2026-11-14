package daemon

import "sync"

type navCompletionKey struct{}

// navCompletion owns at most one code-owned slot leave, until the outer handler
// completes. It retains no request or diagnostic state and invokes outside mu.
type navCompletion struct {
	mu     sync.Mutex
	closed bool
	leave  func()
}

func (c *navCompletion) adopt(leave func()) bool {
	if c == nil || leave == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.leave != nil {
		return false
	}
	c.leave = leave
	return true
}

func (c *navCompletion) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	leave := c.leave
	c.leave = nil
	c.mu.Unlock()
	if leave != nil {
		leave()
	}
}
