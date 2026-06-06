package middleware_test

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moostackhq/go/router"
	"github.com/moostackhq/go/router/middleware"
)

// --- RealIP ---

func TestRealIP_XRealIP(t *testing.T) {
	r := router.New()
	r.Use(middleware.RealIP())
	r.Get("/x", func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte(req.RemoteAddr))
	})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "10.0.0.5:54321"
	req.Header.Set("X-Real-IP", "1.2.3.4")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Body.String() != "1.2.3.4:54321" {
		t.Errorf("RemoteAddr = %q, want 1.2.3.4:54321 (port preserved)", rec.Body.String())
	}
}

func TestRealIP_XForwardedForLeftmost(t *testing.T) {
	r := router.New()
	r.Use(middleware.RealIP())
	r.Get("/x", func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte(req.RemoteAddr))
	})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "10.0.0.5:54321"
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 10.0.0.1, 192.168.1.1")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Body.String() != "1.2.3.4:54321" {
		t.Errorf("RemoteAddr = %q, want 1.2.3.4:54321 (leftmost, port preserved)", rec.Body.String())
	}
}

func TestRealIP_IPv6BracketsPort(t *testing.T) {
	r := router.New()
	r.Use(middleware.RealIP())
	r.Get("/x", func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte(req.RemoteAddr))
	})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "[::1]:54321"
	req.Header.Set("X-Real-IP", "2001:db8::1")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Body.String() != "[2001:db8::1]:54321" {
		t.Errorf("RemoteAddr = %q, want [2001:db8::1]:54321", rec.Body.String())
	}
}

func TestRealIP_Forwarded_VariousFormats(t *testing.T) {
	cases := []struct {
		name     string
		header   string
		wantBody string // RemoteAddr captured by the handler
	}{
		{"plain ipv4", "for=192.0.2.60", "192.0.2.60:54321"},
		{"quoted ipv4 with port", `for="192.0.2.60:4711"`, "192.0.2.60:54321"},
		{"ipv6 bracketed", `for="[2001:db8::1]"`, "[2001:db8::1]:54321"},
		{"ipv6 bracketed with port", `for="[2001:db8::1]:4711"`, "[2001:db8::1]:54321"},
		{"with other params", "for=192.0.2.60;proto=https;by=203.0.113.43", "192.0.2.60:54321"},
		{"multiple proxies leftmost wins", "for=192.0.2.60, for=198.51.100.17", "192.0.2.60:54321"},
		{"case-insensitive token", "For=192.0.2.60", "192.0.2.60:54321"},
		{"obfuscated for=_hidden falls through", "for=_hidden", "10.0.0.5:54321"}, // unchanged → original RemoteAddr
		{"obfuscated for=unknown falls through", "for=unknown", "10.0.0.5:54321"},
		{"unbracketed ipv6 rejected", "for=2001:db8::1", "10.0.0.5:54321"},          // multi-colon unbracketed → reject
		{"unbracketed ipv6+port rejected", `for="2001:db8::1:4711"`, "10.0.0.5:54321"}, // ambiguous → reject
		{"BWS before equals", "for =192.0.2.60", "192.0.2.60:54321"},                // RFC 7230 BWS
		{"BWS after equals", "for= 192.0.2.60", "192.0.2.60:54321"},
		{"BWS both sides", "For  =  192.0.2.60", "192.0.2.60:54321"},
		// CR / LF in the header rejects the whole value (defence in
		// depth against header-injection through Forwarded).
		{"embedded LF rejected", "for=1.2.3.4\n5.6.7.8", "10.0.0.5:54321"},
		{"embedded CR rejected", "for=1.2.3.4\rEvil: 1", "10.0.0.5:54321"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			r := router.New()
			r.Use(middleware.RealIP())
			r.Get("/x", func(w http.ResponseWriter, req *http.Request) {
				_, _ = w.Write([]byte(req.RemoteAddr))
			})
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.RemoteAddr = "10.0.0.5:54321"
			req.Header.Set("Forwarded", tc.header)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Body.String() != tc.wantBody {
				t.Errorf("Forwarded=%q → RemoteAddr=%q, want %q",
					tc.header, rec.Body.String(), tc.wantBody)
			}
		})
	}
}

func TestRealIP_HeaderPriority_XRealIP_Beats_Forwarded(t *testing.T) {
	// When both headers are present, X-Real-IP wins (preserves the
	// existing nginx-first priority; Forwarded is the third
	// fallback, not a replacement).
	r := router.New()
	r.Use(middleware.RealIP())
	r.Get("/x", func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte(req.RemoteAddr))
	})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "10.0.0.5:54321"
	req.Header.Set("X-Real-IP", "1.2.3.4")
	req.Header.Set("Forwarded", "for=9.9.9.9")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Body.String() != "1.2.3.4:54321" {
		t.Errorf("RemoteAddr = %q, want 1.2.3.4:54321 (X-Real-IP wins over Forwarded)",
			rec.Body.String())
	}
}

func TestRealIP_BareIPv6WithoutPortKeptUnbracketed(t *testing.T) {
	// When RemoteAddr has no port (rare but possible), SplitHostPort
	// errors and we fall back to a bare IP. For IPv6, this means
	// the address stays unbracketed — net.JoinHostPort isn't called.
	r := router.New()
	r.Use(middleware.RealIP())
	r.Get("/x", func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte(req.RemoteAddr))
	})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "2001:db8::ff" // no port at all
	req.Header.Set("X-Real-IP", "2001:db8::1")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	// No port to preserve → bare IP, no brackets.
	if rec.Body.String() != "2001:db8::1" {
		t.Errorf("RemoteAddr = %q, want bare 2001:db8::1 (no port to preserve)", rec.Body.String())
	}
}

func TestRealIP_NoHeadersLeavesRemoteAddr(t *testing.T) {
	r := router.New()
	r.Use(middleware.RealIP())
	r.Get("/x", func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte(req.RemoteAddr))
	})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "192.0.2.1:12345"
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Body.String() != "192.0.2.1:12345" {
		t.Errorf("RemoteAddr = %q, want unchanged 192.0.2.1:12345", rec.Body.String())
	}
}

// --- StripSlashes ---

func TestStripSlashes_RewritesTrailingSlash(t *testing.T) {
	r := router.New()
	r.Use(middleware.StripSlashes())
	r.Get("/users", func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte(req.URL.Path))
	})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/users/", nil))
	if rec.Code != 200 || rec.Body.String() != "/users" {
		t.Errorf("code=%d body=%q, want 200 /users", rec.Code, rec.Body.String())
	}
}

func TestStripSlashes_LeavesRootAlone(t *testing.T) {
	r := router.New()
	r.Use(middleware.StripSlashes())
	r.Get("/", func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte(req.URL.Path))
	})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Body.String() != "/" {
		t.Errorf("root path = %q, want /", rec.Body.String())
	}
}

func TestStripSlashes_PathWithoutTrailingSlashUnaffected(t *testing.T) {
	r := router.New()
	r.Use(middleware.StripSlashes())
	r.Get("/users", func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte(req.URL.Path))
	})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/users", nil))
	if rec.Body.String() != "/users" {
		t.Errorf("body = %q, want /users", rec.Body.String())
	}
}

func TestStripSlashes_PreservesQueryString(t *testing.T) {
	// /users/?q=1&p=2 → /users?q=1&p=2 — the path loses its
	// trailing slash, the query stays intact on both URL.RawQuery
	// and the rebuilt RequestURI.
	r := router.New()
	r.Use(middleware.StripSlashes())
	r.Get("/users", func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte(req.URL.Path + "|" + req.URL.RawQuery + "|" + req.RequestURI))
	})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/users/?q=1&p=2", nil))

	want := "/users|q=1&p=2|/users?q=1&p=2"
	if rec.Body.String() != want {
		t.Errorf("body = %q, want %q", rec.Body.String(), want)
	}
}

func TestStripSlashes_DoesNotMutateOriginalRequest(t *testing.T) {
	// Caller's *http.Request must stay untouched — only the value
	// passed downstream sees the rewritten path. Upstream observers
	// (a wrapping middleware that inspects r.URL after next returns,
	// or an audit hook) need the original.
	mw := middleware.StripSlashes()
	called := false
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		called = true
		if req.URL.Path != "/users" {
			t.Errorf("downstream URL.Path = %q, want /users", req.URL.Path)
		}
	}))

	original := httptest.NewRequest(http.MethodGet, "/users/", nil)
	originalPath := original.URL.Path
	originalRequestURI := original.RequestURI
	h.ServeHTTP(httptest.NewRecorder(), original)

	if !called {
		t.Fatal("handler did not run")
	}
	if original.URL.Path != originalPath {
		t.Errorf("original URL.Path mutated: got %q, want %q", original.URL.Path, originalPath)
	}
	if original.RequestURI != originalRequestURI {
		t.Errorf("original RequestURI mutated: got %q, want %q", original.RequestURI, originalRequestURI)
	}
}

// --- CORS ---

func TestCORS_AllowsAnyOriginByDefault(t *testing.T) {
	r := router.New()
	r.Use(middleware.CORS(middleware.CORSOptions{}))
	r.Get("/x", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Origin", "https://example.com")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("Allow-Origin = %q, want *", rec.Header().Get("Access-Control-Allow-Origin"))
	}
}

func TestCORS_WildcardOriginAlsoEmitsVaryOrigin(t *testing.T) {
	// Vary: Origin is set unconditionally on allowed CORS responses
	// so misconfigured caching proxies don't accidentally serve the
	// "*" response back to a credentialed request later.
	r := router.New()
	r.Use(middleware.CORS(middleware.CORSOptions{})) // defaults to ["*"], no credentials
	r.Get("/x", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Origin", "https://example.com")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("Allow-Origin = %q, want *", rec.Header().Get("Access-Control-Allow-Origin"))
	}
	if !strings.Contains(rec.Header().Get("Vary"), "Origin") {
		t.Errorf("Vary = %q, want it to contain Origin even with wildcard Allow-Origin",
			rec.Header().Get("Vary"))
	}
}

func TestCORS_RejectsUnlistedOrigin(t *testing.T) {
	r := router.New()
	r.Use(middleware.CORS(middleware.CORSOptions{
		AllowedOrigins: []string{"https://allowed.example.com"},
	}))
	r.Get("/x", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("unexpected Allow-Origin = %q", rec.Header().Get("Access-Control-Allow-Origin"))
	}
	if rec.Body.String() != "ok" {
		t.Errorf("body = %q, want ok (request still served, no CORS headers)", rec.Body.String())
	}
}

func TestCORS_RejectedOriginStillEmitsVary(t *testing.T) {
	// Even on rejected origins we set Vary: Origin so a cache
	// doesn't serve this no-CORS-headers response back to a later
	// request from an allowed origin under the same URL.
	r := router.New()
	r.Use(middleware.CORS(middleware.CORSOptions{
		AllowedOrigins: []string{"https://allowed.example.com"},
	}))
	r.Get("/x", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("rejected origin must not get Allow-Origin: %q",
			rec.Header().Get("Access-Control-Allow-Origin"))
	}
	if !strings.Contains(rec.Header().Get("Vary"), "Origin") {
		t.Errorf("Vary = %q, want it to contain Origin even on rejected origin",
			rec.Header().Get("Vary"))
	}
}

func TestCORS_PreflightShortCircuit(t *testing.T) {
	r := router.New()
	r.Use(middleware.CORS(middleware.CORSOptions{
		AllowedOrigins: []string{"https://app.example.com"},
		AllowedMethods: []string{http.MethodPost, http.MethodGet},
		AllowedHeaders: []string{"Content-Type", "Authorization"},
		MaxAge:         600,
	}))
	r.Post("/x", func(http.ResponseWriter, *http.Request) {
		t.Error("handler should not run on preflight")
	})

	req := httptest.NewRequest(http.MethodOptions, "/x", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("code = %d, want 204", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Access-Control-Allow-Methods"), "POST") {
		t.Errorf("Allow-Methods = %q", rec.Header().Get("Access-Control-Allow-Methods"))
	}
	if !strings.Contains(rec.Header().Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Errorf("Allow-Headers = %q", rec.Header().Get("Access-Control-Allow-Headers"))
	}
	if rec.Header().Get("Access-Control-Max-Age") != "600" {
		t.Errorf("Max-Age = %q, want 600", rec.Header().Get("Access-Control-Max-Age"))
	}
}

func TestCORS_AllowCredentials_EchoesOriginEvenWithWildcard(t *testing.T) {
	// "*" + credentials is illegal per CORS spec. The middleware
	// should echo the actual origin and add Vary: Origin instead.
	r := router.New()
	r.Use(middleware.CORS(middleware.CORSOptions{
		AllowedOrigins:   []string{"*"},
		AllowCredentials: true,
	}))
	r.Get("/x", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Origin", "https://example.com")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Header().Get("Access-Control-Allow-Origin") != "https://example.com" {
		t.Errorf("Allow-Origin = %q, want echo of origin (not *)",
			rec.Header().Get("Access-Control-Allow-Origin"))
	}
	if rec.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Errorf("Allow-Credentials missing")
	}
	if !strings.Contains(rec.Header().Get("Vary"), "Origin") {
		t.Errorf("Vary = %q, want Origin", rec.Header().Get("Vary"))
	}
}

func TestCORS_PreflightIntersectsRequestedHeadersWithAllowlist(t *testing.T) {
	// Client asks about a subset of our allowed headers AND one we
	// don't allow. Allow-Headers should contain only the subset —
	// the disallowed name must not be echoed.
	r := router.New()
	r.Use(middleware.CORS(middleware.CORSOptions{
		AllowedOrigins: []string{"https://app.example.com"},
		AllowedMethods: []string{http.MethodPost},
		AllowedHeaders: []string{"Content-Type", "Authorization", "X-Custom"},
	}))
	r.Post("/x", func(http.ResponseWriter, *http.Request) {})

	req := httptest.NewRequest(http.MethodOptions, "/x", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "Authorization, X-Forbidden")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	got := rec.Header().Get("Access-Control-Allow-Headers")
	want := "Authorization"
	if got != want {
		t.Errorf("Allow-Headers = %q, want %q (intersection only)", got, want)
	}
}

func TestCORS_PreflightIntersectsCaseInsensitively(t *testing.T) {
	// Header names are case-insensitive — "authorization" (lowercase)
	// in the request must match "Authorization" in the allowlist.
	// The response preserves the case the client used.
	r := router.New()
	r.Use(middleware.CORS(middleware.CORSOptions{
		AllowedOrigins: []string{"https://app.example.com"},
		AllowedHeaders: []string{"Authorization"},
	}))
	r.Post("/x", func(http.ResponseWriter, *http.Request) {})

	req := httptest.NewRequest(http.MethodOptions, "/x", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	got := rec.Header().Get("Access-Control-Allow-Headers")
	if got != "authorization" {
		t.Errorf("Allow-Headers = %q, want %q (matched case-insensitively, request casing preserved)",
			got, "authorization")
	}
}

func TestCORS_PreflightWildcardEchoesRequestedHeaders(t *testing.T) {
	// With AllowedHeaders: ["*"], echo the requested headers
	// verbatim — robust against older browsers that don't honour
	// "*" as a header wildcard.
	r := router.New()
	r.Use(middleware.CORS(middleware.CORSOptions{
		AllowedOrigins: []string{"https://app.example.com"},
		AllowedHeaders: []string{"*"},
	}))
	r.Post("/x", func(http.ResponseWriter, *http.Request) {})

	req := httptest.NewRequest(http.MethodOptions, "/x", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "X-Foo, X-Bar")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	got := rec.Header().Get("Access-Control-Allow-Headers")
	if got != "X-Foo, X-Bar" {
		t.Errorf("Allow-Headers = %q, want %q (wildcard echoes requested)", got, "X-Foo, X-Bar")
	}
}

func TestCORS_PreflightWithoutRequestedHeadersEchoesFullAllowlist(t *testing.T) {
	// Discovery preflight without Access-Control-Request-Headers
	// gets the full configured allowlist for "what can I send?"
	// probes.
	r := router.New()
	r.Use(middleware.CORS(middleware.CORSOptions{
		AllowedOrigins: []string{"https://app.example.com"},
		AllowedHeaders: []string{"Content-Type", "Authorization"},
	}))
	r.Post("/x", func(http.ResponseWriter, *http.Request) {})

	req := httptest.NewRequest(http.MethodOptions, "/x", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	// Note: no Access-Control-Request-Headers.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	got := rec.Header().Get("Access-Control-Allow-Headers")
	if got != "Content-Type, Authorization" {
		t.Errorf("Allow-Headers = %q, want %q (full allowlist as discovery)",
			got, "Content-Type, Authorization")
	}
}

func TestCORS_PanicsOnCRLFInOptions(t *testing.T) {
	cases := []struct {
		name string
		opts middleware.CORSOptions
	}{
		{
			name: "AllowedOrigins",
			opts: middleware.CORSOptions{
				AllowedOrigins: []string{"https://example.com\r\nX-Evil: 1"},
			},
		},
		{
			name: "AllowedMethods",
			opts: middleware.CORSOptions{
				AllowedMethods: []string{"GET\nX-Smuggle: 1"},
			},
		},
		{
			name: "AllowedHeaders",
			opts: middleware.CORSOptions{
				AllowedHeaders: []string{"Authorization\r\nX-Evil: 1"},
			},
		},
		{
			name: "ExposedHeaders",
			opts: middleware.CORSOptions{
				ExposedHeaders: []string{"X-Custom\rX-Evil: 1"},
			},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				v := recover()
				if v == nil {
					t.Fatal("expected panic on CR/LF in option")
				}
				msg, _ := v.(string)
				if !strings.Contains(msg, tc.name) {
					t.Errorf("panic message = %q, want one mentioning %q", msg, tc.name)
				}
				if !strings.Contains(msg, "header-injection") {
					t.Errorf("panic message = %q, want one mentioning 'header-injection'", msg)
				}
			}()
			middleware.CORS(tc.opts)
		})
	}
}

func TestCORS_PanicsOnNonUppercaseAllowedMethods(t *testing.T) {
	cases := []struct {
		name    string
		methods []string
	}{
		{"mixed case", []string{"Get"}},
		{"all lowercase", []string{"get"}},
		{"trailing digit", []string{"GET1"}},
		{"symbol", []string{"GET-1"}},
		{"empty", []string{""}},
		{"valid + invalid", []string{"GET", "Post"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				v := recover()
				if v == nil {
					t.Fatal("expected panic on non-uppercase AllowedMethods entry")
				}
				msg, _ := v.(string)
				if !strings.Contains(msg, "AllowedMethods") {
					t.Errorf("panic message = %q, want one mentioning 'AllowedMethods'", msg)
				}
			}()
			middleware.CORS(middleware.CORSOptions{AllowedMethods: tc.methods})
		})
	}
}

func TestCORS_AcceptsCustomUppercaseMethods(t *testing.T) {
	// WebDAV verbs like PROPFIND are valid uppercase ASCII — they
	// must not panic.
	middleware.CORS(middleware.CORSOptions{
		AllowedMethods: []string{"PROPFIND", "MKCOL", "GET"},
	})
}

func TestCORS_NoOriginPassesThroughUntouched(t *testing.T) {
	r := router.New()
	r.Use(middleware.CORS(middleware.CORSOptions{}))
	r.Get("/x", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("non-CORS request got CORS header: %q", rec.Header().Get("Access-Control-Allow-Origin"))
	}
}

// --- Compress ---

func TestCompress_GzipsWhenClientAccepts(t *testing.T) {
	// Body is 2400 bytes — comfortably over the default 1024 MinSize.
	payload := bytes.Repeat([]byte("hello "), 400)

	r := router.New()
	r.Use(middleware.Compress())
	r.Get("/x", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", rec.Header().Get("Content-Encoding"))
	}
	if rec.Header().Get("Content-Length") != "" {
		t.Errorf("Content-Length should be cleared, got %q", rec.Header().Get("Content-Length"))
	}
	if !strings.Contains(rec.Header().Get("Vary"), "Accept-Encoding") {
		t.Errorf("Vary = %q", rec.Header().Get("Vary"))
	}

	// Body should be gzipped — decode + compare.
	gz, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("body is not gzip: %v", err)
	}
	body, _ := io.ReadAll(gz)
	if !bytes.Equal(body, payload) {
		t.Errorf("decompressed body mismatch")
	}
}

func TestCompress_SkipsWhenClientDoesNotAccept(t *testing.T) {
	r := router.New()
	r.Use(middleware.Compress())
	r.Get("/x", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("plain"))
	})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	// No Accept-Encoding header.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") != "" {
		t.Errorf("unexpected Content-Encoding = %q", rec.Header().Get("Content-Encoding"))
	}
	if rec.Body.String() != "plain" {
		t.Errorf("body = %q, want plain", rec.Body.String())
	}
}

func TestCompress_AcceptsGzipWithQValue(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 2048)
	r := router.New()
	r.Use(middleware.Compress())
	r.Get("/x", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Accept-Encoding", "br;q=0.9, gzip;q=0.8, deflate;q=0.5")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", rec.Header().Get("Content-Encoding"))
	}
}

func TestCompress_SkipsBelowMinSize(t *testing.T) {
	// A 50-byte response should pass through uncompressed (default
	// MinSize is 1024).
	r := router.New()
	r.Use(middleware.Compress())
	r.Get("/x", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("tiny response, nothing worth gzipping"))
	})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") != "" {
		t.Errorf("under-threshold body should not be compressed, got Content-Encoding=%q",
			rec.Header().Get("Content-Encoding"))
	}
	if !strings.HasPrefix(rec.Body.String(), "tiny response") {
		t.Errorf("body = %q, want plaintext", rec.Body.String())
	}
}

func TestCompress_SkipsWhenContentLengthBelowMinSize(t *testing.T) {
	// Handler sets a Content-Length that's well under MinSize.
	// Compress should short-circuit before buffering.
	r := router.New()
	r.Use(middleware.Compress())
	r.Get("/x", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "5")
		_, _ = w.Write([]byte("small"))
	})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") != "" {
		t.Errorf("Content-Encoding should be empty when CL < MinSize, got %q",
			rec.Header().Get("Content-Encoding"))
	}
	if rec.Body.String() != "small" {
		t.Errorf("body = %q, want small", rec.Body.String())
	}
}

func TestCompress_DoesNotDoubleEncodeWhenHandlerSetsContentEncoding(t *testing.T) {
	// Handler emits its own pre-encoded body and sets
	// Content-Encoding; Compress must pass through verbatim.
	preEncoded := []byte("\x1f\x8b\x08\x00<<pretend gzip>>")
	r := router.New()
	r.Use(middleware.Compress())
	r.Get("/x", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(preEncoded)
	})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip (handler's own)",
			rec.Header().Get("Content-Encoding"))
	}
	if !bytes.Equal(rec.Body.Bytes(), preEncoded) {
		t.Errorf("body = %x, want %x (pass-through)", rec.Body.Bytes(), preEncoded)
	}
}

func TestCompress_PreservesContentTypeSniffingUnderMinSize(t *testing.T) {
	// Regression: an earlier finish() pre-committed the header on
	// the under-threshold path, killing stdlib's Content-Type
	// detection. The wire response should look the same as one
	// without Compress (modulo the Vary header, which we don't
	// set in the bypass-through-finish branch).
	r := router.New()
	r.Use(middleware.Compress())
	r.Get("/x", func(w http.ResponseWriter, _ *http.Request) {
		// Tiny HTML body. No explicit Content-Type, no Content-Length,
		// no WriteHeader. Stdlib should sniff this as text/html.
		_, _ = w.Write([]byte("<html><body>hi</body></html>"))
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") != "" {
		t.Fatalf("under-threshold body should not be compressed, got Content-Encoding=%q",
			rec.Header().Get("Content-Encoding"))
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html prefix (sniffed by stdlib on first Write)", ct)
	}
}

func TestCompress_WriteAfter101IsPassedThrough(t *testing.T) {
	// S1 follow-up: if a handler writes through the wrapper AFTER
	// sending 101 (instead of the normal pattern of 101 then
	// Hijack), the wrapper must not start a gzip stream — that
	// would mix gzip framing into bytes the client interprets as
	// the new protocol's data. 101 forces bypass mode so writes
	// pass straight to the underlying writer.
	r := router.New()
	r.Use(middleware.Compress())
	r.Get("/upgrade", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusSwitchingProtocols)
		// Unusual but legal — handler writes after 101 without Hijack.
		_, _ = w.Write([]byte("raw-protocol-bytes"))
	})

	req := httptest.NewRequest(http.MethodGet, "/upgrade", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusSwitchingProtocols {
		t.Errorf("status = %d, want 101", rec.Code)
	}
	if rec.Header().Get("Content-Encoding") == "gzip" {
		t.Errorf("Content-Encoding = gzip after 101 — must NOT compress, the wrapper had no business interpreting the post-101 stream")
	}
	// Body should be the raw bytes verbatim (not gzipped).
	if rec.Body.String() != "raw-protocol-bytes" {
		t.Errorf("body = %q, want raw bytes verbatim", rec.Body.String())
	}
}

func TestCompress_WriteHeader101FlushesBeforeHijack(t *testing.T) {
	// B1 regression: a naive WebSocket-style handler that does
	// WriteHeader(101) then Hijack expects the 101 status line on
	// the wire before the conn handover. Compress used to defer
	// the WriteHeader, so Hijack succeeded without underlying ever
	// receiving the 101 — client hangs waiting for handshake.
	// 1xx must flow straight through.
	r := router.New()
	r.Use(middleware.Compress())
	r.Get("/upgrade", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusSwitchingProtocols)
		if h, ok := w.(http.Hijacker); ok {
			_, _, _ = h.Hijack()
		}
	})

	rec := newFlushHijackRecorder()
	req := httptest.NewRequest(http.MethodGet, "/upgrade", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusSwitchingProtocols {
		t.Errorf("status = %d, want 101 — must reach underlying writer BEFORE Hijack hands off the conn", rec.Code)
	}
	if !rec.hijacked {
		t.Error("Hijack did not reach underlying writer")
	}
}

func TestCompress_FirstWriteHeaderWinsBeforeCommit(t *testing.T) {
	// Stdlib's contract is "first WriteHeader wins, subsequent
	// ignored (with a warning)". Compress defers the underlying
	// WriteHeader until it decides bypass vs. compress vs. buffer,
	// which opens a window where multiple WriteHeader calls happen
	// before commit. The guard ensures the FIRST one is what
	// reaches the wire — same as stdlib.
	r := router.New()
	r.Use(middleware.Compress())
	r.Get("/x", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.WriteHeader(http.StatusInternalServerError) // should be ignored
		_, _ = w.Write([]byte("ok"))
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (first WriteHeader wins, matching stdlib)", rec.Code)
	}
}

func TestCompress_PreservesExplicitStatusOnUnderThresholdBody(t *testing.T) {
	// Regression: when a handler calls WriteHeader(500) and then
	// writes a small body that stays under MinSize, the wire status
	// must be 500. An earlier version of finish() wrote the buffer
	// without committing the stored status, so stdlib's implicit
	// WriteHeader(200) won and the 500 was lost.
	r := router.New()
	r.Use(middleware.Compress())
	r.Get("/x", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("err"))
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (handler-set status must survive Compress under MinSize)", rec.Code)
	}
	if rec.Body.String() != "err" {
		t.Errorf("body = %q, want err", rec.Body.String())
	}
	if rec.Header().Get("Content-Encoding") != "" {
		t.Errorf("under-threshold body should pass through uncompressed, got Content-Encoding=%q",
			rec.Header().Get("Content-Encoding"))
	}
}

func TestCompress_LargeSingleWriteDoesNotBalloonBuffer(t *testing.T) {
	// B2 smoke test: a single Write that crosses MinSize must
	// route through gz directly, not first grow the internal
	// buffer to len(p). We can't directly measure peak buffer
	// size from outside, but we can confirm the round-trip works
	// for a large payload — the optimisation must not corrupt the
	// output.
	payload := bytes.Repeat([]byte("x"), 256*1024) // 256 KiB in one Write

	r := router.New()
	r.Use(middleware.Compress())
	r.Get("/x", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", rec.Header().Get("Content-Encoding"))
	}
	gz, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("body is not gzip: %v", err)
	}
	body, _ := io.ReadAll(gz)
	if !bytes.Equal(body, payload) {
		t.Errorf("decompressed body mismatch (large single Write through optimisation path)")
	}
}

func TestCompress_FlushDuringBufferingDeliversBytes(t *testing.T) {
	// SSE-style: handler writes a small payload under MinSize and
	// flushes. The Compress middleware must not hold the bytes
	// until handler return — flush has to commit and emit them.
	r := router.New()
	r.Use(middleware.Compress())
	r.Get("/sse", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("event: tick\ndata: 1\n\n"))
		w.(http.Flusher).Flush()
		// Handler does NOT return immediately in real SSE; we
		// simulate the "client reads what's been flushed so far"
		// situation by returning here after the Flush call.
	})

	req := httptest.NewRequest(http.MethodGet, "/sse", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// After Flush, we must have committed to compressing (the
	// alternative — buffering — would mean the underlying writer
	// never saw the bytes during the flush window).
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip after flush",
			rec.Header().Get("Content-Encoding"))
	}
	if rec.Body.Len() == 0 {
		t.Fatal("nothing was written to the underlying writer — Flush did not commit buffered bytes")
	}
	gz, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("body is not gzip: %v", err)
	}
	body, _ := io.ReadAll(gz)
	if string(body) != "event: tick\ndata: 1\n\n" {
		t.Errorf("decompressed body = %q, want SSE payload", string(body))
	}
}

func TestCompress_WriteReturnsLenPOnDownstreamError(t *testing.T) {
	// Contract regression: if the inner gzip.Write returns an
	// error (rare in practice, but possible if the underlying
	// writer is closed), Write must still report len(p) bytes
	// "accepted" — the bytes were absorbed into our buffer before
	// the flush attempt, and reporting 0 would tempt the caller
	// to retry and double-write.
	r := router.New()
	r.Use(middleware.CompressWith(middleware.CompressOptions{MinSize: -1}))
	r.Get("/x", func(w http.ResponseWriter, _ *http.Request) {
		n, err := w.Write([]byte("hello"))
		if n != 5 {
			t.Errorf("Write returned n=%d, want 5 (len of input)", n)
		}
		_ = err // a real downstream error is hard to synthesise via httptest; the n contract is the load-bearing check
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
}

func TestCompressWith_MinSizeNegativeCompressesEverything(t *testing.T) {
	r := router.New()
	r.Use(middleware.CompressWith(middleware.CompressOptions{MinSize: -1}))
	r.Get("/x", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("tiny"))
	})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip (MinSize=-1 forces compression)",
			rec.Header().Get("Content-Encoding"))
	}
	gz, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("body is not gzip: %v", err)
	}
	body, _ := io.ReadAll(gz)
	if string(body) != "tiny" {
		t.Errorf("decompressed body = %q, want tiny", string(body))
	}
}
