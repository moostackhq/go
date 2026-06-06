package middleware_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/moostackhq/go/router"
	"github.com/moostackhq/go/router/middleware"
)

// BenchmarkCompress_LargeSingleWrite exercises the compressWriter
// hot path that B2 fixed: a single large Write that crosses the
// threshold. Before the fix the entire payload was appended to an
// internal buffer first; after the fix it's routed straight through
// gz. The optimisation shows up as smaller allocated bytes per op
// (peaking at the buffer's growth pattern instead of len(p)).
func BenchmarkCompress_LargeSingleWrite(b *testing.B) {
	payload := bytes.Repeat([]byte("x"), 1<<20) // 1 MiB
	r := router.New()
	r.Use(middleware.Compress())
	r.Get("/x", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Accept-Encoding", "gzip")

	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	for i := 0; i < b.N; i++ {
		r.ServeHTTP(httptest.NewRecorder(), req)
	}
}

// BenchmarkCompress_SmallWritesAccumulating exercises the other
// half: many small Writes that each stay under the threshold then
// cumulatively cross it. This is the buf-growth path that still
// needs to grow to ~minSize; the B2 fix shouldn't regress it.
func BenchmarkCompress_SmallWritesAccumulating(b *testing.B) {
	chunk := bytes.Repeat([]byte("y"), 64) // 16 chunks → 1 KiB → threshold
	r := router.New()
	r.Use(middleware.Compress())
	r.Get("/x", func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i < 32; i++ {
			_, _ = w.Write(chunk)
		}
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Accept-Encoding", "gzip")

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r.ServeHTTP(httptest.NewRecorder(), req)
	}
}
