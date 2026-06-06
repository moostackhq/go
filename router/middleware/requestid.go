package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"

	"github.com/moostackhq/go/router"
)

// RequestIDHeader is the conventional header carrying a per-request
// correlation ID. RequestID middleware reads it (so an upstream
// load balancer's value is preserved across hops) and writes it
// (so clients and downstream services can correlate logs).
const RequestIDHeader = "X-Request-ID"

type ctxKey struct{ name string }

var requestIDKey = ctxKey{"request-id"}

// RequestID returns middleware that ensures every request has a
// unique ID. The ID is read from [RequestIDHeader] on the request
// if present (e.g., set by a load balancer); otherwise a fresh
// 16-byte hex ID is generated. The ID is:
//
//   - propagated to downstream handlers via the request context
//     (fetch with [GetRequestID]),
//   - echoed back as the [RequestIDHeader] response header.
//
// Place RequestID early in the chain so Logger / Recover can
// include the ID in their structured output.
func RequestID() router.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(RequestIDHeader)
			if id == "" {
				id = newRequestID()
			}
			w.Header().Set(RequestIDHeader, id)
			ctx := context.WithValue(r.Context(), requestIDKey, id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// GetRequestID returns the per-request ID set by [RequestID]
// middleware, or "" if RequestID hasn't run.
func GetRequestID(ctx context.Context) string {
	v, _ := ctx.Value(requestIDKey).(string)
	return v
}
