package session

import (
	"net/http"
	"time"
)

// Cookie is a [Token] that reads and writes the session ID as an HTTP
// cookie. Empty Name defaults to "sid"; empty Path defaults to "/".
// Other fields take their Go zero values, which means Secure,
// HttpOnly, and SameSite are off unless you set them — production
// deployments should opt in explicitly.
//
// Cookie does not sign or encrypt the session ID. The ID is opaque,
// high-entropy, and meaningful only as a lookup key into the store —
// integrity protection on the value is unnecessary. If a downstream
// store can be tricked into accepting a forged ID, the bug is in the
// store, not the cookie.
type Cookie struct {
	Name     string
	Path     string
	Domain   string
	Secure   bool
	HttpOnly bool
	SameSite http.SameSite
}

// defaultCookieName is used when [Cookie.Name] is empty. The leading
// "__Host-" prefix would force Path=/ and Secure, which is desirable
// but not universally compatible (localhost, non-TLS dev). Plain "sid"
// is the conservative default.
const defaultCookieName = "sid"

func (c Cookie) name() string {
	if c.Name == "" {
		return defaultCookieName
	}
	return c.Name
}

// Read returns the session ID carried by the cookie named [Cookie.Name]
// (or "sid" if unset). If the request happens to carry more than one
// cookie with that name — typically because two paths or domains have
// independent cookies in the browser's jar — Read returns the value
// of whichever the request sent first, matching net/http's
// (*Request).Cookie behaviour. Avoid the ambiguity by keeping a
// single Cookie configuration per manager and not changing Path or
// Domain mid-deploy.
func (c Cookie) Read(r *http.Request) (string, bool) {
	ck, err := r.Cookie(c.name())
	if err != nil || ck.Value == "" {
		return "", false
	}
	return ck.Value, true
}

// Write emits a Set-Cookie header carrying sid. If opts.Expiry is
// non-zero, both the Expires and MaxAge attributes are set from it.
func (c Cookie) Write(w http.ResponseWriter, sid string, opts TokenWriteOptions) {
	ck := &http.Cookie{
		Name:     c.name(),
		Value:    sid,
		Path:     c.cookiePath(),
		Domain:   c.Domain,
		Secure:   c.Secure,
		HttpOnly: c.HttpOnly,
		SameSite: c.SameSite,
	}
	if !opts.Expiry.IsZero() {
		ck.Expires = opts.Expiry
		// MaxAge wins over Expires in modern browsers; set both so
		// older clients also honour the deadline.
		if d := time.Until(opts.Expiry); d > 0 {
			ck.MaxAge = int(d.Seconds())
		}
	}
	http.SetCookie(w, ck)
}

// Clear emits a Set-Cookie header with an empty value and an
// already-elapsed expiry, instructing the client to discard the
// cookie immediately.
func (c Cookie) Clear(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     c.name(),
		Value:    "",
		Path:     c.cookiePath(),
		Domain:   c.Domain,
		Secure:   c.Secure,
		HttpOnly: c.HttpOnly,
		SameSite: c.SameSite,
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
	})
}

func (c Cookie) cookiePath() string {
	if c.Path == "" {
		return "/"
	}
	return c.Path
}
