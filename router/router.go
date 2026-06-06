// Package router wraps stdlib's [http.ServeMux] (Go 1.22+) with
// method shortcuts, middleware chains, prefix groups, mountable
// sub-handlers, and customisable 404 / 405 handlers. See the
// repository README for the rationale and a feature comparison.
//
// Routing follows stdlib pattern syntax: method prefixes
// ("GET /users"), {name} captures, {name...} trailing wildcards.
// Path values come out via [http.Request.PathValue]; typed sugar
// lives in [PathInt] / [PathInt64] / [PathFloat].
//
// A [Router] implements [http.Handler], so pass it to
// [http.ListenAndServe] directly. The [Middleware] type matches the
// de facto stdlib shape (`func(http.Handler) http.Handler`); any
// existing net/http-compatible middleware drops in without
// conversion.
//
// The built-in middleware (RequestID, Logger, Recover, Timeout,
// RealIP, Compress, CORS, StripSlashes) lives in the satellite
// package [github.com/moostackhq/go/router/middleware].
package router

import (
	"cmp"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Mount uses two distinct sentinels because the two contexts have
// different needs:
//
//   - mountMethodSentinel is what methodsByPattern stores; the
//     dispatcher's method check special-cases it to mean "any
//     method matches".
//   - mountWalkLabel is the human-readable string shown by [Router.Walk]
//     and visible to package users.
const (
	mountMethodSentinel = "*"
	mountWalkLabel      = "ALL"
)

// Middleware wraps an [http.Handler] with additional behaviour.
// Composition is outer-to-inner: Use(A, B); Get(p, h) produces the
// chain A(B(h)).
//
// The signature matches the de facto stdlib convention, so
// middleware written for any net/http-based ecosystem (chi,
// gorilla, plain net/http) drops in without conversion.
type Middleware func(http.Handler) http.Handler

// Router composes net/http routing with method shortcuts, middleware
// chains, prefix groups, and customisable 404 / 405 handlers.
//
// Router itself implements [http.Handler]; pass it to
// [http.ListenAndServe] directly.
//
// Router is NOT safe for concurrent registration — register every
// route at boot before the server starts serving. Once serving, the
// router is safe for concurrent reads from any number of goroutines.
// (Stdlib http.ServeMux is stricter and permits concurrent Handle
// calls; we don't, to keep the registration code lock-free.)
type Router struct {
	root       *rootState
	prefix     string
	middleware []Middleware
}

// rootState is the shared mutable state across one Router tree
// (root + every Group descended from it). Registration writes here.
type rootState struct {
	// methodMux holds the registered "METHOD /pattern" entries plus
	// any Mount patterns. Used for the actual dispatch when a
	// path + method match.
	methodMux *http.ServeMux

	// pathOnlyMux holds each unique path pattern once with no
	// method prefix. Used to disambiguate 404 (no path match) from
	// 405 (path match, method mismatch) at dispatch time.
	pathOnlyMux *http.ServeMux

	// methodsByPattern lists the methods registered for each path
	// pattern, in registration order. The [mountMethodSentinel]
	// value means "any method" and is used by Mount. The Allow
	// header on a 405 is populated from this list (sentinel
	// stripped).
	methodsByPattern map[string][]string

	// registeredPaths deduplicates pathOnlyMux registrations: two
	// Handle calls for the same path (different methods) must
	// register pathOnlyMux only once or it panics on duplicate.
	registeredPaths map[string]bool

	// notFoundByPrefix maps group prefixes to their NotFound
	// handler. Resolution walks notFoundOrder (longest-first) so a
	// more specific prefix beats a more general one.
	notFoundByPrefix map[string]http.Handler
	// notFoundOrder is the keys of notFoundByPrefix sorted longest-
	// first. Pre-sorted at registration time so dispatch doesn't
	// allocate + sort on every 404 (which is what an attacker
	// spamming unknown paths would otherwise exercise).
	notFoundOrder []string

	// methodNotAllowed is the root-only override for 405 responses.
	// nil means "fall back to a plain stdlib 405". Set via
	// [Router.MethodNotAllowed]; the Allow header is populated by
	// the dispatcher before the handler runs.
	methodNotAllowed http.Handler

	// routes captures every registered route in registration order
	// for [Router.Walk]. Includes Mount entries (with the method
	// shown as [mountWalkLabel]) but not NotFound or
	// MethodNotAllowed handlers.
	routes []registeredRoute

	// globalMiddleware wraps the entire dispatch — pattern matching
	// AND route handlers AND 404/405. Use on the root Router
	// appends here; Use on a Group appends to that group's local
	// middleware (per-route wrapping). The two-tier split lets
	// path-rewriting middleware (StripSlashes) and short-circuiting
	// middleware (CORS preflight) intercept BEFORE routing.
	globalMiddleware []Middleware

	// chainOnce caches the wrapped dispatcher built from
	// globalMiddleware so we don't recompose closures per request.
	// Routes / middleware added AFTER the first request are
	// reflected in dispatch (methodMux state is read on each
	// call) but NOT in the global middleware wrap.
	chainOnce      sync.Once
	cachedDispatch http.Handler
}

// registeredRoute is the per-route record stored in rootState.routes
// and yielded by Walk. The type is unexported because it's an
// internal record; the fields are exported for readability at the
// few call sites that construct and read it (Handle, Mount, Walk).
type registeredRoute struct {
	Method     string
	Pattern    string
	Handler    http.Handler // middleware chain applied
	RawHandler http.Handler // exactly what the caller passed in
}

// New constructs an empty Router.
func New() *Router {
	return &Router{
		root: &rootState{
			methodMux:        http.NewServeMux(),
			pathOnlyMux:      http.NewServeMux(),
			methodsByPattern: map[string][]string{},
			registeredPaths:  map[string]bool{},
		},
	}
}

// ServeHTTP implements [http.Handler]. Global middleware (added via
// Use on the root Router) wraps the entire dispatch, including
// 404/405 paths and the routing decision itself; group-level
// middleware wraps individual route handlers only.
//
// Dispatch sequence (inside the global-middleware wrap):
//
//  1. Match the request path against any registered pattern,
//     ignoring method. No match → 404 (per-group NotFound resolved
//     longest-prefix-first; fallback is stdlib's [http.NotFound]).
//  2. Path matched: check the registered methods for that pattern.
//     Method mismatch → 405 with Allow header populated; falls back
//     to a plain stdlib 405 if no custom handler is configured.
//  3. Method matches: dispatch through the method-aware mux so the
//     wrapped (per-route middleware applied) handler runs.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.root.chainOnce.Do(func() {
		var h http.Handler = http.HandlerFunc(r.root.dispatch)
		for i := len(r.root.globalMiddleware) - 1; i >= 0; i-- {
			h = r.root.globalMiddleware[i](h)
		}
		r.root.cachedDispatch = h
	})
	r.root.cachedDispatch.ServeHTTP(w, req)
}

func (rs *rootState) dispatch(w http.ResponseWriter, req *http.Request) {
	_, pathPat := rs.pathOnlyMux.Handler(req)
	if pathPat == "" {
		rs.serve404(w, req)
		return
	}
	allowed := rs.methodsByPattern[pathPat]
	if !methodMatches(allowed, req.Method) {
		rs.serve405(w, req, allowed)
		return
	}
	rs.methodMux.ServeHTTP(w, req)
}

func methodMatches(allowed []string, method string) bool {
	for _, a := range allowed {
		if a == mountMethodSentinel || a == method {
			return true
		}
	}
	return false
}

func (rs *rootState) serve404(w http.ResponseWriter, req *http.Request) {
	if h := rs.matchNotFound(req.URL.Path); h != nil {
		h.ServeHTTP(w, req)
		return
	}
	http.NotFound(w, req)
}

// matchNotFound returns the NotFound handler whose group prefix is
// the longest match for path, or nil if none is registered. Reads
// the pre-sorted notFoundOrder list — no allocation per call.
func (rs *rootState) matchNotFound(path string) http.Handler {
	for _, p := range rs.notFoundOrder {
		if prefixCovers(path, p) {
			return rs.notFoundByPrefix[p]
		}
	}
	return nil
}

// prefixCovers reports whether p is a path-prefix of path in the
// router-group sense. Empty p (root) covers everything.
func prefixCovers(path, p string) bool {
	if p == "" {
		return true
	}
	if path == p {
		return true
	}
	return strings.HasPrefix(path, p+"/")
}

func (rs *rootState) serve405(w http.ResponseWriter, req *http.Request, allowed []string) {
	// Allow header lists actual methods, never the mount sentinel.
	methods := make([]string, 0, len(allowed))
	for _, m := range allowed {
		if m == mountMethodSentinel {
			continue
		}
		methods = append(methods, m)
	}
	w.Header().Set("Allow", strings.Join(methods, ", "))
	if rs.methodNotAllowed != nil {
		rs.methodNotAllowed.ServeHTTP(w, req)
		return
	}
	http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
}

// Use appends middleware. The placement is significant:
//
//   - On the ROOT router (the one returned by [New]) middleware is
//     "global": it wraps the entire dispatch — pattern matching,
//     route handlers, AND 404 / 405 paths. Use this layer for
//     cross-cutting concerns like Logger, Recover, RequestID,
//     RealIP, StripSlashes (path rewriting before matching), and
//     CORS (preflight that must short-circuit before method check).
//     Global middleware applies to every route regardless of
//     registration order, because it wraps the dispatcher itself.
//
//   - On a GROUP router middleware is "per-route" — wraps only the
//     handlers registered through that group (and its sub-groups
//     via snapshot inheritance). Use this layer for auth, route-
//     specific rate limits, etc. Group middleware follows snapshot
//     semantics: routes registered AFTER the Use call DO see the
//     middleware; routes registered BEFORE the Use call are
//     unaffected; sub-groups created BEFORE the Use call don't
//     inherit it (sub-groups created after do).
//
// All middleware must be in place before the first request — late
// additions to global middleware after ServeHTTP has run are
// silently ignored (the chain is cached on first dispatch).
//
// Panics if any middleware is nil — a nil entry would crash the
// chain on first dispatch with a less useful stack trace.
func (r *Router) Use(mw ...Middleware) {
	for i, m := range mw {
		if m == nil {
			panic(fmt.Sprintf("router: Use called with nil middleware at index %d", i))
		}
	}
	if r.prefix == "" {
		r.root.globalMiddleware = append(r.root.globalMiddleware, mw...)
		return
	}
	r.middleware = append(r.middleware, mw...)
}

// Handle registers handler for method + pattern. The pattern is
// concatenated with the current group prefix.
//
// Panics if method is empty or contains anything other than
// uppercase ASCII letters — this catches typos like "GETs" that
// would otherwise silently register a dead route (stdlib ServeMux
// matches method strings literally and case-sensitively). Custom
// uppercase methods (e.g., WebDAV's "PROPFIND") are accepted.
func (r *Router) Handle(method, pattern string, h http.Handler) {
	validateMethod(method)
	fullPath := joinPrefix(r.prefix, pattern)
	if fullPath == "" {
		panic("router: empty path pattern")
	}
	if !strings.HasPrefix(fullPath, "/") {
		panic(fmt.Sprintf(`router: path pattern must start with "/", got %q (from pattern %q)`, fullPath, pattern))
	}
	chain := r.applyMiddleware(h)

	r.root.methodMux.Handle(method+" "+fullPath, chain)
	if !r.root.registeredPaths[fullPath] {
		r.root.pathOnlyMux.Handle(fullPath, http.NotFoundHandler())
		r.root.registeredPaths[fullPath] = true
	}
	r.root.methodsByPattern[fullPath] = append(r.root.methodsByPattern[fullPath], method)
	r.root.routes = append(r.root.routes, registeredRoute{
		Method: method, Pattern: fullPath, Handler: chain, RawHandler: h,
	})
}

// HandleFunc is [Router.Handle] for [http.HandlerFunc].
func (r *Router) HandleFunc(method, pattern string, h http.HandlerFunc) {
	r.Handle(method, pattern, h)
}

// Get registers a GET handler.
func (r *Router) Get(pattern string, h http.HandlerFunc) { r.HandleFunc("GET", pattern, h) }

// Post registers a POST handler.
func (r *Router) Post(pattern string, h http.HandlerFunc) { r.HandleFunc("POST", pattern, h) }

// Put registers a PUT handler.
func (r *Router) Put(pattern string, h http.HandlerFunc) { r.HandleFunc("PUT", pattern, h) }

// Patch registers a PATCH handler.
func (r *Router) Patch(pattern string, h http.HandlerFunc) { r.HandleFunc("PATCH", pattern, h) }

// Delete registers a DELETE handler.
func (r *Router) Delete(pattern string, h http.HandlerFunc) { r.HandleFunc("DELETE", pattern, h) }

// Head registers a HEAD handler. Note: stdlib ServeMux does not
// auto-derive HEAD from a registered GET; register HEAD explicitly
// if you need it.
func (r *Router) Head(pattern string, h http.HandlerFunc) { r.HandleFunc("HEAD", pattern, h) }

// Options registers an OPTIONS handler.
func (r *Router) Options(pattern string, h http.HandlerFunc) { r.HandleFunc("OPTIONS", pattern, h) }

// Group creates a sub-Router with the given prefix appended and a
// snapshot of the current middleware chain inherited. The callback
// receives the sub-router; routes registered inside the callback
// pick up the prefix and the inherited chain. Use calls inside the
// callback add to the sub-router only.
//
//	r.Group("/api", func(api *Router) {
//	    api.Get("/users", ...)            // → GET /api/users
//	    api.Group("/v1", func(v1 *Router) {
//	        v1.Get("/users", ...)         // → GET /api/v1/users
//	    })
//	})
//
// An empty prefix is permitted and creates a sub-Router that shares
// the parent's prefix — useful for scoping middleware to a subset
// of routes without changing the URL path (the chi idiom).
//
// A prefix of exactly "/" panics: it's a configuration bug. The
// user either meant "" (group-for-middleware-scoping) or a real
// path prefix like "/api". A literal "/" would create a sub-router
// whose NotFound only fires on the exact path "/", which is almost
// never what was intended.
func (r *Router) Group(prefix string, fn func(g *Router)) {
	if prefix == "/" {
		panic(`router: Group("/", ...) is a configuration bug — use "" to group on the root, or a real prefix like "/api"`)
	}
	g := &Router{
		root:       r.root,
		prefix:     joinPrefix(r.prefix, prefix),
		middleware: slices.Clone(r.middleware),
	}
	fn(g)
}

// Mount registers handler for every request whose path begins with
// the given prefix, regardless of method. The prefix is normalised
// to end in "/" — Mount("/static", h) and Mount("/static/", h)
// behave identically. The handler sees the full URL path (no
// prefix stripping); wrap with [http.StripPrefix] if you need
// stripping. Middleware on this Router applies.
//
// Mount is for delegating to existing handlers like static-file
// servers, third-party libraries, or net/http/pprof. For your own
// sub-trees, [Router.Group] gives finer control over methods and
// middleware.
//
// In [Router.Walk] output mount entries show the method as
// [mountWalkLabel] ("ALL") rather than a real HTTP verb.
//
// An empty prefix panics — it's a configuration bug. Use "/" for a
// deliberate catch-all (every method, every path), or a real path
// prefix like "/static/" for a sub-tree.
//
// Overlap with Group: if a more-specific route (registered through
// Get / Post / Group) shares a Mount's prefix, stdlib ServeMux's
// longest-pattern-wins semantics route the specific path to the
// specific handler and everything else under the prefix to the
// mounted handler. E.g., r.Get("/api/health", ...) together with
// r.Mount("/api/", h) — /api/health hits the GET handler,
// /api/anything-else hits the mount.
func (r *Router) Mount(prefix string, h http.Handler) {
	if prefix == "" {
		panic(`router: Mount("", ...) is a configuration bug — pass a real prefix like "/static/", or "/" for a catch-all`)
	}
	fullPath := joinPrefix(r.prefix, prefix)
	if !strings.HasPrefix(fullPath, "/") {
		panic(fmt.Sprintf(`router: Mount path must start with "/", got %q (from prefix %q)`, fullPath, prefix))
	}
	if !strings.HasSuffix(fullPath, "/") {
		fullPath += "/"
	}
	chain := r.applyMiddleware(h)

	r.root.methodMux.Handle(fullPath, chain)
	if !r.root.registeredPaths[fullPath] {
		r.root.pathOnlyMux.Handle(fullPath, http.NotFoundHandler())
		r.root.registeredPaths[fullPath] = true
	}
	r.root.methodsByPattern[fullPath] = []string{mountMethodSentinel}
	r.root.routes = append(r.root.routes, registeredRoute{
		Method: mountWalkLabel, Pattern: fullPath, Handler: chain, RawHandler: h,
	})
}

// NotFound sets the handler invoked when no registered route
// matches the request path. Called on a Group, the handler scopes
// to that group's prefix; the root's NotFound is the global default.
//
// Most-specific prefix wins: if both /api and root register
// NotFound, a request to /api/missing dispatches to /api's
// NotFound; a request to /unknown dispatches to root's.
//
// Panics if NotFound has already been set for the same scope (root,
// or this group's prefix) — duplicate registration is almost
// certainly a bug.
func (r *Router) NotFound(h http.Handler) {
	if r.root.notFoundByPrefix == nil {
		r.root.notFoundByPrefix = map[string]http.Handler{}
	}
	if _, exists := r.root.notFoundByPrefix[r.prefix]; exists {
		scope := r.prefix
		if scope == "" {
			scope = "(root)"
		}
		panic("router: NotFound already registered for scope " + scope)
	}
	r.root.notFoundByPrefix[r.prefix] = h

	// Rebuild the longest-first lookup list. NotFound calls are
	// rare (boot-time only); per-request matchNotFound stays
	// allocation-free.
	keys := make([]string, 0, len(r.root.notFoundByPrefix))
	for k := range r.root.notFoundByPrefix {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	r.root.notFoundOrder = keys
}

// MethodNotAllowed sets the handler invoked when a registered path
// matches but no registration covers the request method. The Allow
// header is set with the registered methods before the handler
// runs. Root-only — per-group scoping is out of scope for v1.
//
// Panics if called more than once on the same Router tree —
// duplicate registration is almost certainly a bug.
func (r *Router) MethodNotAllowed(h http.Handler) {
	if r.root.methodNotAllowed != nil {
		panic("router: MethodNotAllowed already registered")
	}
	r.root.methodNotAllowed = h
}

// Walk yields every registered route. Iteration order is the order
// in which routes were registered (across all groups, in the
// sequence the registration calls fired) — guaranteed stable so
// CLI output and CI snapshots stay deterministic.
//
// Mount entries appear with method [mountWalkLabel] ("ALL").
// NotFound and MethodNotAllowed handlers are not included.
//
// The callback receives two handlers per route:
//
//   - handler — the middleware chain applied to the registered
//     handler. Call this to invoke the route exactly as the
//     dispatcher would; matches what runs at request time.
//   - raw — the original [http.Handler] the caller passed to
//     [Router.Handle] / [Router.Mount]. Useful for type
//     introspection (`if _, ok := raw.(*http.ServeMux); ok ...`)
//     and for building debug tooling that needs to see through
//     the middleware wrap.
func (r *Router) Walk(fn func(method, pattern string, handler, raw http.Handler)) {
	for _, rt := range r.root.routes {
		fn(rt.Method, rt.Pattern, rt.Handler, rt.RawHandler)
	}
}

// validateMethod panics if method is empty or contains anything
// other than uppercase ASCII letters. The strictness catches typos
// (e.g., "GETs") that stdlib ServeMux would silently accept as a
// distinct method never reachable by real clients.
func validateMethod(method string) {
	if method == "" {
		panic("router: empty HTTP method")
	}
	for _, c := range method {
		if c < 'A' || c > 'Z' {
			panic(fmt.Sprintf("router: HTTP method must be uppercase ASCII letters, got %q", method))
		}
	}
}

// applyMiddleware wraps h with the router's middleware chain. Order
// is preserved as outer-to-inner: Use(A, B) produces A(B(h)).
func (r *Router) applyMiddleware(h http.Handler) http.Handler {
	chain := h
	for i := len(r.middleware) - 1; i >= 0; i-- {
		chain = r.middleware[i](chain)
	}
	return chain
}

// joinPrefix concatenates a group prefix with a route pattern,
// normalising slashes so callers can pass either form for either
// side.
func joinPrefix(prefix, pattern string) string {
	if prefix == "" {
		return pattern
	}
	if pattern == "" {
		return prefix
	}
	prefix = strings.TrimSuffix(prefix, "/")
	if !strings.HasPrefix(pattern, "/") {
		return prefix + "/" + pattern
	}
	return prefix + pattern
}

// PathInt returns the int value of the named path parameter, or an
// error when absent or non-numeric. Convenience for the common case;
// callers needing fancier parsing can use [http.Request.PathValue]
// directly.
func PathInt(r *http.Request, name string) (int, error) {
	s := r.PathValue(name)
	if s == "" {
		return 0, fmt.Errorf("router: path param %q not present", name)
	}
	return strconv.Atoi(s)
}

// PathInt64 is [PathInt] for int64.
func PathInt64(r *http.Request, name string) (int64, error) {
	s := r.PathValue(name)
	if s == "" {
		return 0, fmt.Errorf("router: path param %q not present", name)
	}
	return strconv.ParseInt(s, 10, 64)
}

// PathFloat is [PathInt] for float64.
func PathFloat(r *http.Request, name string) (float64, error) {
	s := r.PathValue(name)
	if s == "" {
		return 0, fmt.Errorf("router: path param %q not present", name)
	}
	return strconv.ParseFloat(s, 64)
}
