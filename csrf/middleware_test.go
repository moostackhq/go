package csrf

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

// issueToken drives a GET through the middleware and returns the token
// cookie and the masked request token a form would carry.
func issueToken(t *testing.T, p *Protector) (*http.Cookie, string) {
	t.Helper()
	var token string
	h := p.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token = Token(r)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	var ck *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == defaultCookieName {
			ck = c
		}
	}
	if ck == nil {
		t.Fatal("GET did not set a CSRF cookie")
	}
	if token == "" {
		t.Fatal("Token(r) empty on a safe request")
	}
	return ck, token
}

func TestSafeMethod_MintsCookieAndField(t *testing.T) {
	p, _ := New(Config{Secret: testSecret()})
	var field string
	h := p.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		field = string(Field(r))
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code == http.StatusForbidden {
		t.Fatal("safe method rejected")
	}
	if !strings.Contains(field, `type="hidden"`) || !strings.Contains(field, `name="csrf_token"`) {
		t.Errorf("Field did not render a hidden input: %q", field)
	}
}

func TestValidPOST_FormAndHeader(t *testing.T) {
	p, _ := New(Config{Secret: testSecret()})
	ck, token := issueToken(t, p)
	h := p.Middleware(okHandler())

	t.Run("form field", func(t *testing.T) {
		body := url.Values{"csrf_token": {token}}.Encode()
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(ck)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200", rec.Code)
		}
	})

	t.Run("header", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Header.Set("X-CSRF-Token", token)
		req.AddCookie(ck)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200", rec.Code)
		}
	})
}

func TestUnsafe_Rejections(t *testing.T) {
	p, _ := New(Config{Secret: testSecret()})
	ck, token := issueToken(t, p)
	h := p.Middleware(okHandler())

	post := func(setup func(*http.Request)) int {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		setup(req)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	cases := map[string]func(*http.Request){
		"no cookie, no token": func(r *http.Request) {},
		"cookie, no token":    func(r *http.Request) { r.AddCookie(ck) },
		"token, no cookie":    func(r *http.Request) { r.Header.Set("X-CSRF-Token", token) },
		"wrong token": func(r *http.Request) {
			r.AddCookie(ck)
			r.Header.Set("X-CSRF-Token", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
		},
		"tampered cookie": func(r *http.Request) {
			bad := *ck
			bad.Value = ck.Value[:len(ck.Value)-2] + "AA"
			r.AddCookie(&bad)
			r.Header.Set("X-CSRF-Token", token)
		},
	}
	for name, setup := range cases {
		if code := post(setup); code != http.StatusForbidden {
			t.Errorf("%s: code = %d, want 403", name, code)
		}
	}
}

func TestSafeMethods_NeverRejected(t *testing.T) {
	p, _ := New(Config{Secret: testSecret()})
	h := p.Middleware(okHandler())
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(m, "/", nil)) // no cookie, no token
		if rec.Code == http.StatusForbidden {
			t.Errorf("%s rejected, want allowed", m)
		}
	}
}

func TestOriginCheck_HTTPS(t *testing.T) {
	p, _ := New(Config{Secret: testSecret(), TrustedOrigins: []string{"https://trusted.example"}})
	ck, token := issueToken(t, p)
	h := p.Middleware(okHandler())

	post := func(origin string) int {
		req := httptest.NewRequest(http.MethodPost, "https://example.com/", nil)
		req.TLS = &tls.ConnectionState{} // mark as HTTPS
		req.Host = "example.com"
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		req.Header.Set("X-CSRF-Token", token)
		req.AddCookie(ck)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	if post("https://example.com") != http.StatusOK {
		t.Error("same-origin POST rejected")
	}
	if post("https://trusted.example") != http.StatusOK {
		t.Error("trusted-origin POST rejected")
	}
	if post("https://evil.example") != http.StatusForbidden {
		t.Error("cross-origin POST allowed, want 403")
	}
	if post("") != http.StatusForbidden {
		t.Error("HTTPS POST with no Origin/Referer allowed, want 403")
	}
}

// TestOriginCheck_OriginWhenPresentAndRefererFallback covers the fix
// for the TLS-terminating-proxy gap: the Origin header is validated
// whenever present (even without r.TLS), while the Referer fallback is
// only enforced on a direct TLS request.
func TestOriginCheck_OriginWhenPresentAndRefererFallback(t *testing.T) {
	p, _ := New(Config{Secret: testSecret(), TrustedOrigins: []string{"https://trusted.example"}})
	ck, token := issueToken(t, p)
	h := p.Middleware(okHandler())

	do := func(useTLS bool, origin, referer string) int {
		req := httptest.NewRequest(http.MethodPost, "http://example.com/", nil)
		req.Host = "example.com"
		if useTLS {
			req.TLS = &tls.ConnectionState{}
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if referer != "" {
			req.Header.Set("Referer", referer)
		}
		req.Header.Set("X-CSRF-Token", token)
		req.AddCookie(ck)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	// Origin is checked even without r.TLS (proxy-terminated HTTPS).
	if do(false, "http://evil.example", "") != http.StatusForbidden {
		t.Error("cross-origin POST over plain HTTP allowed — proxy gap not closed")
	}
	if do(false, "http://example.com", "") != http.StatusOK {
		t.Error("same-origin POST over HTTP rejected")
	}
	if do(false, "https://trusted.example", "") != http.StatusOK {
		t.Error("trusted origin rejected")
	}
	// No Origin over plain HTTP: skipped (dev / non-browser clients).
	if do(false, "", "") != http.StatusOK {
		t.Error("HTTP POST with no Origin rejected; should skip")
	}
	// Referer fallback only kicks in on a direct TLS request.
	if do(true, "", "https://example.com/login") != http.StatusOK {
		t.Error("TLS Referer match rejected")
	}
	if do(true, "", "https://evil.example/x") != http.StatusForbidden {
		t.Error("TLS Referer mismatch allowed")
	}
	if do(true, "", "") != http.StatusForbidden {
		t.Error("TLS POST with neither Origin nor Referer allowed")
	}
}

func TestCookie_Attributes(t *testing.T) {
	p, _ := New(Config{Secret: testSecret(), Cookie: CookieOptions{Secure: true, MaxAge: 2 * time.Hour}})
	h := p.Middleware(okHandler())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	var ck *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == defaultCookieName {
			ck = c
		}
	}
	if ck == nil {
		t.Fatal("no CSRF cookie set")
	}
	if !ck.HttpOnly {
		t.Error("cookie not HttpOnly")
	}
	if !ck.Secure {
		t.Error("cookie not Secure when configured")
	}
	if ck.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", ck.SameSite)
	}
	if want := int(2 * time.Hour / time.Second); ck.MaxAge != want {
		t.Errorf("MaxAge = %d, want %d", ck.MaxAge, want)
	}
}

func TestCookieReusedAcrossRequests(t *testing.T) {
	p, _ := New(Config{Secret: testSecret()})
	ck, _ := issueToken(t, p)

	// A second GET that already carries the cookie must not re-set it.
	var reissued bool
	h := p.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(ck)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Name == defaultCookieName {
			reissued = true
		}
	}
	if reissued {
		t.Error("cookie re-set on a request that already had a valid one")
	}
}
