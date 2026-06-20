// Package csrf is stateless CSRF protection for net/http, built on a
// signed double-submit token. There is no server-side storage and no
// dependency on a session: a [Protector] holds an HMAC secret, and the
// token lives in a signed cookie plus the submitted form/header.
//
// Usage is three pieces:
//
//   - [New] + [Protector.Middleware] — mount it once (it works with any
//     router taking func(http.Handler) http.Handler). On unsafe methods
//     it validates the token and rejects on failure; on safe methods it
//     ensures a token exists.
//   - [Token] / [Field] — read the request-scoped token in templates or
//     handlers, to put it in a form or a <meta> tag. They are
//     package-level, so any handler (or another package's templates)
//     can emit a field with only the *http.Request — no Protector
//     reference needed.
//
// It pairs with SameSite=Lax cookies (which block the common cross-site
// POST on their own); the token is the primary, defense-in-depth layer.
//
// Limitations:
//
//   - No session binding. The token is not tied to an authenticated
//     identity, so this does not prevent login-CSRF (an attacker mints
//     a valid token/cookie pair and submits their own credentials to
//     log a victim into the attacker's account). Pair it with another
//     control if that matters for your login flow.
//   - The Origin header is checked whenever present (so a TLS-
//     terminating proxy is covered without trusting forwarded headers);
//     the Referer fallback only runs on a directly-TLS request.
package csrf

import (
	"errors"
	"fmt"
	"html"
	"html/template"
	"net/http"
	"time"
)

// ErrInvalidConfig is returned by [New] for a malformed [Config] — most
// often a Secret shorter than 32 bytes.
var ErrInvalidConfig = errors.New("csrf: invalid config")

const (
	defaultCookieName = "csrf_token"
	defaultFieldName  = "csrf_token"
	defaultHeaderName = "X-CSRF-Token"
	minSecretLen      = 32
	defaultMaxAge     = 12 * time.Hour
)

// Config configures a [Protector].
type Config struct {
	// Secret is the HMAC key authenticating the token cookie. It must
	// be at least 32 bytes and stable across instances and restarts —
	// changing it invalidates every outstanding token. Required.
	Secret []byte

	// Cookie customizes the token cookie. HttpOnly is always enforced;
	// Name defaults to "csrf_token", SameSite to Lax, MaxAge to 12h.
	Cookie CookieOptions

	// FieldName is the form field checked on unsafe requests; defaults
	// to "csrf_token". HeaderName is the header checked (taking
	// precedence over the form so JSON bodies aren't consumed);
	// defaults to "X-CSRF-Token".
	FieldName  string
	HeaderName string

	// TrustedOrigins are extra origins ("https://app.example.com")
	// accepted by the HTTPS Origin/Referer check, beyond same-origin.
	TrustedOrigins []string

	// ErrorHandler handles a rejected request. Defaults to 403 text.
	ErrorHandler http.Handler
}

// CookieOptions customizes the token cookie. HttpOnly is always true —
// the canonical token must not be readable by JS; scripts use [Token]
// (rendered into the page) instead.
type CookieOptions struct {
	Name     string
	Path     string // default "/"
	Domain   string
	Secure   bool // set true behind HTTPS
	SameSite http.SameSite
	MaxAge   time.Duration
}

// Protector validates CSRF tokens. Construct with [New], mount
// [Protector.Middleware], and emit tokens with [Token] / [Field].
type Protector struct {
	secret         []byte
	cookie         CookieOptions
	fieldName      string
	headerName     string
	trustedOrigins []string
	onError        http.Handler
}

// New returns a Protector from cfg, or [ErrInvalidConfig].
func New(cfg Config) (*Protector, error) {
	if len(cfg.Secret) < minSecretLen {
		return nil, fmt.Errorf("%w: Secret must be at least %d bytes, got %d", ErrInvalidConfig, minSecretLen, len(cfg.Secret))
	}
	p := &Protector{
		secret:         append([]byte(nil), cfg.Secret...),
		cookie:         cfg.Cookie,
		fieldName:      orDefault(cfg.FieldName, defaultFieldName),
		headerName:     orDefault(cfg.HeaderName, defaultHeaderName),
		trustedOrigins: cfg.TrustedOrigins,
		onError:        cfg.ErrorHandler,
	}
	p.cookie.Name = orDefault(p.cookie.Name, defaultCookieName)
	p.cookie.Path = orDefault(p.cookie.Path, "/")
	if p.cookie.SameSite == 0 {
		p.cookie.SameSite = http.SameSiteLaxMode
	}
	if p.cookie.MaxAge == 0 {
		p.cookie.MaxAge = defaultMaxAge
	}
	if p.onError == nil {
		p.onError = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "Forbidden - invalid CSRF token", http.StatusForbidden)
		})
	}
	return p, nil
}

// ctxKey keys the request-scoped token; requestToken carries the masked
// token plus the field name so [Field] can build the input element.
type ctxKey struct{}

type requestToken struct {
	token string
	field string
}

// Token returns the masked CSRF token for this request, suitable for a
// hidden field or a <meta> tag (for JS to send as X-CSRF-Token).
// Returns "" when the [Protector.Middleware] is not in the chain.
func Token(r *http.Request) string {
	if rt, ok := r.Context().Value(ctxKey{}).(requestToken); ok {
		return rt.token
	}
	return ""
}

// Field returns a hidden <input> carrying the token, for embedding in a
// form. Returns "" when the middleware is not in the chain.
func Field(r *http.Request) template.HTML {
	rt, ok := r.Context().Value(ctxKey{}).(requestToken)
	if !ok || rt.token == "" {
		return ""
	}
	return template.HTML(`<input type="hidden" name="` +
		html.EscapeString(rt.field) + `" value="` +
		html.EscapeString(rt.token) + `">`)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
