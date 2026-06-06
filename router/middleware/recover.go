package middleware

import (
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/moostackhq/go/router"
)

// Recover returns middleware that catches panics in downstream
// handlers, logs them at Error level via [slog.Default], and writes
// a 500 response. Without it a panic crashes the per-connection
// goroutine and the client sees a closed connection with no body.
//
// Place Recover in the outermost position of the chain — i.e., as
// one of the first arguments to [router.Router.Use], since middleware
// composes outer-to-inner (see the [router.Middleware] doc). That way
// Recover covers every other middleware's panics too, not just the
// handler's.
//
// ErrAbortHandler — the stdlib's signal that the server should
// abandon the request — is detected with [errors.Is] (so a wrapped
// sentinel still triggers it) and re-panicked as the BARE
// [http.ErrAbortHandler]. The bare form is required because stdlib's
// http.Server identifies the abort with a `==` comparison; a wrapped
// error would slip past it and produce a regular 500. The wrapping
// context (if any) is dropped on the re-panic, on purpose.
func Recover() router.Middleware {
	return RecoverWith(slog.Default())
}

// RecoverWith is [Recover] with an explicit logger instance.
func RecoverWith(logger *slog.Logger) router.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				v := recover()
				if v == nil {
					return
				}
				if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					// Stdlib's "stop processing this request" signal.
					// Re-panic the BARE sentinel so the http.Server's
					// `==` check fires; wrapping context is dropped.
					panic(http.ErrAbortHandler)
				}
				attrs := []slog.Attr{
					slog.Any("panic", v),
					slog.String("stack", string(debug.Stack())),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
				}
				if id := GetRequestID(r.Context()); id != "" {
					attrs = append(attrs, slog.String("request_id", id))
				}
				logger.LogAttrs(r.Context(), slog.LevelError, "panic recovered", attrs...)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}()
			next.ServeHTTP(w, r)
		})
	}
}
