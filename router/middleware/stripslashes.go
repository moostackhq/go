package middleware

import (
	"net/http"
	"strings"

	"github.com/moostackhq/go/router"
)

// StripSlashes returns middleware that strips trailing slashes from
// the request path before downstream matching. /users/ becomes
// /users; the response is the same content as /users without an
// HTTP redirect.
//
// Stdlib [http.ServeMux] treats /users and /users/ as distinct
// patterns, which surprises many users; StripSlashes normalises the
// path so a single registration handles both forms. Place it early
// in the chain so the rewrite is visible to the matcher.
//
// The rewritten path is delivered via a shallow-cloned [*http.Request]
// so the original is not mutated — code upstream of this middleware
// still sees the trailing slash if it inspects [http.Request] later.
//
// The root "/" is left alone — stripping it would produce an empty
// path, which is invalid.
func StripSlashes() router.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(r.URL.Path) <= 1 || !strings.HasSuffix(r.URL.Path, "/") {
				next.ServeHTTP(w, r)
				return
			}
			stripped := strings.TrimRight(r.URL.Path, "/")
			r2 := *r
			u := *r.URL
			u.Path = stripped
			if u.RawPath != "" {
				u.RawPath = strings.TrimRight(u.RawPath, "/")
			}
			r2.URL = &u
			r2.RequestURI = u.RequestURI()
			next.ServeHTTP(w, &r2)
		})
	}
}
