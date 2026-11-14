package daemon

import (
	"context"
	"net/http"
)

type navInputKey struct{}

// This owner must be acquired at the first HTTP handler boundary, before any
// request copy. net/http publishes chunk trailers on its ORIGINAL request.
// The creator clears the pointer at completion; borrowers never reopen it.
type navInput struct{ original *http.Request }

func ownNavInput(r *http.Request) (*navInput, bool) {
	if r == nil {
		return nil, false
	}
	ctx := r.Context()
	if existing := ctx.Value(navInputKey{}); existing != nil {
		owner, _ := existing.(*navInput)
		return owner, false
	}
	if len(r.TransferEncoding) == 0 {
		return nil, false
	}
	owner := &navInput{original: r}
	*r = *r.WithContext(context.WithValue(ctx, navInputKey{}, owner))
	return owner, true
}
func (o *navInput) close() {
	if o != nil {
		o.original = nil
	}
}
func (o *navInput) open(r *http.Request) bool {
	return o != nil && o.original != nil && r != nil && r.Context().Value(navInputKey{}) == o
}
func (o *navInput) trailersEmpty(r *http.Request) bool {
	return o.open(r) && len(o.original.Trailer) == 0 && len(r.Trailer) == 0
}
