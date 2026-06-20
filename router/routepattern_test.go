package router_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/moostackhq/go/router"
)

func TestRoutePattern_VisibleToMiddlewareAndHandler(t *testing.T) {
	var fromMiddleware, fromHandler string

	r := router.New()
	// A global middleware reads the pattern AFTER serving — the case that
	// fails without the context holder (stdlib leaves it downstream).
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req)
			fromMiddleware = router.RoutePattern(req.Context())
		})
	})
	r.Get("/monitors/{id}/check", func(w http.ResponseWriter, req *http.Request) {
		fromHandler = router.RoutePattern(req.Context())
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/monitors/42/check", nil)
	r.ServeHTTP(httptest.NewRecorder(), req)

	const want = "/monitors/{id}/check"
	if fromHandler != want {
		t.Errorf("handler saw %q, want %q", fromHandler, want)
	}
	if fromMiddleware != want {
		t.Errorf("middleware saw %q, want %q (the pattern must survive to outer middleware)", fromMiddleware, want)
	}
}

func TestRoutePattern_405HasRoute_404Empty(t *testing.T) {
	var captured string
	r := router.New()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req)
			captured = router.RoutePattern(req.Context())
		})
	})
	r.Get("/things/{id}", func(w http.ResponseWriter, req *http.Request) {})

	// Wrong method → 405, but the path matched, so the route is known.
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/things/1", nil))
	if captured != "/things/{id}" {
		t.Errorf("405 route = %q, want /things/{id}", captured)
	}

	// No such path → 404, route stays empty.
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/nope", nil))
	if captured != "" {
		t.Errorf("404 route = %q, want empty", captured)
	}
}

func TestRoutePattern_NoRouterReturnsEmpty(t *testing.T) {
	if got := router.RoutePattern(httptest.NewRequest(http.MethodGet, "/", nil).Context()); got != "" {
		t.Errorf("RoutePattern off a bare request = %q, want empty", got)
	}
}
