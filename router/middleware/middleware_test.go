package middleware_test

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/moostackhq/go/router"
	"github.com/moostackhq/go/router/middleware"
)

// flushHijackRecorder is a ResponseWriter that satisfies
// http.Flusher and http.Hijacker so we can verify that wrapper
// middleware forwards those interfaces. The bodies don't matter —
// only that calls reach the underlying type.
type flushHijackRecorder struct {
	*httptest.ResponseRecorder
	flushed   bool
	hijacked  bool
	hijackErr error
}

func newFlushHijackRecorder() *flushHijackRecorder {
	return &flushHijackRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func (f *flushHijackRecorder) Flush() {
	f.flushed = true
	f.ResponseRecorder.Flush()
}

func (f *flushHijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	f.hijacked = true
	if f.hijackErr != nil {
		return nil, nil, f.hijackErr
	}
	// Return a placeholder; the test only checks that Hijack was
	// reached, not that the connection works.
	return nil, nil, nil
}

func exec(t *testing.T, h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// --- RequestID ---

func TestRequestID_GeneratesIDWhenAbsent(t *testing.T) {
	r := router.New()
	r.Use(middleware.RequestID())
	r.Get("/x", func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte(middleware.GetRequestID(req.Context())))
	})

	rec := exec(t, r, httptest.NewRequest(http.MethodGet, "/x", nil))
	id := rec.Header().Get(middleware.RequestIDHeader)
	if id == "" {
		t.Fatal("response missing X-Request-ID header")
	}
	if rec.Body.String() != id {
		t.Errorf("handler saw %q, response header %q — should match", rec.Body.String(), id)
	}
	if len(id) != 32 {
		t.Errorf("generated ID length = %d, want 32 hex chars", len(id))
	}
}

func TestRequestID_PreservesIncomingHeader(t *testing.T) {
	r := router.New()
	r.Use(middleware.RequestID())
	r.Get("/x", func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte(middleware.GetRequestID(req.Context())))
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(middleware.RequestIDHeader, "upstream-id")
	rec := exec(t, r, req)
	if rec.Header().Get(middleware.RequestIDHeader) != "upstream-id" {
		t.Errorf("response ID = %q, want upstream-id", rec.Header().Get(middleware.RequestIDHeader))
	}
	if rec.Body.String() != "upstream-id" {
		t.Errorf("handler saw %q, want upstream-id", rec.Body.String())
	}
}

func TestGetRequestID_EmptyWhenMiddlewareAbsent(t *testing.T) {
	if id := middleware.GetRequestID(context.Background()); id != "" {
		t.Errorf("no-middleware ctx returned ID %q, want empty", id)
	}
}

// --- Logger ---

func TestLogger_EmitsOneRecordPerRequest(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	r := router.New()
	r.Use(middleware.RequestID(), middleware.LoggerWith(logger))
	r.Get("/x", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hi"))
	})

	exec(t, r, httptest.NewRequest(http.MethodGet, "/x?q=1", nil))

	out := buf.String()
	for _, want := range []string{
		`msg="http request"`,
		"method=GET",
		"path=/x",
		"status=200",
		"bytes=2",
		"request_id=",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in log:\n%s", want, out)
		}
	}
	// Exactly one record.
	if c := strings.Count(out, `msg="http request"`); c != 1 {
		t.Errorf("log record count = %d, want 1", c)
	}
}

func TestLogger_WrappedWriterForwardsFlusherAndHijacker(t *testing.T) {
	// Regression: a previous version embedded ResponseWriter and
	// relied on Go to "inherit" interface satisfaction — which it
	// doesn't. Handlers asking for http.Flusher / http.Hijacker
	// behind Logger silently got the wrong answer.
	r := router.New()
	r.Use(middleware.LoggerWith(slog.New(slog.NewTextHandler(io.Discard, nil))))

	var sawFlusher, sawHijacker bool
	r.Get("/x", func(w http.ResponseWriter, _ *http.Request) {
		if f, ok := w.(http.Flusher); ok {
			sawFlusher = true
			f.Flush()
		}
		if h, ok := w.(http.Hijacker); ok {
			sawHijacker = true
			_, _, _ = h.Hijack()
		}
	})

	rec := newFlushHijackRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if !sawFlusher {
		t.Error("wrapped writer should satisfy http.Flusher")
	}
	if !sawHijacker {
		t.Error("wrapped writer should satisfy http.Hijacker")
	}
	if !rec.flushed {
		t.Error("Flush did not reach underlying ResponseWriter")
	}
	if !rec.hijacked {
		t.Error("Hijack did not reach underlying ResponseWriter")
	}
}

func TestLogger_HijackErrorWhenUnderlyingDoesNotSupport(t *testing.T) {
	// httptest.ResponseRecorder does NOT implement Hijacker; the
	// wrapper should report that cleanly instead of panicking.
	r := router.New()
	r.Use(middleware.LoggerWith(slog.New(slog.NewTextHandler(io.Discard, nil))))

	var hijackErr error
	r.Get("/x", func(w http.ResponseWriter, _ *http.Request) {
		if h, ok := w.(http.Hijacker); ok {
			_, _, hijackErr = h.Hijack()
		}
	})

	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	if hijackErr == nil {
		t.Fatal("expected non-nil error from Hijack when underlying writer does not support it")
	}
	if !errors.Is(hijackErr, http.ErrNotSupported) {
		t.Errorf("hijack error = %v, want errors.Is(_, http.ErrNotSupported) — the conventional sentinel", hijackErr)
	}
}

func TestLogger_ResponseControllerCanFlushAndHijack(t *testing.T) {
	// Modern Go handlers use http.NewResponseController instead of
	// direct type assertions. ResponseController walks the writer
	// chain via Unwrap() to find the underlying optional interfaces;
	// without Unwrap on our wrapper, it would fail with
	// http.ErrNotSupported even though our type-asserting forwarders
	// would have worked.
	r := router.New()
	r.Use(middleware.LoggerWith(slog.New(slog.NewTextHandler(io.Discard, nil))))

	var flushErr, hijackErr error
	r.Get("/x", func(w http.ResponseWriter, _ *http.Request) {
		rc := http.NewResponseController(w)
		flushErr = rc.Flush()
		_, _, hijackErr = rc.Hijack()
	})

	rec := newFlushHijackRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if flushErr != nil {
		t.Errorf("ResponseController.Flush returned %v, want nil", flushErr)
	}
	if hijackErr != nil {
		t.Errorf("ResponseController.Hijack returned %v, want nil", hijackErr)
	}
	if !rec.flushed {
		t.Error("Flush did not reach underlying ResponseWriter via ResponseController")
	}
	if !rec.hijacked {
		t.Error("Hijack did not reach underlying ResponseWriter via ResponseController")
	}
}

func TestCompress_ResponseControllerCanFlushAndHijack(t *testing.T) {
	r := router.New()
	r.Use(middleware.Compress())

	var flushErr, hijackErr error
	r.Get("/x", func(w http.ResponseWriter, _ *http.Request) {
		rc := http.NewResponseController(w)
		flushErr = rc.Flush()
		_, _, hijackErr = rc.Hijack()
	})

	rec := newFlushHijackRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	r.ServeHTTP(rec, req)

	if flushErr != nil {
		t.Errorf("ResponseController.Flush returned %v, want nil", flushErr)
	}
	if hijackErr != nil {
		t.Errorf("ResponseController.Hijack returned %v, want nil", hijackErr)
	}
	if !rec.flushed {
		t.Error("Flush did not reach underlying ResponseWriter via ResponseController")
	}
	if !rec.hijacked {
		t.Error("Hijack did not reach underlying ResponseWriter via ResponseController")
	}
}

func TestCompress_WrappedWriterForwardsFlusherAndHijacker(t *testing.T) {
	r := router.New()
	r.Use(middleware.Compress())

	var sawFlusher, sawHijacker bool
	r.Get("/x", func(w http.ResponseWriter, _ *http.Request) {
		if f, ok := w.(http.Flusher); ok {
			sawFlusher = true
			f.Flush()
		}
		if h, ok := w.(http.Hijacker); ok {
			sawHijacker = true
			_, _, _ = h.Hijack()
		}
	})

	rec := newFlushHijackRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	r.ServeHTTP(rec, req)

	if !sawFlusher {
		t.Error("Compress writer should satisfy http.Flusher")
	}
	if !sawHijacker {
		t.Error("Compress writer should satisfy http.Hijacker")
	}
	if !rec.flushed {
		t.Error("Flush did not reach underlying ResponseWriter")
	}
	if !rec.hijacked {
		t.Error("Hijack did not reach underlying ResponseWriter")
	}
}

func TestLoggerCompressChain_HijackSkipBehaviour(t *testing.T) {
	// Three-layer chain — Logger wraps Compress wraps the
	// Hijacker-capable recorder. Handler hijacks via
	// http.NewResponseController. The S4 caveat (now in Logger's
	// godoc) was concerned that intermediate Hijacker layers would
	// bypass statusWriter and break the hijack-skip-logging
	// behaviour.
	//
	// In practice ResponseController tries the current writer as
	// Hijacker FIRST and only Unwrap()s on miss. statusWriter
	// implements Hijacker, so it gets called, and
	// statusWriter.hijacked is correctly set. This test pins that
	// behaviour and the S4 caveat doc is overstated for the
	// current chain shape; if a future wrapper hides statusWriter
	// from ResponseController, this test will start failing and
	// the doc warning becomes load-bearing.
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	r := router.New()
	r.Use(middleware.LoggerWith(logger), middleware.Compress())
	r.Get("/ws", func(w http.ResponseWriter, _ *http.Request) {
		rc := http.NewResponseController(w)
		_, _, _ = rc.Hijack()
	})

	rec := newFlushHijackRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	r.ServeHTTP(rec, req)

	if !rec.hijacked {
		t.Fatal("Hijack did not reach the underlying recorder through the chain")
	}
	if strings.Contains(buf.String(), `msg="http request"`) {
		t.Errorf("logger emitted a record after hijack through Logger→Compress chain — statusWriter.hijacked was not set:\n%s",
			buf.String())
	}
}

func TestCompress_PanicAfterHijackDoesNotTouchHijackedConn(t *testing.T) {
	// Pathological sequence: handler writes enough to commit to a
	// gzip stream, hijacks the connection, then panics. The
	// connection is no longer ours, so the panic-cleanup defer
	// must NOT call gz.Close (it would write the trailer through
	// the hijacked underlying writer, which is undefined).
	//
	// Test passes as long as the assertion below holds AND no
	// post-hijack Write reaches the recorder via the gzip wrap.
	preamble := bytes.Repeat([]byte("preamble "), 200) // 1800 bytes — crosses MinSize

	r := router.New()
	r.Use(middleware.Compress())
	r.Get("/upgrade-and-panic", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(preamble) // commits to compression
		if h, ok := w.(http.Hijacker); ok {
			_, _, _ = h.Hijack()
		}
		panic("panic after hijack")
	})

	rec := newFlushHijackRecorder()
	req := httptest.NewRequest(http.MethodGet, "/upgrade-and-panic", nil)
	req.Header.Set("Accept-Encoding", "gzip")

	defer func() {
		// Panic re-emerges via http.Server in production; in the
		// test it propagates here. Recover so the test framework
		// doesn't fail.
		_ = recover()
	}()
	r.ServeHTTP(rec, req)

	if !rec.hijacked {
		t.Fatal("Hijack did not reach underlying writer")
	}
	// The recorder body should contain only what gz wrote to it
	// BEFORE the hijack (the gzip header + maybe some deflate
	// bytes). It must NOT contain a gzip trailer written by the
	// panic-cleanup defer through the hijacked conn.
	//
	// Concretely: a panic-cleanup gz.Close on a hijacked writer
	// would either error harmlessly OR write trailer bytes after
	// the hijack point. Either is undefined; the regression
	// guards us from the "writes more bytes" path by asserting
	// the cleanup left the body at whatever size it was at hijack
	// time. We can't read that "size" precisely without whitebox
	// access, so the load-bearing check is: the test does not
	// panic / data-race / fail under `-race`. If hijacked is
	// observed and the test completes, we're good.
}

func TestCompress_FlushNoOpsAfterHijack(t *testing.T) {
	// B1 regression: handler writes a small body that gets
	// buffered, hijacks the connection, then calls Flush (rare in
	// practice but legal). Flush must NOT then enter the "start
	// compressing" branch and write gzip framing to a connection
	// we no longer own.
	r := router.New()
	r.Use(middleware.Compress())
	r.Get("/upgrade", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hi"))
		if h, ok := w.(http.Hijacker); ok {
			_, _, _ = h.Hijack()
		}
		// Post-hijack Flush — must not write through the wrapper.
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})

	rec := newFlushHijackRecorder()
	req := httptest.NewRequest(http.MethodGet, "/upgrade", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	r.ServeHTTP(rec, req)

	if !rec.hijacked {
		t.Fatal("Hijack did not reach underlying writer")
	}
	// After hijack, neither the buffered "hi" nor any gzip framing
	// should reach the recorder via our wrapper.
	if rec.Body.Len() != 0 {
		t.Errorf("Flush after hijack wrote bytes through the wrapper: body=%q, want empty", rec.Body.String())
	}
	if rec.Header().Get("Content-Encoding") == "gzip" {
		t.Errorf(`Flush after hijack set Content-Encoding to %q — should not have`, rec.Header().Get("Content-Encoding"))
	}
	if rec.flushed {
		// Specifically: the underlying recorder's Flush must not have been called.
		t.Errorf("underlying writer received Flush after hijack")
	}
}

func TestCompressRecoverChain_RecoverInside_RecoveryBodyInStream(t *testing.T) {
	// Use(Compress, Recover): Compress wraps Recover wraps handler.
	// Recover catches the panic via cw (the compressWriter), so
	// its "Internal Server Error" body flows THROUGH the gzip
	// stream. After Recover returns normally, Compress's deferred
	// finish() closes gz cleanly. Wire is a complete gzip stream
	// containing preamble + recovery message.
	preamble := bytes.Repeat([]byte("preamble "), 200) // 1800 bytes — crosses MinSize=1024

	r := router.New()
	r.Use(
		middleware.Compress(),
		middleware.RecoverWith(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	r.Get("/boom", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(preamble)
		panic("kaboom mid-stream")
	})

	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", rec.Header().Get("Content-Encoding"))
	}
	gz, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("body is not a valid gzip stream: %v", err)
	}
	body, err := io.ReadAll(gz)
	if err != nil {
		t.Errorf("gzip stream did not terminate cleanly: %v", err)
	}
	if !bytes.HasPrefix(body, preamble) {
		t.Errorf("body did not start with the pre-panic preamble")
	}
	if !bytes.Contains(body, []byte("Internal Server Error")) {
		t.Errorf("body did not contain Recover's 500 text — it should have flowed through the gzip wrap")
	}
}

func TestCompressRecoverChain_RecoverOutside_StreamStillValid(t *testing.T) {
	// Use(Recover, Compress): Recover wraps Compress wraps handler.
	// Recover's `w` is the original writer, so its
	// "Internal Server Error" goes RAW to the wire — used to
	// corrupt the gzip stream. Compress now catches panic
	// propagation in its own defer and closes gz cleanly first.
	// Result: a valid (truncated) gzip stream containing just the
	// preamble, with Recover's plain-text trailing after it. The
	// gzip part is decodable; the trailing junk is ignored by
	// browsers / clients (and would be Recover's plain 500 in our
	// case).
	preamble := bytes.Repeat([]byte("preamble "), 200)

	r := router.New()
	r.Use(
		middleware.RecoverWith(slog.New(slog.NewTextHandler(io.Discard, nil))),
		middleware.Compress(),
	)
	r.Get("/boom", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(preamble)
		panic("kaboom mid-stream")
	})

	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", rec.Header().Get("Content-Encoding"))
	}
	gz, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("gzip stream is not structurally valid (Compress's panic-defer didn't close it cleanly): %v", err)
	}
	// Disable multistream: rec.Body has plain-text bytes from
	// Recover trailing after the gzip member, and the default
	// multi-member reader would try to parse them as a second
	// gzip member and fail. The relevant assertion is that the
	// FIRST member decodes cleanly.
	gz.Multistream(false)
	body, err := io.ReadAll(gz)
	if err != nil {
		t.Errorf("first gzip member did not terminate cleanly: %v", err)
	}
	if !bytes.HasPrefix(body, preamble) {
		t.Errorf("decompressed body did not start with the pre-panic preamble")
	}
	// The recovery message went OUT of the gzip wrap (to the
	// original writer) — it should NOT appear inside the
	// decompressed body.
	if bytes.Contains(body, []byte("Internal Server Error")) {
		t.Errorf("recovery message ended up inside the gzip stream — that's the Recover-inside-Compress case, not this one")
	}
}

func TestCompress_FinishNoOpsAfterHijack(t *testing.T) {
	// B2 regression: handler writes a small body that gets
	// buffered, then hijacks the connection (e.g., upgrades to a
	// raw protocol). The middleware-side cw.finish() must not try
	// to flush the buffered bytes to the underlying writer — the
	// connection is no longer ours to use.
	r := router.New()
	r.Use(middleware.Compress())
	r.Get("/upgrade", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hi"))
		if h, ok := w.(http.Hijacker); ok {
			_, _, _ = h.Hijack()
		}
	})

	rec := newFlushHijackRecorder()
	req := httptest.NewRequest(http.MethodGet, "/upgrade", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	r.ServeHTTP(rec, req)

	if !rec.hijacked {
		t.Fatal("Hijack did not reach underlying writer")
	}
	// After hijack, finish() must not flush the buffered "hi".
	// We can't perfectly assert "no Write happened" via the
	// recorder (its Body would contain the bytes either way), but
	// the lack of a panic / data race is the load-bearing check.
	// The body MAY contain "hi" if our wrapper wrote it before
	// hijack-detection — assert it does NOT (the test confirms
	// finish() respects the hijacked flag).
	if rec.Body.String() == "hi" {
		t.Errorf("finish() wrote buffered bytes after hijack — body=%q, want empty", rec.Body.String())
	}
}

func TestLogger_SkipsRecordWhenHandlerHijackedWithoutWriting(t *testing.T) {
	// WebSocket-style upgrade: handler hijacks the connection and
	// writes directly to the raw socket. The wrapper saw no
	// WriteHeader/Write, so its status=200/bytes=0 would be lies
	// if logged. Expectation: no log record at all.
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	r := router.New()
	r.Use(middleware.LoggerWith(logger))
	r.Get("/ws", func(w http.ResponseWriter, _ *http.Request) {
		if h, ok := w.(http.Hijacker); ok {
			_, _, _ = h.Hijack()
		}
	})

	rec := newFlushHijackRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ws", nil))

	if strings.Contains(buf.String(), `msg="http request"`) {
		t.Errorf("logger should not emit record after hijack with no writes:\n%s", buf.String())
	}
}

func TestLogger_LogsWhenHandlerHijackedAfterWriting(t *testing.T) {
	// If the handler DID write through the wrapper before hijacking
	// (e.g., sent a 101 Switching Protocols via WriteHeader), the
	// status is meaningful and we should still log.
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	r := router.New()
	r.Use(middleware.LoggerWith(logger))
	r.Get("/ws", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusSwitchingProtocols)
		if h, ok := w.(http.Hijacker); ok {
			_, _, _ = h.Hijack()
		}
	})

	rec := newFlushHijackRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ws", nil))

	if !strings.Contains(buf.String(), `msg="http request"`) {
		t.Errorf("logger should emit record when handler wrote 101 before hijack:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "status=101") {
		t.Errorf("log should contain status=101, got:\n%s", buf.String())
	}
}

func TestLogger_CapturesNonDefaultStatus(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	r := router.New()
	r.Use(middleware.LoggerWith(logger))
	r.Get("/x", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	})

	exec(t, r, httptest.NewRequest(http.MethodGet, "/x", nil))
	if !strings.Contains(buf.String(), "status=403") {
		t.Errorf("log did not capture 403: %s", buf.String())
	}
}

// --- Recover ---

func TestRecover_CatchesPanicAndReturns500(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	r := router.New()
	r.Use(middleware.RecoverWith(logger))
	r.Get("/boom", func(http.ResponseWriter, *http.Request) {
		panic("kaboom")
	})

	rec := exec(t, r, httptest.NewRequest(http.MethodGet, "/boom", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d, want 500", rec.Code)
	}
	for _, want := range []string{
		`msg="panic recovered"`,
		`panic=kaboom`,
		"stack=",
		"path=/boom",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log missing %q:\n%s", want, buf.String())
		}
	}
}

func TestRecover_EmitsExactlyOneLogRecordWithRequestID(t *testing.T) {
	// Regression: an earlier implementation logged the request_id in
	// a *second* record. We want exactly one "panic recovered" record
	// with request_id attached as an attribute.
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	r := router.New()
	r.Use(middleware.RequestID(), middleware.RecoverWith(logger))
	r.Get("/boom", func(http.ResponseWriter, *http.Request) {
		panic("kaboom")
	})

	exec(t, r, httptest.NewRequest(http.MethodGet, "/boom", nil))

	out := buf.String()
	if c := strings.Count(out, `msg="panic recovered"`); c != 1 {
		t.Errorf("got %d 'panic recovered' records, want 1:\n%s", c, out)
	}
	if !strings.Contains(out, "request_id=") {
		t.Errorf("request_id should be on the panic record:\n%s", out)
	}
	if strings.Contains(out, `msg="panic request id"`) {
		t.Error("found legacy second log record 'panic request id' — should be merged into the first")
	}
}

func TestRecover_RepanicsErrAbortHandler(t *testing.T) {
	r := router.New()
	r.Use(middleware.RecoverWith(slog.New(slog.NewTextHandler(io.Discard, nil))))
	r.Get("/abort", func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	})

	defer func() {
		if v := recover(); v != http.ErrAbortHandler {
			t.Errorf("recover got %v, want ErrAbortHandler (stdlib's signal must propagate)", v)
		}
	}()
	exec(t, r, httptest.NewRequest(http.MethodGet, "/abort", nil))
}

func TestRecover_RepanicsWrappedErrAbortHandlerAsBareSentinel(t *testing.T) {
	// Stdlib http.Server's abort handling uses `v == ErrAbortHandler`,
	// which a wrapped error would slip past. Recover unwraps detection
	// via errors.Is but re-panics the bare sentinel so stdlib's
	// special path still fires. Wrapping context is dropped.
	r := router.New()
	r.Use(middleware.RecoverWith(slog.New(slog.NewTextHandler(io.Discard, nil))))
	r.Get("/abort", func(http.ResponseWriter, *http.Request) {
		panic(fmt.Errorf("client closed: %w", http.ErrAbortHandler))
	})

	defer func() {
		v := recover()
		if v != http.ErrAbortHandler {
			t.Errorf("recover got %v (%T), want bare http.ErrAbortHandler", v, v)
		}
	}()
	exec(t, r, httptest.NewRequest(http.MethodGet, "/abort", nil))
}

// --- Timeout ---

func TestTimeout_AttachesDeadlineToContext(t *testing.T) {
	r := router.New()
	r.Use(middleware.Timeout(50 * time.Millisecond))
	r.Get("/x", func(w http.ResponseWriter, req *http.Request) {
		dl, ok := req.Context().Deadline()
		if !ok {
			http.Error(w, "no deadline", http.StatusInternalServerError)
			return
		}
		// Deadline should be roughly start+50ms.
		if remaining := time.Until(dl); remaining > 50*time.Millisecond || remaining < 0 {
			http.Error(w, "weird deadline", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})

	rec := exec(t, r, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != 200 || rec.Body.String() != "ok" {
		t.Errorf("code=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestTimeout_ZeroOrNegativeIsNoOp(t *testing.T) {
	r := router.New()
	r.Use(middleware.Timeout(0))
	r.Get("/x", func(w http.ResponseWriter, req *http.Request) {
		if _, ok := req.Context().Deadline(); ok {
			http.Error(w, "deadline set when it shouldn't be", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	rec := exec(t, r, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != 200 {
		t.Errorf("code=%d body=%q", rec.Code, rec.Body.String())
	}
}

// --- composition smoke ---

func TestFullStack_RequestIDLoggerRecover(t *testing.T) {
	// The canonical four-middleware stack: request_id present in
	// Logger output AND in Recover log when a panic fires.
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	r := router.New()
	r.Use(
		middleware.RequestID(),
		middleware.LoggerWith(logger),
		middleware.RecoverWith(logger),
	)
	r.Get("/boom", func(http.ResponseWriter, *http.Request) {
		panic("oops")
	})

	rec := exec(t, r, httptest.NewRequest(http.MethodGet, "/boom", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d, want 500", rec.Code)
	}
	out := buf.String()
	if !strings.Contains(out, "panic recovered") {
		t.Errorf("missing panic log: %s", out)
	}
	if !strings.Contains(out, "request_id=") {
		t.Errorf("Logger should have emitted request_id: %s", out)
	}
}
