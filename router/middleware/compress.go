package middleware

import (
	"bufio"
	"compress/gzip"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/moostackhq/go/router"
)

// DefaultCompressMinSize is the body-size threshold (in bytes) below
// which [Compress] passes responses through uncompressed. Gzipping
// tiny responses costs CPU and can inflate them past the original.
const DefaultCompressMinSize = 1024

// CompressOptions controls [CompressWith].
type CompressOptions struct {
	// MinSize is the byte threshold below which responses are
	// passed through uncompressed. Zero uses [DefaultCompressMinSize].
	// Negative disables the threshold (compress everything that
	// reaches the writer).
	MinSize int
}

// gzipPool reuses *gzip.Writer instances across requests. Reset is
// cheap; allocating a fresh writer per request is the avoidable cost.
var gzipPool = sync.Pool{
	New: func() any { return gzip.NewWriter(io.Discard) },
}

// Compress returns middleware that gzips the response body when the
// client advertises gzip support via Accept-Encoding and the body
// is large enough to be worth compressing. See [CompressWith] for
// the configurable form.
func Compress() router.Middleware {
	return CompressWith(CompressOptions{})
}

// CompressWith is [Compress] with options.
//
// Caveats:
//
//   - Content-blind: responses are gzipped regardless of media type.
//     JS/CSS/HTML/JSON benefit; pre-compressed media (JPEG, PNG, MP4)
//     gains nothing and wastes CPU. Skip Compress on routes that
//     serve binary blobs.
//   - Responses whose handler has already set Content-Encoding are
//     passed through unchanged — Compress will not double-encode.
//   - Removes Content-Length on compressed responses since the
//     pre-encoding length doesn't match the wire length.
//   - When Content-Length is set and below MinSize, the response is
//     passed through without compression. Otherwise the first
//     MinSize bytes are buffered to decide; bodies that never
//     exceed the threshold are flushed unencoded on handler return.
//   - Panic safety: a handler panic mid-stream does NOT corrupt the
//     response. Compress catches the panic propagation in its own
//     defer, finalises any in-flight gzip stream cleanly (writing
//     the trailer), then lets the panic continue. Any outer
//     [Recover] that subsequently writes a plain-text 500 to the
//     original writer just trails after a complete gzip stream —
//     browsers ignore the tail. For the cleanest 500 body (the
//     recovery message ends up INSIDE the gzip stream), place
//     Recover inside Compress with Use(Compress(), Recover()).
//     The reverse order (Use(Recover(), Compress())) is also safe;
//     the response is truncated at the panic point but
//     structurally valid.
func CompressWith(opts CompressOptions) router.Middleware {
	minSize := opts.MinSize
	if minSize == 0 {
		minSize = DefaultCompressMinSize
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !acceptsGzip(r) {
				next.ServeHTTP(w, r)
				return
			}
			gz := gzipPool.Get().(*gzip.Writer)
			defer func() {
				// Reset to io.Discard before returning to the pool
				// so an idle entry doesn't pin this request's
				// ResponseWriter (and its underlying conn / header
				// map) in memory until the next Get+Reset.
				gz.Reset(io.Discard)
				gzipPool.Put(gz)
			}()

			cw := &compressWriter{
				ResponseWriter: w,
				gz:             gz,
				minSize:        minSize,
			}
			completed := false
			defer func() {
				if completed {
					cw.finish()
					return
				}
				// Handler / inner middleware panicked. If we've
				// already committed to a gzip stream, close it now
				// so the wire stays a structurally valid (truncated)
				// gzip — that way any outer Recover writing a
				// plain-text 500 to the original writer just trails
				// after a complete gzip stream instead of corrupting
				// it. Skip the close when the connection was
				// hijacked: the underlying writer isn't ours
				// anymore. We don't recover() the panic ourselves;
				// it keeps propagating.
				if cw.compressing && !cw.hijacked {
					_ = cw.gz.Close()
				}
			}()
			next.ServeHTTP(cw, r)
			completed = true
		})
	}
}

func acceptsGzip(r *http.Request) bool {
	for _, enc := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		enc = strings.TrimSpace(enc)
		// "gzip" or "gzip;q=0.8" — q-value parsing isn't worth it
		// (clients with q=0 are vanishingly rare and the cost of
		// honouring them outweighs the benefit).
		if enc == "gzip" || strings.HasPrefix(enc, "gzip;") {
			return true
		}
	}
	return false
}

// compressWriter buffers up to minSize bytes before deciding whether
// to gzip. Terminal states (mutually exclusive in any well-formed
// handler flow — once one is set, the methods that would transition
// to another short-circuit; a handler that writes through the
// wrapper AFTER a Hijack is undefined and may corrupt this):
//
//   - bypass=true: pass-through (caller set Content-Encoding, or
//     Content-Length is known and below minSize, or the response
//     was a 101 Switching Protocols and subsequent writes should
//     go to the raw protocol stream unmodified). Writes go straight
//     to the underlying ResponseWriter.
//   - compressing=true: threshold crossed; subsequent writes go
//     through gz; the buffer was flushed into gz at the transition.
//   - hijacked=true: handler took over the raw connection. finish()
//     and Flush() both no-op; the buffered body, the deferred status
//     code (headerCode), and any pending header mutations are all
//     dropped since the connection is no longer ours to write to.
//   - neither, on finish(): body finished under threshold and the
//     buffer is written uncompressed. If the handler called
//     WriteHeader explicitly the stored status is committed before
//     the buffered Write so it survives; if WriteHeader was never
//     called the buffer Write is allowed to trigger stdlib's
//     implicit-200 + Content-Type sniff.
type compressWriter struct {
	http.ResponseWriter
	gz      *gzip.Writer
	minSize int

	headerCode    int  // buffered status, 0 if WriteHeader not yet called
	headerWritten bool // true once underlying WriteHeader has fired

	buf         []byte
	bypass      bool
	compressing bool
	hijacked    bool // set by Hijack on success; finish() then no-ops
}

// WriteHeader stores the status for later commit (so we can defer
// the underlying WriteHeader until we know whether to compress).
// First call wins: subsequent calls are no-ops, matching stdlib's
// "first WriteHeader wins, rest ignored" contract. Without the
// second-call guard our deferred-commit design would let a later
// call silently overwrite the stored status — a divergence stdlib
// users would not expect.
//
// 1xx (informational / interim) responses are NOT deferred — they
// flow straight to the underlying writer and we record no state.
// 1xx responses never carry a compressible body, and a 1xx may be
// followed by another WriteHeader for the final response (e.g.
// 103 Early Hints → 200) which our deferred-commit machinery
// would block. The 101 Switching Protocols + Hijack pattern also
// only works if 101 reaches the wire BEFORE Hijack hands off the
// connection.
//
// One 1xx is special: 101 Switching Protocols commits the
// connection to a new protocol. Any subsequent write through this
// wrapper would interleave gzip framing with what's supposed to
// be raw protocol bytes — so we force bypass mode after 101 to
// pass any further writes through unmodified. (For 100/102/103
// the final 2xx response can still compress normally.)
func (c *compressWriter) WriteHeader(code int) {
	if code < 200 {
		c.ResponseWriter.WriteHeader(code)
		if code == http.StatusSwitchingProtocols {
			// Suppress any future startCompressing: subsequent
			// writes go straight to the underlying writer.
			c.bypass = true
			c.headerWritten = true
		}
		return
	}
	if c.headerWritten || c.headerCode != 0 {
		return
	}
	c.headerCode = code

	h := c.ResponseWriter.Header()
	// Caller already encoded — pass through verbatim.
	if h.Get("Content-Encoding") != "" {
		c.bypass = true
		c.commitHeader()
		return
	}
	// Known-small response: skip compression.
	if c.minSize > 0 {
		if cl := h.Get("Content-Length"); cl != "" {
			if n, err := strconv.Atoi(cl); err == nil && n < c.minSize {
				c.bypass = true
				c.commitHeader()
				return
			}
		}
	}
	// Otherwise defer the header commit until we know whether the
	// body crosses the threshold.
}

func (c *compressWriter) commitHeader() {
	if c.headerWritten {
		return
	}
	if c.headerCode == 0 {
		c.headerCode = http.StatusOK
	}
	c.ResponseWriter.WriteHeader(c.headerCode)
	c.headerWritten = true
}

func (c *compressWriter) Write(p []byte) (int, error) {
	if c.bypass {
		c.commitHeader()
		return c.ResponseWriter.Write(p)
	}
	if c.compressing {
		return c.gz.Write(p)
	}
	if c.minSize <= 0 {
		c.startCompressing()
		return c.gz.Write(p)
	}
	// Pre-check: if buf + p would cross the threshold, commit to
	// compression now and route p directly through gz instead of
	// growing buf to hold it. A single 10 MB Write should not first
	// balloon a 10 MB buffer just to immediately hand it to gz.
	if len(c.buf)+len(p) >= c.minSize {
		c.startCompressing()
		if len(c.buf) > 0 {
			buffered := c.buf
			c.buf = nil
			if _, err := c.gz.Write(buffered); err != nil {
				// We're committed; the caller's bytes are dropped
				// alongside the buffered prefix. Report len(p) so
				// the caller doesn't double-write (matches the
				// "absorbed, don't retry" contract).
				return len(p), err
			}
		}
		return c.gz.Write(p)
	}
	c.buf = append(c.buf, p...)
	return len(p), nil
}

func (c *compressWriter) startCompressing() {
	h := c.ResponseWriter.Header()
	h.Set("Content-Encoding", "gzip")
	h.Add("Vary", "Accept-Encoding")
	h.Del("Content-Length")
	c.commitHeader()
	c.gz.Reset(c.ResponseWriter)
	c.compressing = true
}

// finish closes the gzip stream or flushes the buffered body.
// Called by the middleware after the handler returns.
//
// Under-threshold path (the default branch): if we have buffered
// bytes, write them BEFORE committing the header — but only when
// the handler never called WriteHeader explicitly (headerCode == 0).
// Stdlib's ResponseWriter sniffs Content-Type on the first Write
// only when no Content-Type is set AND WriteHeader hasn't fired
// yet; pre-committing in that case would silently disable the
// sniff. When the handler DID call WriteHeader (e.g. http.Error
// → 500), we must commit the stored status before the Write or
// stdlib's implicit-200 takes over and the handler's status is
// lost.
func (c *compressWriter) finish() {
	// Hijacked: the connection no longer belongs to us. Buffered
	// bytes and the deferred status code are dropped — writing to
	// the underlying writer here is undefined. Match the Logger
	// pattern (which skips its log record under the same condition).
	if c.hijacked {
		return
	}
	switch {
	case c.compressing:
		_ = c.gz.Close()
	case c.bypass:
		c.commitHeader()
	default:
		if len(c.buf) > 0 {
			if c.headerCode != 0 {
				// Handler set a status — commit it before Write so
				// it doesn't get replaced by stdlib's implicit 200.
				c.commitHeader()
			}
			_, _ = c.ResponseWriter.Write(c.buf)
		} else {
			c.commitHeader()
		}
	}
}

// Unwrap returns the embedded ResponseWriter so [http.NewResponseController]
// can walk the chain and find optional interfaces on whatever is
// underneath. The explicit Flush / Hijack / Push forwarders below
// cover direct type assertions; Unwrap covers ResponseController.
func (c *compressWriter) Unwrap() http.ResponseWriter {
	return c.ResponseWriter
}

// Flush propagates a flush. If we're still buffering (under threshold)
// when Flush is called, the caller wants partial bytes on the wire
// now — typical of SSE — so we abandon the threshold optimisation
// and start compressing immediately, emitting the buffered prefix.
// Without this, Flush during buffering would silently swallow data
// until handler return.
//
// If the connection has been hijacked, Flush is a no-op for the same
// reason finish() is: the underlying writer is no longer ours, and
// writing gzip framing or invoking its Flush is undefined.
func (c *compressWriter) Flush() {
	if c.hijacked {
		return
	}
	if !c.compressing && !c.bypass {
		buffered := c.buf
		c.buf = nil
		c.startCompressing()
		if len(buffered) > 0 {
			_, _ = c.gz.Write(buffered)
		}
	}
	if c.compressing {
		_ = c.gz.Flush()
	}
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack returns the underlying connection unchanged. The caller
// takes over the connection; gzip framing for any further bytes is
// the hijacker's responsibility (typically: there are none, because
// hijack is for WebSocket upgrades and similar).
//
// On success we mark the writer as hijacked so finish() doesn't
// later try to flush buffered bytes / a deferred status to a
// connection we no longer own.
func (c *compressWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := c.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	conn, brw, err := h.Hijack()
	if err == nil {
		c.hijacked = true
	}
	return conn, brw, err
}

func (c *compressWriter) Push(target string, opts *http.PushOptions) error {
	if p, ok := c.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}
