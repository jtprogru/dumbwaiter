package proxy

import (
	"context"
	"net/http"
	"time"
)

// lookup bounds the whole search rather than giving every fallback a fresh
// timeout. Its context must stay alive after selection to stream the body,
// so a context.WithTimeout (which cannot remove its deadline) is unsuitable.
type lookup struct {
	cancel context.CancelCauseFunc
	timer  *time.Timer
}

func newLookup(r *http.Request, timeout time.Duration) (*http.Request, *lookup) {
	ctx, cancel := context.WithCancelCause(r.Context())
	l := &lookup{cancel: cancel}
	l.timer = time.AfterFunc(timeout, func() { cancel(context.DeadlineExceeded) })
	return r.WithContext(ctx), l
}

// finish stops the lookup clock before sending the selected response. If the
// callback already started, synchronously mark the timeout: Timer.Stop does
// not wait for an AfterFunc callback to finish, and a late 200 would be wrong.
func (l *lookup) finish() {
	if !l.timer.Stop() {
		l.cancel(context.DeadlineExceeded)
	}
}

func (l *lookup) close() {
	l.timer.Stop()
	l.cancel(nil)
}
