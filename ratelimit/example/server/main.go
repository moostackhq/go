// Command server demonstrates github.com/moostackhq/go/ratelimit:
// the limiter as a plain library, as middleware on a single route and
// on a route group, with the memory and sqlite backends, a custom key
// function, namespacing a shared store, and fail-open vs fail-closed.
//
//	go run ./example/server
//
// It first prints a short library-only demo to stdout, then serves:
//
//	# public "/": generous IP limit (memory), fail-open — watch the headers
//	for i in $(seq 1 8); do
//	  curl -s -o /dev/null -D - localhost:8080/ | grep -i '^ratelimit'
//	done
//
//	# "/login": strict 3/min (sqlite), fail-CLOSED — 4th is 429
//	for i in $(seq 1 4); do
//	  curl -s -o /dev/null -w "%{http_code}\n" -X POST localhost:8080/login
//	done
//
//	# "/api/*": one limiter for the whole group, keyed by X-API-Key (else IP)
//	curl -s -D - -o /dev/null -H 'X-API-Key: alice' localhost:8080/api/data  | grep -i '^ratelimit'
//	curl -s -D - -o /dev/null -H 'X-API-Key: bob'   localhost:8080/api/whoami | grep -i '^ratelimit'
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/moostackhq/go/ratelimit"

	_ "modernc.org/sqlite"
)

func main() {
	ctx := context.Background()

	// --- 1. As a library, no HTTP: burst then deny with a retry hint.
	libDemo(ctx)

	// --- 2. Two backends. Memory is per-process; sqlite is shared
	// across processes on one host. (":memory:" here so the example
	// leaves no file; a real app passes a path.)
	mem := ratelimit.NewMemoryStore(ratelimit.WithCleanupInterval(time.Minute))
	defer mem.Close()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		log.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	sqlite, err := ratelimit.NewSQLiteStore(ctx, db)
	if err != nil {
		log.Fatal(err)
	}

	// --- 3. Limiters. The api and a hypothetical second memory-backed
	// limiter share `mem`, so each gets a namespace to avoid colliding
	// on the same client key.
	public, _ := ratelimit.New(mem, ratelimit.PerSecond(5).WithBurst(5), ratelimit.WithNamespace("public"))
	login, _ := ratelimit.New(sqlite, ratelimit.PerMinute(3), ratelimit.WithNamespace("login"))
	api, _ := ratelimit.New(mem, ratelimit.PerMinute(30), ratelimit.WithNamespace("api"))

	// --- 4. Middleware. Route-level on "/" and "/login"; group-level
	// on the whole "/api/" subtree via one wrapped sub-mux.
	apiMux := http.NewServeMux()
	apiMux.HandleFunc("/api/data", echo("api: data"))
	apiMux.HandleFunc("/api/whoami", echo("api: whoami"))

	mux := http.NewServeMux()
	mux.Handle("/", ratelimit.Middleware(public)(echo("public: hello")))
	mux.Handle("/login", ratelimit.Middleware(login, ratelimit.WithFailClosed())(echo("login: ok")))
	mux.Handle("/api/", ratelimit.Middleware(api, ratelimit.WithKeyFunc(apiKeyOrIP))(apiMux))

	log.Println("listening on :8080 — see the curl commands in the file header")
	log.Fatal(http.ListenAndServe(":8080", mux))
}

// libDemo shows the core API: Allow returns a decision plus metadata,
// no HTTP involved.
func libDemo(ctx context.Context) {
	lim, _ := ratelimit.New(ratelimit.NewMemoryStore(), ratelimit.PerSecond(2).WithBurst(3))
	fmt.Println(`library demo — PerSecond(2).WithBurst(3), key "demo":`)
	for i := 1; i <= 4; i++ {
		res, _ := lim.Allow(ctx, "demo")
		if res.Allowed {
			fmt.Printf("  req %d: allowed (remaining %d)\n", i, res.Remaining)
		} else {
			fmt.Printf("  req %d: denied, retry after %s\n", i, res.RetryAfter.Round(time.Millisecond))
		}
	}
	fmt.Println()
}

// apiKeyOrIP keys on an API key header when present, otherwise the
// client IP — reusing the safe default for the fallback.
func apiKeyOrIP(r *http.Request) string {
	if k := r.Header.Get("X-API-Key"); k != "" {
		return "key:" + k
	}
	return "ip:" + ratelimit.KeyByIP(r)
}

func echo(msg string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, msg)
	}
}
