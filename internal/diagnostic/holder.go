package diagnostic

import (
	"context"
	"sync"
)

// Holder keeps only the first value observed by an attached operation. It has
// no reference back to its context, callbacks or a request. Zero is ready to use.
type Holder struct {
	mu    sync.Mutex
	first Origin
}

func (h *Holder) Note(origin Origin) {
	if !origin.Present() {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.first.Present() {
		h.first = origin
	}
}

func (h *Holder) Origin() Origin {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.first
}

type holderKey struct{}

// WithHolder installs a new optional observation sink. Detached work publishes
// Origin values; it must never retain this context or the caller's holder.
func WithHolder(parent context.Context) (context.Context, *Holder) {
	h := &Holder{}
	return context.WithValue(parent, holderKey{}, h), h
}

func Note(ctx context.Context, origin Origin) {
	if !origin.Present() {
		return
	}
	if h, ok := ctx.Value(holderKey{}).(*Holder); ok && h != nil {
		h.Note(origin)
	}
}
