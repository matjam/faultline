package adminhttp

import (
	"context"
	"time"
)

// contextWithTimeout is a tiny indirection that lets tests stub the
// timeout without touching context.WithTimeout directly. Production
// implementation is a thin wrapper.
var contextWithTimeout = func(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, d)
}
