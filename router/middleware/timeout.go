package middleware

import (
	"context"
	"net/http"
	"time"

	"github.com/moostackhq/go/router"
)

// Timeout returns middleware that attaches a deadline to the
// request context. Handlers are expected to honour ctx.Done()
// during long-running work; Timeout does not interrupt the
// goroutine itself or stream a 504 response.
//
// In practice you rarely need to check ctx.Done() yourself: any
// context-aware library call propagates the deadline automatically
// — [database/sql] queries, [net/http.Client] requests, gRPC stubs,
// Redis clients, AWS SDK calls, and so on all check the request
// context and return promptly when it expires. Handlers that do
// nothing but call such libraries get cancellation for free.
//
// That second behaviour is [http.TimeoutHandler]'s job, but its
// implementation buffers the entire response body in memory so it
// can swap it for a 504 if the deadline fires — which breaks
// streaming handlers (SSE, chunked downloads, gzip) and adds an
// unbounded memory cost on large responses. Use Timeout when you
// want a deadline your handlers respect; use http.TimeoutHandler
// only when you can tolerate the buffering trade-off.
//
// d <= 0 disables the timeout (the middleware becomes a no-op),
// useful for swapping configurations across environments without
// changing the chain shape.
func Timeout(d time.Duration) router.Middleware {
	if d <= 0 {
		return identityMiddleware
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// identityMiddleware returns next unchanged — the no-op used by
// Timeout(d<=0) so toggling timeouts off doesn't allocate a fresh
// closure each call.
var identityMiddleware router.Middleware = func(next http.Handler) http.Handler { return next }
