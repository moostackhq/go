package router_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/moostackhq/go/router"
)

// exec runs h against a synthetic (method, path) request and returns
// (status, body).
func exec(t *testing.T, h http.Handler, method, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Body)
	return rec.Code, string(body)
}

// --- basic dispatch ---

func TestRouter_BasicGet(t *testing.T) {
	r := router.New()
	r.Get("/hello", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hi"))
	})
	code, body := exec(t, r, http.MethodGet, "/hello")
	if code != 200 || body != "hi" {
		t.Errorf("code=%d body=%q, want 200 hi", code, body)
	}
}

func TestRouter_AllMethodShortcuts(t *testing.T) {
	r := router.New()
	methods := []string{
		http.MethodGet, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodHead,
		http.MethodOptions,
	}
	register := map[string]func(string, http.HandlerFunc){
		http.MethodGet:     r.Get,
		http.MethodPost:    r.Post,
		http.MethodPut:     r.Put,
		http.MethodPatch:   r.Patch,
		http.MethodDelete:  r.Delete,
		http.MethodHead:    r.Head,
		http.MethodOptions: r.Options,
	}
	for _, m := range methods {
		m := m
		register[m]("/"+strings.ToLower(m), func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(m))
		})
	}
	for _, m := range methods {
		code, body := exec(t, r, m, "/"+strings.ToLower(m))
		if code != 200 || body != m {
			t.Errorf("method %s: code=%d body=%q", m, code, body)
		}
	}
}

// --- 404 / 405 ---

func TestRouter_PathMismatch_Default404(t *testing.T) {
	r := router.New()
	r.Get("/x", func(http.ResponseWriter, *http.Request) {})
	code, _ := exec(t, r, http.MethodGet, "/y")
	if code != 404 {
		t.Errorf("code = %d, want 404", code)
	}
}

func TestRouter_MethodMismatch_Default405WithAllow(t *testing.T) {
	r := router.New()
	r.Get("/x", func(http.ResponseWriter, *http.Request) {})
	r.Post("/x", func(http.ResponseWriter, *http.Request) {})

	req := httptest.NewRequest(http.MethodPut, "/x", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 405 {
		t.Errorf("code = %d, want 405", rec.Code)
	}
	allow := rec.Header().Get("Allow")
	if !strings.Contains(allow, "GET") || !strings.Contains(allow, "POST") {
		t.Errorf("Allow = %q, want both GET and POST", allow)
	}
}

func TestRouter_NotFound_RootCustom(t *testing.T) {
	r := router.New()
	r.NotFound(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte("custom 404"))
	}))
	code, body := exec(t, r, http.MethodGet, "/anywhere")
	if code != 404 || body != "custom 404" {
		t.Errorf("code=%d body=%q", code, body)
	}
}

func TestRouter_NotFound_PerGroup(t *testing.T) {
	r := router.New()
	r.NotFound(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte("root"))
	}))
	r.Group("/api", func(api *router.Router) {
		api.NotFound(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(404)
			_, _ = w.Write([]byte("api"))
		}))
		api.Get("/users", func(http.ResponseWriter, *http.Request) {})
	})

	code, body := exec(t, r, http.MethodGet, "/api/missing")
	if code != 404 || body != "api" {
		t.Errorf("api/missing: code=%d body=%q", code, body)
	}
	code, body = exec(t, r, http.MethodGet, "/outside")
	if code != 404 || body != "root" {
		t.Errorf("outside: code=%d body=%q", code, body)
	}
}

func TestRouter_NotFound_LongestPrefixWins(t *testing.T) {
	r := router.New()
	r.NotFound(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("root"))
	}))
	r.Group("/api", func(api *router.Router) {
		api.NotFound(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("api"))
		}))
		api.Group("/v1", func(v1 *router.Router) {
			v1.NotFound(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("v1"))
			}))
		})
	})
	// /api/v1/anything → v1 (most specific)
	_, body := exec(t, r, http.MethodGet, "/api/v1/anything")
	if body != "v1" {
		t.Errorf("v1 case: body=%q, want v1", body)
	}
	// /api/anything → api
	_, body = exec(t, r, http.MethodGet, "/api/anything")
	if body != "api" {
		t.Errorf("api case: body=%q, want api", body)
	}
	// /other → root
	_, body = exec(t, r, http.MethodGet, "/other")
	if body != "root" {
		t.Errorf("root case: body=%q, want root", body)
	}
}

func TestRouter_MethodNotAllowed_Custom(t *testing.T) {
	r := router.New()
	r.Get("/x", func(http.ResponseWriter, *http.Request) {})
	r.MethodNotAllowed(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(405)
		_, _ = w.Write([]byte("custom 405"))
	}))
	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 405 || rec.Body.String() != "custom 405" {
		t.Errorf("code=%d body=%q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Allow") != "GET" {
		t.Errorf("Allow = %q, want GET", rec.Header().Get("Allow"))
	}
}

// --- groups + prefix composition ---

func TestRouter_Group_ComposesPrefix(t *testing.T) {
	r := router.New()
	r.Group("/api", func(api *router.Router) {
		api.Group("/v1", func(v1 *router.Router) {
			v1.Get("/users", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("users"))
			})
		})
	})
	code, body := exec(t, r, http.MethodGet, "/api/v1/users")
	if code != 200 || body != "users" {
		t.Errorf("code=%d body=%q", code, body)
	}
}

func TestRouter_Group_EmptyPatternIsPrefixRoot(t *testing.T) {
	r := router.New()
	r.Group("/api", func(api *router.Router) {
		api.Get("", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("api-root"))
		})
	})
	code, body := exec(t, r, http.MethodGet, "/api")
	if code != 200 || body != "api-root" {
		t.Errorf("code=%d body=%q", code, body)
	}
}

// --- middleware ---

func TestRouter_Use_PreservesOrderOuterToInner(t *testing.T) {
	var calls []string
	record := func(name string) router.Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				calls = append(calls, "before-"+name)
				next.ServeHTTP(w, req)
				calls = append(calls, "after-"+name)
			})
		}
	}
	r := router.New()
	r.Use(record("A"), record("B"))
	r.Get("/x", func(http.ResponseWriter, *http.Request) {
		calls = append(calls, "handler")
	})
	exec(t, r, http.MethodGet, "/x")

	want := []string{"before-A", "before-B", "handler", "after-B", "after-A"}
	if !reflect.DeepEqual(calls, want) {
		t.Errorf("calls = %v, want %v", calls, want)
	}
}

func TestRouter_RootMiddlewareIsGlobalAndAppliesToAllRoutes(t *testing.T) {
	// Root-level Use is global: it wraps the entire dispatch, so
	// it applies to every route regardless of registration order.
	// (This is the path-rewriting / CORS-preflight enabler that
	// changed in the global-middleware redesign.)
	var calls []string
	record := func(name string) router.Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				calls = append(calls, name)
				next.ServeHTTP(w, req)
			})
		}
	}
	r := router.New()
	r.Get("/early", func(http.ResponseWriter, *http.Request) {})
	r.Use(record("global"))
	r.Get("/late", func(http.ResponseWriter, *http.Request) {})

	calls = nil
	exec(t, r, http.MethodGet, "/early")
	if !reflect.DeepEqual(calls, []string{"global"}) {
		t.Errorf("early route should see global mw: %v", calls)
	}
	calls = nil
	exec(t, r, http.MethodGet, "/late")
	if !reflect.DeepEqual(calls, []string{"global"}) {
		t.Errorf("late route should see global mw: %v", calls)
	}
}

func TestRouter_GroupMiddlewareIsPerRouteSnapshot(t *testing.T) {
	// Group-level Use is per-route and snapshot-inherited by
	// sub-groups created at that point. Later additions to the
	// outer group's middleware don't retroactively affect already-
	// created sub-groups or already-registered routes.
	var calls []string
	record := func(name string) router.Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				calls = append(calls, name)
				next.ServeHTTP(w, req)
			})
		}
	}
	r := router.New()
	r.Group("/outer", func(outer *router.Router) {
		outer.Use(record("outer-a"))
		outer.Group("/inner", func(inner *router.Router) {
			inner.Use(record("inner"))
			inner.Get("/x", func(http.ResponseWriter, *http.Request) {})
		})
		// Adding outer-b AFTER inner Group must not affect inner's routes.
		outer.Use(record("outer-b"))
		outer.Get("/y", func(http.ResponseWriter, *http.Request) {})
	})

	calls = nil
	exec(t, r, http.MethodGet, "/outer/inner/x")
	if !reflect.DeepEqual(calls, []string{"outer-a", "inner"}) {
		t.Errorf("inner/x calls = %v, want [outer-a inner] (no outer-b — snapshot)", calls)
	}

	calls = nil
	exec(t, r, http.MethodGet, "/outer/y")
	if !reflect.DeepEqual(calls, []string{"outer-a", "outer-b"}) {
		t.Errorf("outer/y calls = %v, want [outer-a outer-b]", calls)
	}
}

func TestRouter_GroupUseOnlyAffectsLaterRoutes(t *testing.T) {
	// Within a group, Use only affects routes registered AFTER it.
	// (The snapshot semantics — same as chi for group middleware.)
	var calls []string
	mw := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			calls = append(calls, "mw")
			next.ServeHTTP(w, req)
		})
	}
	r := router.New()
	r.Group("/g", func(g *router.Router) {
		g.Get("/early", func(http.ResponseWriter, *http.Request) {})
		g.Use(mw)
		g.Get("/late", func(http.ResponseWriter, *http.Request) {})
	})

	calls = nil
	exec(t, r, http.MethodGet, "/g/early")
	if len(calls) != 0 {
		t.Errorf("early route in group picked up later mw: %v", calls)
	}
	calls = nil
	exec(t, r, http.MethodGet, "/g/late")
	if !reflect.DeepEqual(calls, []string{"mw"}) {
		t.Errorf("late route in group missed mw: %v", calls)
	}
}

func TestRouter_GlobalMiddlewareAppliesTo404(t *testing.T) {
	// Logger / RequestID style middleware on root should log even
	// unmatched requests. Global wrapping makes that work.
	var calls []string
	mw := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			calls = append(calls, "mw")
			next.ServeHTTP(w, req)
		})
	}
	r := router.New()
	r.Use(mw)
	exec(t, r, http.MethodGet, "/unknown")
	if !reflect.DeepEqual(calls, []string{"mw"}) {
		t.Errorf("global mw should run on 404: %v", calls)
	}
}

func TestRouter_GlobalMiddlewareAppliesTo405(t *testing.T) {
	// Symmetric case for the 404 test: global middleware must wrap
	// the 405 path too, otherwise observability falls off the cliff
	// when clients hit registered paths with the wrong verb.
	var calls []string
	mw := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			calls = append(calls, "mw")
			next.ServeHTTP(w, req)
		})
	}
	r := router.New()
	r.Use(mw)
	r.Get("/x", func(http.ResponseWriter, *http.Request) {})

	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code = %d, want 405", rec.Code)
	}
	if !reflect.DeepEqual(calls, []string{"mw"}) {
		t.Errorf("global mw should run on 405: %v", calls)
	}
}

func TestRouter_UseAfterFirstDispatchIsSilentlyIgnored(t *testing.T) {
	// Documented behaviour: the global dispatch chain is cached on
	// first ServeHTTP via sync.Once. Late Use calls don't get into
	// the chain. Pin it so we notice if the caching model changes.
	var calls []string
	mw := func(name string) router.Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				calls = append(calls, name)
				next.ServeHTTP(w, req)
			})
		}
	}
	r := router.New()
	r.Use(mw("early"))
	r.Get("/x", func(http.ResponseWriter, *http.Request) {})

	// First dispatch — locks in the chain.
	calls = nil
	exec(t, r, http.MethodGet, "/x")
	if !reflect.DeepEqual(calls, []string{"early"}) {
		t.Fatalf("first dispatch calls = %v, want [early]", calls)
	}

	// Late addition is appended to the slice but the cached chain
	// is what handles the request.
	r.Use(mw("late"))
	calls = nil
	exec(t, r, http.MethodGet, "/x")
	if !reflect.DeepEqual(calls, []string{"early"}) {
		t.Errorf("after late Use, calls = %v, want [early] (late mw is ignored)", calls)
	}
}

// --- Mount ---

func TestRouter_Mount_NoPrefixStripping(t *testing.T) {
	sub := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte("sub:" + req.URL.Path))
	})
	r := router.New()
	r.Mount("/assets/", sub)
	code, body := exec(t, r, http.MethodGet, "/assets/foo.js")
	if code != 200 || body != "sub:/assets/foo.js" {
		t.Errorf("code=%d body=%q", code, body)
	}
}

func TestRouter_Mount_LongestPatternWinsAgainstStaticRoute(t *testing.T) {
	// Documented overlap behaviour: a static route under a Mount
	// prefix wins for its exact path; everything else under the
	// prefix falls through to the mount. This is stdlib ServeMux's
	// longest-pattern-wins applied to (METHOD path) entries.
	mount := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("mount"))
	})
	r := router.New()
	r.Get("/api/health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("health"))
	})
	r.Mount("/api/", mount)

	_, body := exec(t, r, http.MethodGet, "/api/health")
	if body != "health" {
		t.Errorf("specific path body = %q, want health", body)
	}
	_, body = exec(t, r, http.MethodGet, "/api/anything-else")
	if body != "mount" {
		t.Errorf("fallback body = %q, want mount", body)
	}
}

func TestRouter_Mount_AnyMethodGoesThrough(t *testing.T) {
	sub := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte(req.Method))
	})
	r := router.New()
	r.Mount("/api/", sub)
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
		code, body := exec(t, r, m, "/api/x")
		if code != 200 || body != m {
			t.Errorf("method %s: code=%d body=%q", m, code, body)
		}
	}
}

// --- Path params ---

func TestRouter_PathParam_String(t *testing.T) {
	r := router.New()
	r.Get("/users/{name}", func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte(req.PathValue("name")))
	})
	_, body := exec(t, r, http.MethodGet, "/users/alice")
	if body != "alice" {
		t.Errorf("body=%q, want alice", body)
	}
}

func TestRouter_PathInt_ParsesIntegers(t *testing.T) {
	r := router.New()
	r.Get("/n/{n}", func(w http.ResponseWriter, req *http.Request) {
		n, err := router.PathInt(req, "n")
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		_, _ = w.Write([]byte(strconv.Itoa(n * 2)))
	})
	code, body := exec(t, r, http.MethodGet, "/n/21")
	if code != 200 || body != "42" {
		t.Errorf("code=%d body=%q", code, body)
	}
}

func TestRouter_PathInt_NonNumericReturnsError(t *testing.T) {
	r := router.New()
	r.Get("/n/{n}", func(w http.ResponseWriter, req *http.Request) {
		if _, err := router.PathInt(req, "n"); err != nil {
			http.Error(w, "bad", 400)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	code, _ := exec(t, r, http.MethodGet, "/n/abc")
	if code != 400 {
		t.Errorf("code=%d, want 400 for non-numeric", code)
	}
}

func TestRouter_PathInt64_AndFloat(t *testing.T) {
	r := router.New()
	r.Get("/i64/{n}", func(w http.ResponseWriter, req *http.Request) {
		v, _ := router.PathInt64(req, "n")
		_, _ = w.Write([]byte(strconv.FormatInt(v, 10)))
	})
	r.Get("/f/{n}", func(w http.ResponseWriter, req *http.Request) {
		v, _ := router.PathFloat(req, "n")
		_, _ = w.Write([]byte(strconv.FormatFloat(v, 'f', -1, 64)))
	})
	_, body := exec(t, r, http.MethodGet, "/i64/9999999999")
	if body != "9999999999" {
		t.Errorf("int64 body=%q", body)
	}
	_, body = exec(t, r, http.MethodGet, "/f/3.14")
	if body != "3.14" {
		t.Errorf("float body=%q", body)
	}
}

// --- Walk + introspection ---

func TestRouter_Walk_ListsRegistrationsInOrder(t *testing.T) {
	r := router.New()
	noop := func(http.ResponseWriter, *http.Request) {}
	r.Get("/a", noop)
	r.Post("/a", noop)
	r.Group("/api", func(api *router.Router) {
		api.Get("/b", noop)
	})
	r.Mount("/assets/", http.NotFoundHandler())

	var seen []string
	r.Walk(func(method, pattern string, _, _ http.Handler) {
		seen = append(seen, method+" "+pattern)
	})
	want := []string{"GET /a", "POST /a", "GET /api/b", "ALL /assets/"}
	if !reflect.DeepEqual(seen, want) {
		t.Errorf("seen=%v want=%v", seen, want)
	}
}

// --- Compatibility surface ---

func TestRouter_HandleAcceptsHttpHandler(t *testing.T) {
	// Anything implementing http.Handler must drop in without a
	// conversion (sanity check for net/http compatibility claim).
	var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	r := router.New()
	r.Handle(http.MethodGet, "/x", h)
	_, body := exec(t, r, http.MethodGet, "/x")
	if body != "ok" {
		t.Errorf("body=%q", body)
	}
}

func TestRouter_MiddlewareSignatureMatchesStdlibConvention(t *testing.T) {
	// Anything matching `func(http.Handler) http.Handler` must work
	// as Middleware without conversion. The variable here is the
	// plain stdlib shape, NOT router.Middleware.
	var plain func(http.Handler) http.Handler = func(next http.Handler) http.Handler {
		return next
	}
	r := router.New()
	r.Use(plain)
	r.Get("/x", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	_, body := exec(t, r, http.MethodGet, "/x")
	if body != "ok" {
		t.Errorf("body=%q", body)
	}
}

func TestRouter_PanicsOnEmptyPattern(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic on empty path")
		}
	}()
	r := router.New()
	r.Get("", func(http.ResponseWriter, *http.Request) {})
}

func TestRouter_PanicsOnPatternWithoutLeadingSlash(t *testing.T) {
	defer func() {
		v := recover()
		if v == nil {
			t.Fatal("expected panic on pattern without leading slash")
		}
		msg, _ := v.(string)
		if !strings.Contains(msg, `must start with "/"`) {
			t.Errorf("panic message = %v, want one mentioning the slash requirement", v)
		}
	}()
	r := router.New()
	r.Get("users", func(http.ResponseWriter, *http.Request) {})
}

func TestRouter_PanicsOnMountPrefixWithoutLeadingSlash(t *testing.T) {
	defer func() {
		v := recover()
		if v == nil {
			t.Fatal("expected panic on Mount prefix without leading slash")
		}
		msg, _ := v.(string)
		if !strings.Contains(msg, `must start with "/"`) {
			t.Errorf("panic message = %v, want one mentioning the slash requirement", v)
		}
	}()
	r := router.New()
	r.Mount("static/", http.NotFoundHandler())
}

func TestRouter_GroupWithRelativePrefixIsCaughtAtRouteRegistration(t *testing.T) {
	// Group("api") creates a router with prefix "api" (no slash).
	// The bad state is caught when the first inner route tries to
	// register — the user sees a router-level error, not stdlib's
	// cryptic "invalid pattern" message.
	defer func() {
		v := recover()
		if v == nil {
			t.Fatal("expected panic when inner route is registered under non-slash group prefix")
		}
		msg, _ := v.(string)
		if !strings.Contains(msg, `must start with "/"`) {
			t.Errorf("panic message = %v, want one mentioning the slash requirement", v)
		}
	}()
	r := router.New()
	r.Group("api", func(g *router.Router) {
		g.Get("/users", func(http.ResponseWriter, *http.Request) {})
	})
}

func TestRouter_HandleMethodValidation(t *testing.T) {
	cases := []struct {
		method    string
		wantPanic bool
	}{
		{"GET", false},
		{"POST", false},
		{"PROPFIND", false}, // WebDAV custom method, all uppercase letters
		{"", true},          // empty
		{"GETs", true},      // lowercase letter
		{"GET1", true},      // digit
		{"GET-1", true},     // symbol
		{"GÉT", true},       // non-ASCII
		{"get", true},       // entirely lowercase
		{"Get", true},       // mixed case
		{" GET", true},      // leading whitespace
		{"GET ", true},      // trailing whitespace
	}
	for _, tc := range cases {
		tc := tc
		t.Run(fmt.Sprintf("method=%q", tc.method), func(t *testing.T) {
			defer func() {
				v := recover()
				gotPanic := v != nil
				if gotPanic != tc.wantPanic {
					t.Errorf("method %q: gotPanic=%v, want %v (recovered: %v)",
						tc.method, gotPanic, tc.wantPanic, v)
				}
			}()
			r := router.New()
			r.Handle(tc.method, "/x", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		})
	}
}

func TestRouter_PanicsOnNilMiddleware(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic on nil middleware")
		}
	}()
	r := router.New()
	r.Use(nil)
}

func TestRouter_UseNilMiddlewarePanicMessageIncludesIndex(t *testing.T) {
	// Helpful diagnostic: the panic message should name the
	// offending index so the user can find which entry is nil.
	defer func() {
		v := recover()
		if v == nil {
			t.Fatal("expected panic")
		}
		msg, _ := v.(string)
		if !strings.Contains(msg, "index 1") {
			t.Errorf("panic message = %q, want one mentioning 'index 1' (the offending position)", msg)
		}
	}()
	identity := func(next http.Handler) http.Handler { return next }
	r := router.New()
	r.Use(identity, nil, identity)
}

func TestRouter_HandleMethodValidation_PanicMessageMentionsUppercase(t *testing.T) {
	// Stronger assertion than the table test: a specific bad input
	// must produce a panic that explains the rule. If we ever
	// reword validateMethod's panic, this test catches the drop in
	// guidance quality.
	defer func() {
		v := recover()
		if v == nil {
			t.Fatal("expected panic")
		}
		msg, _ := v.(string)
		if !strings.Contains(msg, "uppercase") {
			t.Errorf("panic message = %q, want one mentioning 'uppercase'", msg)
		}
	}()
	r := router.New()
	r.Handle("GET1", "/x", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
}

func TestRouter_WalkYieldsHandlerWithMiddlewareChained(t *testing.T) {
	// S1: Walk hands back the *wrapped* handler (middleware
	// already applied). Calling the yielded handler must run any
	// per-route middleware that was registered around it.
	var calls []string
	mw := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			calls = append(calls, "mw")
			next.ServeHTTP(w, req)
		})
	}
	r := router.New()
	r.Group("/api", func(g *router.Router) {
		g.Use(mw)
		g.Get("/x", func(w http.ResponseWriter, _ *http.Request) {
			calls = append(calls, "handler")
			_, _ = w.Write([]byte("ok"))
		})
	})

	var got, gotRaw http.Handler
	r.Walk(func(method, pattern string, h, raw http.Handler) {
		if method == http.MethodGet && pattern == "/api/x" {
			got = h
			gotRaw = raw
		}
	})
	if got == nil {
		t.Fatal("Walk did not yield the GET /api/x route")
	}
	got.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/x", nil))
	if !reflect.DeepEqual(calls, []string{"mw", "handler"}) {
		t.Errorf("calls = %v, want [mw handler] (middleware must fire on the Walk-yielded handler)", calls)
	}

	// Raw handler bypasses the middleware chain.
	calls = nil
	gotRaw.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/x", nil))
	if !reflect.DeepEqual(calls, []string{"handler"}) {
		t.Errorf("raw handler should skip middleware: calls = %v, want [handler]", calls)
	}
}

func TestRouter_WalkRawHandlerEnablesTypeIntrospection(t *testing.T) {
	// Concrete S1 use case: a debug command wants to find Mount
	// entries that delegate to an http.FileServer. The wrapped
	// chain handler is opaque (it's a closure); the raw handler
	// is the value the caller passed in.
	fs := http.FileServer(http.Dir("."))
	r := router.New()
	r.Mount("/static/", fs)

	var foundFS bool
	r.Walk(func(method, pattern string, _, raw http.Handler) {
		if pattern != "/static/" {
			return
		}
		// raw should be the exact http.FileServer value.
		if reflect.ValueOf(raw).Pointer() == reflect.ValueOf(fs).Pointer() {
			foundFS = true
		}
	})
	if !foundFS {
		t.Error("Walk's raw handler did not match the http.FileServer that was Mounted")
	}
}

func TestRouter_WalkRawHandlerForHandleIsExactInstance(t *testing.T) {
	// Symmetric to the Mount case: a route registered via Handle
	// (or via a method shortcut, which calls Handle internally)
	// must expose the exact http.Handler instance the caller
	// passed in, not the middleware-wrapped chain.
	orig := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	wrap := func(next http.Handler) http.Handler {
		// Allocates a new closure — guarantees the wrapped chain
		// handler is a distinct value from `orig`.
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req)
		})
	}
	r := router.New()
	r.Group("/api", func(g *router.Router) {
		g.Use(wrap)
		g.Handle(http.MethodGet, "/x", orig)
	})

	var sawHandler, sawRaw http.Handler
	r.Walk(func(method, pattern string, handler, raw http.Handler) {
		if method == http.MethodGet && pattern == "/api/x" {
			sawHandler = handler
			sawRaw = raw
		}
	})
	if sawRaw == nil {
		t.Fatal("Walk did not yield GET /api/x")
	}
	// raw must be the exact orig — not a wrapping closure.
	if reflect.ValueOf(sawRaw).Pointer() != reflect.ValueOf(orig).Pointer() {
		t.Error("raw handler is not the same instance the caller passed to Handle")
	}
	// And handler must be different (it's the wrapped chain).
	if reflect.ValueOf(sawHandler).Pointer() == reflect.ValueOf(orig).Pointer() {
		t.Error("wrapped handler unexpectedly equals raw — group middleware did not wrap")
	}
}

func TestRouter_MountPanicsOnEmptyPrefix(t *testing.T) {
	defer func() {
		v := recover()
		if v == nil {
			t.Fatal(`expected panic on Mount("", ...)`)
		}
		msg, _ := v.(string)
		if !strings.Contains(msg, "configuration bug") {
			t.Errorf("panic message = %q, want one explaining the configuration bug", msg)
		}
	}()
	r := router.New()
	r.Mount("", http.NotFoundHandler())
}

func TestRouter_MountInsideGroupComposesPrefix(t *testing.T) {
	// Mount("/sub/", h) inside r.Group("/api", ...) must route
	// /api/sub/anything to h, with the handler seeing the full
	// path (no prefix stripping). Group-level middleware should
	// also apply.
	var mwCalls []string
	mw := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			mwCalls = append(mwCalls, "mw")
			next.ServeHTTP(w, req)
		})
	}
	sub := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte("sub:" + req.URL.Path))
	})

	r := router.New()
	r.Group("/api", func(api *router.Router) {
		api.Use(mw)
		api.Mount("/sub/", sub)
	})

	// The mount path should match.
	code, body := exec(t, r, http.MethodGet, "/api/sub/foo")
	if code != 200 || body != "sub:/api/sub/foo" {
		t.Errorf("/api/sub/foo: code=%d body=%q, want 200 sub:/api/sub/foo (composed prefix, no stripping)", code, body)
	}
	if !reflect.DeepEqual(mwCalls, []string{"mw"}) {
		t.Errorf("group middleware should fire on mounted route: %v", mwCalls)
	}

	// A path outside the mount's composed prefix should not hit.
	code, _ = exec(t, r, http.MethodGet, "/api/elsewhere")
	if code == 200 {
		t.Errorf("/api/elsewhere should 404, got 200")
	}
}

func TestRouter_MountSlashIsCatchAll(t *testing.T) {
	// Mount("/", h) is a degenerate but valid catch-all: it
	// matches every path the dispatcher reaches, for every method.
	// Pin this behaviour so we notice if it ever changes.
	r := router.New()
	r.Mount("/", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte("mount:" + req.Method + " " + req.URL.Path))
	}))

	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		code, body := exec(t, r, m, "/literally/anywhere")
		if code != 200 || body != "mount:"+m+" /literally/anywhere" {
			t.Errorf("method %s: code=%d body=%q", m, code, body)
		}
	}
}

func TestRouter_PanicsOnDuplicateNotFoundForSameScope(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic on duplicate NotFound for same scope")
		}
	}()
	r := router.New()
	r.NotFound(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	r.NotFound(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
}

func TestRouter_DuplicateNotFoundForDifferentScopesIsFine(t *testing.T) {
	// Different prefixes are independent — root + /api should coexist.
	r := router.New()
	r.NotFound(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	r.Group("/api", func(api *router.Router) {
		api.NotFound(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	})
}

func TestRouter_GroupPanicsOnRootSlashPrefix(t *testing.T) {
	defer func() {
		v := recover()
		if v == nil {
			t.Fatal(`expected panic on Group("/", ...)`)
		}
		msg, _ := v.(string)
		if !strings.Contains(msg, "configuration bug") {
			t.Errorf("panic message = %v, want one explaining the configuration bug", v)
		}
	}()
	r := router.New()
	r.Group("/", func(*router.Router) {})
}

func TestRouter_GroupEmptyPrefixIsValid(t *testing.T) {
	// "" is the chi-style "scope-middleware-without-changing-prefix"
	// idiom and must NOT panic.
	r := router.New()
	r.Group("", func(g *router.Router) {
		g.Get("/x", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("ok"))
		})
	})
	code, body := exec(t, r, http.MethodGet, "/x")
	if code != 200 || body != "ok" {
		t.Errorf(`Group("") should be valid — code=%d body=%q`, code, body)
	}
}

func TestRouter_PanicsOnDuplicateMethodNotAllowed(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic on duplicate MethodNotAllowed")
		}
	}()
	r := router.New()
	r.MethodNotAllowed(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	r.MethodNotAllowed(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
}
