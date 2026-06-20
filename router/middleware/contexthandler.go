package middleware

import (
	"context"
	"log/slog"
)

// requestIDLogKey is the attribute key the request ID is logged under —
// the same key Logger and Recover emit, so correlation holds across the
// access log, recovered panics, and application logs.
const requestIDLogKey = "request_id"

// ContextHandler wraps a [slog.Handler] so every record carries the
// request-scoped attributes stashed in its context — currently the
// request ID from [RequestID] middleware. Install it as the default
// handler:
//
//	base := slog.NewJSONHandler(os.Stdout, opts)
//	slog.SetDefault(slog.New(middleware.ContextHandler(base)))
//
// After that, slog.InfoContext(ctx, …) anywhere in a request is tagged
// with request_id automatically — no logger threading. Logs made outside
// a request (no ID in context) pass through unchanged.
//
// It is idempotent: it will not add request_id to a record that already
// has it, so [Logger] and [Recover], which add it themselves, don't
// double up.
func ContextHandler(next slog.Handler) slog.Handler {
	return contextHandler{Handler: next}
}

type contextHandler struct {
	slog.Handler
}

func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := GetRequestID(ctx); id != "" && !recordHasAttr(r, requestIDLogKey) {
		r.AddAttrs(slog.String(requestIDLogKey, id))
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs and WithGroup must re-wrap, otherwise the embedded handler's
// versions would return an unwrapped handler and drop context injection
// for any logger derived via With / WithGroup.
func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{Handler: h.Handler.WithGroup(name)}
}

func recordHasAttr(r slog.Record, key string) bool {
	found := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			found = true
			return false
		}
		return true
	})
	return found
}
