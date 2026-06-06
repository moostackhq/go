package router_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/moostackhq/go/router"
)

// noopHandler is intentionally trivial — these benchmarks measure
// the router's dispatch cost, not the handler's work.
var noopHandler = http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})

func BenchmarkRouter_DispatchStatic(b *testing.B) {
	r := router.New()
	r.Get("/users", noopHandler)
	req := httptest.NewRequest(http.MethodGet, "/users", nil)

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r.ServeHTTP(httptest.NewRecorder(), req)
	}
}

func BenchmarkRouter_DispatchPathParam(b *testing.B) {
	r := router.New()
	r.Get("/users/{id}", noopHandler)
	req := httptest.NewRequest(http.MethodGet, "/users/42", nil)

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r.ServeHTTP(httptest.NewRecorder(), req)
	}
}

func BenchmarkRouter_Dispatch404(b *testing.B) {
	// Specific concern: matchNotFound must not allocate per
	// dispatch (it didn't sort/alloc before either, but if the
	// pre-sorted cache ever regresses we want to catch it).
	r := router.New()
	r.Get("/known", noopHandler)
	r.NotFound(noopHandler)
	r.Group("/api", func(api *router.Router) {
		api.NotFound(noopHandler)
	})
	req := httptest.NewRequest(http.MethodGet, "/totally-unknown", nil)

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r.ServeHTTP(httptest.NewRecorder(), req)
	}
}

func BenchmarkRouter_DispatchWithMiddleware(b *testing.B) {
	identity := func(next http.Handler) http.Handler { return next }

	r := router.New()
	r.Use(identity, identity, identity)
	r.Get("/users/{id}", noopHandler)
	req := httptest.NewRequest(http.MethodGet, "/users/42", nil)

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r.ServeHTTP(httptest.NewRecorder(), req)
	}
}

func BenchmarkRouter_DispatchGroupNested(b *testing.B) {
	r := router.New()
	r.Group("/api", func(api *router.Router) {
		api.Group("/v1", func(v1 *router.Router) {
			v1.Get("/users/{id}", noopHandler)
		})
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/42", nil)

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r.ServeHTTP(httptest.NewRecorder(), req)
	}
}
