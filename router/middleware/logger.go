package middleware

import (
	"bufio"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/moostackhq/go/router"
)

// Logger returns middleware that emits a single Info-level slog
// record per request. Uses [slog.Default]; see [LoggerWith] for a
// custom logger.
//
// Logged attributes: method, path, status, dur, bytes, remote, and
// request_id when [RequestID] middleware ran earlier in the chain.
//
// Hijack-skip: Logger skips the log record when the handler
// hijacked the connection without first writing through the
// wrapper (otherwise it would log fictional "200 OK 0 bytes"
// values for what was really a WebSocket upgrade or similar).
//
// This works across the typical wrapping shapes — direct
// `w.(http.Hijacker)` and [http.NewResponseController] — because
// statusWriter satisfies [http.Hijacker] explicitly. Both lookup
// paths stop at statusWriter (controller tries the current writer
// before unwrapping; the type assertion finds statusWriter as the
// outermost concrete type that implements Hijacker). statusWriter
// then cascades to the inner writer and records the hijack flag.
//
// Theoretical sharp edge: if a future intermediate wrapper sits
// between statusWriter and the writer the handler sees, AND that
// wrapper does NOT implement Hijacker, AND the handler's Hijack
// resolution mechanism somehow skips statusWriter, the flag
// wouldn't be set and Logger would emit a fictional record. None
// of the built-in middleware trigger this; see the
// TestLoggerCompressChain_HijackSkipBehaviour regression for the
// canonical Logger→Compress→conn chain.
func Logger() router.Middleware {
	return LoggerWith(slog.Default())
}

// LoggerWith is [Logger] with an explicit logger instance.
func LoggerWith(logger *slog.Logger) router.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sw, r)

			// Hijacked + nothing written through the wrapper:
			// status / bytes from sw are fictional (the handler
			// wrote directly to the raw connection). Skip the
			// record rather than log "200 OK 0 bytes" lies.
			// WebSocket upgrades, CONNECT tunnels, and any
			// SSE-via-hijack handler land here.
			if sw.hijacked && !sw.wrote {
				return
			}

			attrs := []slog.Attr{
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", sw.status),
				slog.Duration("dur", time.Since(start)),
				slog.Int("bytes", sw.bytes),
				slog.String("remote", r.RemoteAddr),
			}
			if id := GetRequestID(r.Context()); id != "" {
				attrs = append(attrs, slog.String("request_id", id))
			}
			logger.LogAttrs(r.Context(), slog.LevelInfo, "http request", attrs...)
		})
	}
}

// statusWriter captures the response's status code and byte count
// so the Logger can include them. Handlers that never call
// WriteHeader explicitly default to 200 (matching stdlib semantics).
type statusWriter struct {
	http.ResponseWriter
	status   int
	bytes    int
	wrote    bool
	hijacked bool
}

func (s *statusWriter) WriteHeader(code int) {
	if !s.wrote {
		s.status = code
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if !s.wrote {
		// Implicit 200 from a Write without a prior WriteHeader.
		s.wrote = true
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// Unwrap returns the embedded ResponseWriter so [http.NewResponseController]
// can walk the chain and find optional interfaces on whatever is
// underneath. The explicit Flush / Hijack / Push forwarders below
// cover direct type assertions (`w.(http.Flusher)`); Unwrap covers
// the ResponseController path. Both styles need to work.
func (s *statusWriter) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}

// Flush / Hijack / Push forward to the underlying ResponseWriter if
// it supports them. Wrapping ResponseWriter opaquely would break
// SSE handlers, WebSocket upgrades, and HTTP/2 push.
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := s.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	conn, brw, err := h.Hijack()
	if err == nil {
		s.hijacked = true
	}
	return conn, brw, err
}

func (s *statusWriter) Push(target string, opts *http.PushOptions) error {
	if p, ok := s.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}
