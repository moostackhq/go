package ratelimit

import (
	"net"
	"net/http"
	"strconv"
	"time"
)

// KeyFunc derives the rate-limit key from a request. Returning an
// empty key signals "cannot identify the client"; the middleware then
// applies its fail-open/closed policy rather than limiting a shared
// empty key.
type KeyFunc func(*http.Request) string

// KeyByIP keys on the request's RemoteAddr (host part only). It does
// NOT consult X-Forwarded-For or any other header — those are
// spoofable, and trusting them lets a client forge its key. Behind a
// trusted proxy, supply your own KeyFunc that reads the header you
// control.
func KeyByIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr // no port (e.g. test rigs) — use as-is
	}
	return host
}

// middleware holds the resolved config for one mount.
type middleware struct {
	limiter   *Limiter
	key       KeyFunc
	failOpen  bool
	onLimited http.Handler
}

// MiddlewareOption configures [Middleware].
type MiddlewareOption func(*middleware)

// WithKeyFunc sets how the client key is derived. Default: [KeyByIP].
func WithKeyFunc(k KeyFunc) MiddlewareOption {
	return func(m *middleware) { m.key = k }
}

// WithFailClosed makes the middleware deny requests (503) when the
// limiter errors or the key can't be derived. The default is
// fail-open (allow), which favors availability; use fail-closed for
// security-sensitive routes like login.
func WithFailClosed() MiddlewareOption {
	return func(m *middleware) { m.failOpen = false }
}

// WithLimitedHandler overrides the response for a throttled request.
// The default writes 429 with a short text body; RateLimit-* and
// Retry-After headers are already set when it runs.
func WithLimitedHandler(h http.Handler) MiddlewareOption {
	return func(m *middleware) { m.onLimited = h }
}

// Middleware returns an http middleware that limits requests through l,
// keyed per client. It plugs into any router taking
// func(http.Handler) http.Handler — apply it to a route or a group.
//
// On every request it sets RateLimit-Limit / RateLimit-Remaining /
// RateLimit-Reset; on a throttled request it also sets Retry-After and
// responds 429 (see [WithLimitedHandler]).
func Middleware(l *Limiter, opts ...MiddlewareOption) func(http.Handler) http.Handler {
	m := &middleware{
		limiter:  l,
		key:      KeyByIP,
		failOpen: true,
		onLimited: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		}),
	}
	for _, o := range opts {
		o(m)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := m.key(r)
			if key == "" {
				m.degraded(w, r, next)
				return
			}
			res, err := m.limiter.Allow(r.Context(), key)
			if err != nil {
				m.degraded(w, r, next)
				return
			}

			setRateLimitHeaders(w.Header(), res)
			if !res.Allowed {
				w.Header().Set("Retry-After", strconv.Itoa(secondsCeil(res.RetryAfter)))
				m.onLimited.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// degraded applies the fail-open/closed policy when the client can't
// be identified or the limiter errors.
func (m *middleware) degraded(w http.ResponseWriter, r *http.Request, next http.Handler) {
	if m.failOpen {
		next.ServeHTTP(w, r)
		return
	}
	http.Error(w, "rate limiter unavailable", http.StatusServiceUnavailable)
}

func setRateLimitHeaders(h http.Header, res Result) {
	h.Set("RateLimit-Limit", strconv.Itoa(res.Limit))
	h.Set("RateLimit-Remaining", strconv.Itoa(res.Remaining))
	h.Set("RateLimit-Reset", strconv.Itoa(secondsCeil(res.ResetAfter)))
}

// secondsCeil rounds up to whole seconds (never below 1 for a positive
// duration), the granularity Retry-After / RateLimit-Reset use.
func secondsCeil(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return int((d + time.Second - 1) / time.Second)
}
