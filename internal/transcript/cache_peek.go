package transcript

// ParseIdentity is comparable and opaque. Retaining it keeps a small token,
// never the parsed session or its transcript text.
type ParseIdentity struct{ token *parseIdentity }
type parseIdentity struct{ mark byte } // Nonzero size keeps live tokens distinct.
func (i ParseIdentity) Valid() bool    { return i.token != nil }

// CachedSession observes an existing full parse without I/O, discovery, clocks,
// LRU mutation or starting/joining work. The immutable pointer distinguishes
// equal-stat parse incarnations. The transient held parse is conservatively
// omitted; callers must report unknown rather than reconstructing a miss.
func (c *Cache) CachedSession(path string) (*Session, Snapshot, bool) {
	if c == nil {
		return nil, Snapshot{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el := c.full[path]
	if el == nil {
		return nil, Snapshot{}, false
	}
	p := el.Value.(*parsed)
	if p.sess == nil {
		return nil, Snapshot{}, false
	}
	return p.sess, p.snap, true
}

// SessionIdentity correlates an actual Session result with its current parse
// incarnation. A transient held parse can have an identity without being an
// eligible CachedSession peek. This does not establish availability or liveness.
func (c *Cache) SessionIdentity(path string, snap Snapshot, selected *Session) (ParseIdentity, bool) {
	if c == nil || selected == nil {
		return ParseIdentity{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var p *parsed
	if el := c.full[path]; el != nil {
		p = el.Value.(*parsed)
	}
	if (p == nil || p.sess != selected || p.snap != snap) && c.held != nil && c.held.snap.Path == path {
		p = c.held
	}
	if p == nil || p.sess != selected || p.snap != snap || p.identity == nil {
		return ParseIdentity{}, false
	}
	return ParseIdentity{p.identity}, true
}
