package csrf

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// Middleware returns CSRF-protection middleware. It plugs into any
// router taking func(http.Handler) http.Handler.
//
//   - Safe methods (GET/HEAD/OPTIONS/TRACE): ensure a token cookie
//     exists (minting one if needed) and stash the request token for
//     [Token]/[Field]. These requests are never rejected.
//   - Unsafe methods: require the Origin (if the browser sent one) — or
//     the Referer on a TLS request — to match the host or a trusted
//     origin; then require a valid cookie and a submitted token (header,
//     then form field) that unmasks to it. Any failure is handled by the
//     configured ErrorHandler (403).
func (p *Protector) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var canonical []byte

		if safeMethod(r.Method) {
			if t, ok := p.parseCookie(cookieValue(r, p.cookie.Name)); ok {
				canonical = t
			} else {
				t, err := newToken()
				if err != nil {
					http.Error(w, "csrf: token generation failed", http.StatusInternalServerError)
					return
				}
				canonical = t
				p.setCookie(w, t)
			}
		} else {
			if !p.originOK(r) {
				p.onError.ServeHTTP(w, r)
				return
			}
			t, ok := p.parseCookie(cookieValue(r, p.cookie.Name))
			if !ok {
				p.onError.ServeHTTP(w, r)
				return
			}
			submitted, ok := unmaskToken(p.submittedToken(r))
			if !ok || !tokensEqual(t, submitted) {
				p.onError.ServeHTTP(w, r)
				return
			}
			canonical = t
		}

		// Stash a fresh masked token so a (re-)rendered form on this
		// response carries a valid value — including a handler that
		// re-renders a form after a validation error.
		masked, err := maskToken(canonical)
		if err != nil {
			http.Error(w, "csrf: token masking failed", http.StatusInternalServerError)
			return
		}
		ctx := context.WithValue(r.Context(), ctxKey{}, requestToken{token: masked, field: p.fieldName})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func safeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

// submittedToken prefers the header (so a JSON body isn't parsed) and
// falls back to the form field.
func (p *Protector) submittedToken(r *http.Request) string {
	if v := r.Header.Get(p.headerName); v != "" {
		return v
	}
	return r.PostFormValue(p.fieldName)
}

func cookieValue(r *http.Request, name string) string {
	if c, err := r.Cookie(name); err == nil {
		return c.Value
	}
	return ""
}

func (p *Protector) setCookie(w http.ResponseWriter, t []byte) {
	http.SetCookie(w, &http.Cookie{
		Name:     p.cookie.Name,
		Value:    p.signCookie(t),
		Path:     p.cookie.Path,
		Domain:   p.cookie.Domain,
		MaxAge:   int(p.cookie.MaxAge / time.Second),
		Secure:   p.cookie.Secure,
		HttpOnly: true,
		SameSite: p.cookie.SameSite,
	})
}

// originOK enforces the Origin/Referer check.
//
// The Origin header is validated whenever it is present — browsers send
// it on state-changing requests regardless of scheme, so this also
// covers deployments behind a TLS-terminating proxy (where r.TLS is nil
// but the request is really HTTPS) without trusting any forwarded
// header. When Origin is absent we fall back to Referer, but only on a
// directly-TLS request: over plain HTTP the Referer is unreliable and
// often stripped, and requiring it would break local/dev clients.
//
// Note: the match is host-based; an http vs https scheme mismatch on
// the same host is not distinguished (exploiting it requires an
// attacker already controlling that host, i.e. an on-path/MITM
// position, against which the token is the real defense).
func (p *Protector) originOK(r *http.Request) bool {
	if origin := r.Header.Get("Origin"); origin != "" {
		return p.originAllowed(origin, r.Host)
	}
	// No Origin: only enforce a Referer match on a direct TLS request.
	if r.TLS == nil {
		return true
	}
	ref := r.Header.Get("Referer")
	if ref == "" {
		return false // HTTPS with neither Origin nor Referer
	}
	u, err := url.Parse(ref)
	if err != nil || u.Host == "" {
		return false
	}
	return p.originAllowed(u.Scheme+"://"+u.Host, r.Host)
}

// originAllowed reports whether origin (a "scheme://host" string) is the
// request host or a configured trusted origin.
func (p *Protector) originAllowed(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if u.Host == host {
		return true
	}
	for _, t := range p.trustedOrigins {
		if origin == t {
			return true
		}
	}
	return false
}
