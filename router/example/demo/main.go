// Command demo illustrates every capability of
// github.com/moostackhq/go/router in a single short file. Every
// handler is a one-liner that echoes which route matched — the
// point is the wiring, not the handlers.
//
//	go run ./example/demo
//
// Then poke at it:
//
//	curl localhost:8080/
//	curl localhost:8080/users/42                    # path param
//	curl localhost:8080/api/things                  # group + CORS
//	curl localhost:8080/api/admin/things/1 -X DELETE \
//	     -H 'Authorization: Bearer x'               # nested group + auth
//	curl localhost:8080/api/unknown                 # per-group 404
//	curl localhost:8080/unknown                     # root 404
//	curl -X POST localhost:8080/                    # 405 with Allow
//	curl localhost:8080/static/anything             # Mount
//	curl localhost:8080/boom                        # Recover catches the panic
//	curl localhost:8080/api/things/                 # StripSlashes normalises
package main

import (
	"fmt"
	"net/http"
	"time"

	"github.com/moostackhq/go/router"
	"github.com/moostackhq/go/router/middleware"
)

func main() {
	r := router.New()

	// Global middleware: wraps the entire dispatch, including 404 / 405.
	// CORS belongs at the root: its preflight short-circuit must run
	// before the dispatcher's method check, otherwise OPTIONS reaches
	// the 405 handler instead of returning a CORS 204.
	r.Use(
		middleware.RequestID(),
		middleware.RealIP(), // rewrite RemoteAddr from X-Real-IP / X-Forwarded-For (assume a trusted proxy)
		middleware.Logger(),
		middleware.Recover(),
		middleware.StripSlashes(),
		middleware.CORS(middleware.CORSOptions{AllowedOrigins: []string{"*"}}),
		middleware.Compress(), // gzips responses ≥ 1024 bytes when the client accepts gzip
		middleware.Timeout(15*time.Second),
	)

	// Custom 404 / 405 for the root.
	r.NotFound(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "root: not found", http.StatusNotFound)
	}))
	r.MethodNotAllowed(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "method not allowed (Allow header lists what's registered)", http.StatusMethodNotAllowed)
	}))

	// Method shortcuts.
	r.Get("/", echo("home"))
	r.Get("/boom", func(http.ResponseWriter, *http.Request) {
		panic("demo panic — Recover turns this into a 500 + logged stack")
	})

	// Path parameter + typed accessor.
	r.Get("/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := router.PathInt(r, "id")
		if err != nil {
			http.Error(w, "id must be int", http.StatusBadRequest)
			return
		}
		fmt.Fprintf(w, "user %d\n", id)
	})

	// Group: prefix + group-scoped per-route middleware. The Group
	// layer is the right home for things that should only fire on
	// real route matches (auth, rate limits) — CORS is NOT one of
	// those because its preflight needs the global-middleware wrap.
	r.Group("/api", func(api *router.Router) {
		// Per-group NotFound: requests to /api/<unknown> dispatch
		// here, not to the root NotFound. Longest-prefix wins.
		api.NotFound(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "api: endpoint not found", http.StatusNotFound)
		}))

		api.Get("/things", echo("api list"))

		// Nested group with its own middleware (toy auth).
		api.Group("/admin", func(admin *router.Router) {
			admin.Use(requireBearer)
			admin.Delete("/things/{id}", echo("admin delete"))
		})
	})

	// Mount: delegate every request under /static/ to a handler.
	// The handler sees the full URL path (no prefix stripping by
	// the router); wrap with http.StripPrefix yourself if you want
	// the stripped form, like the standard file-server idiom.
	r.Mount("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "mounted handler saw %s\n", r.URL.Path)
	}))

	// Walk prints the route table at boot. The fourth argument is
	// the original handler the caller registered, useful for type
	// introspection — here we print its concrete type so you can
	// see Mount entries pointing at *http.HandlerFunc, FileServer
	// values, etc.
	fmt.Println("routes:")
	r.Walk(func(method, pattern string, _, raw http.Handler) {
		fmt.Printf("  %-6s %-20s (%T)\n", method, pattern, raw)
	})

	fmt.Println("\nlistening on :8080")
	_ = http.ListenAndServe(":8080", r)
}

// echo returns a handler that writes a one-line label so the
// response makes it obvious which route matched.
func echo(label string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, label)
	}
}

// requireBearer is a one-line auth-style middleware that 401s on
// requests without an Authorization header.
func requireBearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
