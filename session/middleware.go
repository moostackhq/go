package session

import (
	"bufio"
	"context"
	"net"
	"net/http"
)

// Middleware is the session middleware. It attaches a per-request
// session to the context on the way in and finalises it just before
// the response headers are sent.
//
// The wrapped handler interacts with the session via the Manager's
// context-aware methods (Get, Save, Update, Destroy, Renew, Promote,
// SID, UserID). A request that does not pass through Middleware will
// get [ErrNoSession] from any of them. The identity lookups
// ([Manager.ListForUser], [Manager.RevokeAllForUser]) operate on a
// userID directly and do not require a wrapped request.
//
// Middleware is itself the middleware — pass the method value to
// your router (no parens):
//
//	r.Use(sessMgr.Middleware)
//
// Cookie writes happen as part of the commit, which is triggered by
// the first WriteHeader or Write on the response. If the handler
// returns without writing anything, the middleware finalises the
// session explicitly and emits a 200 OK so the cookie still reaches
// the client.
func (m *Manager[T]) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sid, _ := m.cfg.Token.Read(r)
		st := &state[T]{sid: sid}
		ctx := context.WithValue(r.Context(), m.ctxKey, st)

		cw := &committingWriter{
			ResponseWriter: w,
			commitFn: func(rw http.ResponseWriter) error {
				return m.commit(ctx, st, rw)
			},
		}
		next.ServeHTTP(cw, r.WithContext(ctx))

		// If the handler wrote nothing, force commit + an implicit
		// 200 so any Set-Cookie from the commit still leaves the
		// process.
		if !cw.headerWritten {
			cw.WriteHeader(http.StatusOK)
		}
	})
}

// committingWriter intercepts the response stream just long enough to
// run the session commit before any bytes leave for the client.
//
// The interface promotions for [http.Flusher], [http.Hijacker], and
// [http.Pusher] go through the underlying writer when it supports
// them; without these promotions a middleware would silently break
// SSE, WebSockets, and HTTP/2 push for downstream handlers.
type committingWriter struct {
	http.ResponseWriter
	commitFn      func(http.ResponseWriter) error
	headerWritten bool
	committed     bool
}

func (cw *committingWriter) WriteHeader(status int) {
	if cw.headerWritten {
		return
	}
	cw.runCommit()
	if cw.headerWritten {
		// runCommit only sets headerWritten when commit failed and
		// http.Error already wrote a 500. Don't issue a second
		// WriteHeader on the underlying writer — net/http would log
		// "superfluous response.WriteHeader call" and silently
		// ignore the status anyway.
		return
	}
	cw.headerWritten = true
	cw.ResponseWriter.WriteHeader(status)
}

func (cw *committingWriter) Write(b []byte) (int, error) {
	if !cw.headerWritten {
		cw.runCommit()
		cw.headerWritten = true
		// Defer to ResponseWriter's implicit 200 on first write so
		// we don't double-set the status.
	}
	return cw.ResponseWriter.Write(b)
}

func (cw *committingWriter) runCommit() {
	if cw.committed {
		return
	}
	cw.committed = true
	if err := cw.commitFn(cw.ResponseWriter); err != nil {
		// Headers haven't been written yet. Surface the failure as
		// a 500. Any subsequent handler Write will append to this
		// body, which is the expected trade-off — we cannot
		// pre-empt a handler that has already decided to respond.
		http.Error(cw.ResponseWriter, "session commit failed: "+err.Error(), http.StatusInternalServerError)
		cw.headerWritten = true
	}
}

// Flush implements [http.Flusher] if the underlying writer does.
func (cw *committingWriter) Flush() {
	if !cw.headerWritten {
		cw.runCommit()
		cw.headerWritten = true
	}
	if f, ok := cw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack implements [http.Hijacker] if the underlying writer does.
// The session is committed before the connection is taken over so a
// Set-Cookie can still land in any response the caller writes
// directly. After a successful Hijack the wrapper marks the response
// as written so [Manager.Middleware]'s tail path will not attempt a stray
// WriteHeader against a writer that no longer owns the connection.
//
// If the commit fails and surfaces a 500 to the client before Hijack
// runs, the connection is no longer hijackable and the method
// returns an error rather than handing the caller a writer that has
// already shipped bytes.
func (cw *committingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := cw.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	cw.runCommit()
	if cw.headerWritten {
		// runCommit only sets headerWritten when commit failed and
		// http.Error already wrote a 500. The hijack contract has
		// been broken by the in-flight response.
		return nil, nil, http.ErrHijacked
	}
	conn, brw, err := h.Hijack()
	if err == nil {
		cw.headerWritten = true
	}
	return conn, brw, err
}

// Push implements [http.Pusher] if the underlying writer does.
func (cw *committingWriter) Push(target string, opts *http.PushOptions) error {
	if p, ok := cw.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}
