package middleware

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/moostackhq/go/router"
)

// CORSOptions controls [CORS]. The zero value is a permissive
// development default: allow any origin, the common HTTP methods,
// and any request header; do not allow credentials. Override for
// production where you want explicit allowlists.
type CORSOptions struct {
	// AllowedOrigins is the list of origins the browser may make
	// credentialed-or-not requests from. Use ["*"] for any origin
	// (CORS spec forbids combining "*" with AllowCredentials).
	// Nil defaults to ["*"].
	AllowedOrigins []string

	// AllowedMethods is the methods echoed in
	// Access-Control-Allow-Methods on preflight. Nil defaults to
	// GET / POST / PUT / PATCH / DELETE / OPTIONS / HEAD.
	AllowedMethods []string

	// AllowedHeaders is the request headers permitted on actual
	// cross-origin requests. On preflight the intersection of
	// AllowedHeaders with the client's Access-Control-Request-Headers
	// is echoed back, so the browser sees only the names it asked
	// about that we also accept.
	//
	// Wildcard ["*"] echoes the client's requested headers verbatim
	// (which works even on older browsers that don't treat "*" as
	// a header wildcard).
	//
	// When the client sends no Access-Control-Request-Headers, the
	// full configured list is echoed as a discovery aid (handy for
	// "what can I send?" probes via curl -X OPTIONS).
	//
	// Nil defaults to ["*"].
	AllowedHeaders []string

	// ExposedHeaders is the response headers exposed to client
	// JavaScript (the browser hides most non-CORS-safelisted
	// headers by default).
	ExposedHeaders []string

	// AllowCredentials sets Access-Control-Allow-Credentials: true.
	// Forces a non-wildcard origin echo even when AllowedOrigins is
	// ["*"], per the CORS spec.
	AllowCredentials bool

	// MaxAge is the preflight cache lifetime in seconds. 0 leaves
	// the browser default (typically 5 seconds).
	MaxAge int
}

// CORS returns middleware that emits the Cross-Origin Resource
// Sharing headers a browser needs to make XHR / fetch requests
// across origins. Handles preflight (OPTIONS) requests by short-
// circuiting with 204; otherwise lets the request continue down
// the chain with the appropriate response headers attached.
//
// Requests without an Origin header pass through untouched (CORS
// doesn't apply to same-origin or non-browser traffic).
func CORS(opts CORSOptions) router.Middleware {
	// Reject CR / LF in every list that flows into a response
	// header. Go's http.Header.Set silently drops bad values at
	// write time, but failing fast at construction surfaces the
	// bug at boot instead of at first cross-origin request.
	validateNoCRLF("AllowedOrigins", opts.AllowedOrigins)
	validateNoCRLF("AllowedMethods", opts.AllowedMethods)
	validateNoCRLF("AllowedHeaders", opts.AllowedHeaders)
	validateNoCRLF("ExposedHeaders", opts.ExposedHeaders)

	// AllowedMethods must be uppercase ASCII — browsers compare
	// method strings case-sensitively on preflight, so "Get" would
	// silently fail to authorise real GET requests.
	validateMethodsUppercase("AllowedMethods", opts.AllowedMethods)

	if len(opts.AllowedOrigins) == 0 {
		opts.AllowedOrigins = []string{"*"}
	}
	if len(opts.AllowedMethods) == 0 {
		opts.AllowedMethods = []string{
			http.MethodGet, http.MethodPost, http.MethodPut,
			http.MethodPatch, http.MethodDelete,
			http.MethodOptions, http.MethodHead,
		}
	}
	if len(opts.AllowedHeaders) == 0 {
		opts.AllowedHeaders = []string{"*"}
	}

	// "*" wins whenever it appears in AllowedOrigins, even alongside
	// explicit origins — the user clearly wants wildcard access and
	// the explicit entries are redundant rather than restricting.
	allowAny := false
	originSet := make(map[string]struct{}, len(opts.AllowedOrigins))
	for _, o := range opts.AllowedOrigins {
		if o == "*" {
			allowAny = true
		}
		originSet[o] = struct{}{}
	}
	methods := strings.Join(opts.AllowedMethods, ", ")
	headersAllowlist := strings.Join(opts.AllowedHeaders, ", ")
	exposed := strings.Join(opts.ExposedHeaders, ", ")

	// Case-insensitive lookup of allowed headers; track wildcard
	// separately so the preflight branch can echo the requested
	// headers verbatim.
	allowAnyHeader := false
	allowedHeadersLower := make(map[string]struct{}, len(opts.AllowedHeaders))
	for _, h := range opts.AllowedHeaders {
		if h == "*" {
			allowAnyHeader = true
			continue
		}
		allowedHeadersLower[strings.ToLower(h)] = struct{}{}
	}
	maxAge := ""
	if opts.MaxAge > 0 {
		maxAge = strconv.Itoa(opts.MaxAge)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}

			allowed := allowAny
			if !allowed {
				_, allowed = originSet[origin]
			}

			// Always advertise Origin-sensitivity to caches —
			// allowed AND rejected — because the same URL produces
			// different responses depending on the request's Origin.
			// Without this, a cache could serve the no-CORS-headers
			// reject response back to a later request from an
			// allowed origin.
			w.Header().Add("Vary", "Origin")

			if !allowed {
				next.ServeHTTP(w, r)
				return
			}

			// "*" + credentials is illegal per the CORS spec: echo
			// the actual origin instead when credentials are
			// allowed.
			if allowAny && !opts.AllowCredentials {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			} else {
				w.Header().Set("Access-Control-Allow-Origin", origin)
			}
			if opts.AllowCredentials {
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}

			// Preflight: OPTIONS request with the
			// Access-Control-Request-Method header. Short-circuit
			// with 204 and the preflight-only headers.
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				w.Header().Set("Access-Control-Allow-Methods", methods)
				if ah := buildAllowHeaders(
					r.Header.Get("Access-Control-Request-Headers"),
					allowAnyHeader, allowedHeadersLower, headersAllowlist,
				); ah != "" {
					w.Header().Set("Access-Control-Allow-Headers", ah)
				}
				if maxAge != "" {
					w.Header().Set("Access-Control-Max-Age", maxAge)
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}

			// Access-Control-Expose-Headers belongs on actual
			// responses, not preflights — set it after the preflight
			// short-circuit returns.
			if exposed != "" {
				w.Header().Set("Access-Control-Expose-Headers", exposed)
			}

			next.ServeHTTP(w, r)
		})
	}
}

// validateMethodsUppercase panics if any entry is empty or contains
// anything other than uppercase ASCII letters. Mirrors the router's
// own method validation so the diagnostic message is consistent.
//
// field is the CORSOptions struct field name the caller is
// validating (e.g. "AllowedMethods"). It appears verbatim in the
// panic message so the user can find the offending option without
// having to read the stack trace.
func validateMethodsUppercase(field string, values []string) {
	for i, m := range values {
		if m == "" {
			panic(fmt.Sprintf("middleware/cors: %s[%d] is empty", field, i))
		}
		for _, c := range m {
			if c < 'A' || c > 'Z' {
				panic(fmt.Sprintf(
					"middleware/cors: %s[%d] must be uppercase ASCII letters, got %q",
					field, i, m,
				))
			}
		}
	}
}

// validateNoCRLF panics if any entry in values contains a CR or LF
// character. Such bytes in a response-header value enable HTTP
// response-header injection (an attacker-controlled string smuggling
// in a Set-Cookie or another header).
func validateNoCRLF(field string, values []string) {
	for i, v := range values {
		if strings.ContainsAny(v, "\r\n") {
			panic(fmt.Sprintf(
				"middleware/cors: %s[%d] contains CR or LF (header-injection risk): %q",
				field, i, v,
			))
		}
	}
}

// buildAllowHeaders computes the Access-Control-Allow-Headers value
// for one preflight. requested is the client's
// Access-Control-Request-Headers header (may be empty). allowAny
// signals that the allowlist contains "*". allowedLower is a
// case-insensitive set of explicit allowed headers. fallback is the
// pre-joined full allowlist used when the client sent no headers.
//
// Behaviour:
//
//   - requested == "":         emit the full allowlist (discovery).
//   - allowAny == true:        echo the requested headers verbatim
//     (the modern browser interprets "*"
//     but older ones don't — echoing
//     works either way).
//   - explicit allowlist:      intersect requested with the allowlist,
//     preserving the request's order and case.
func buildAllowHeaders(requested string, allowAny bool, allowedLower map[string]struct{}, fallback string) string {
	if requested == "" {
		return fallback
	}
	if allowAny {
		return requested
	}
	parts := strings.Split(requested, ",")
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		name := strings.TrimSpace(p)
		if name == "" {
			continue
		}
		if _, ok := allowedLower[strings.ToLower(name)]; ok {
			kept = append(kept, name)
		}
	}
	return strings.Join(kept, ", ")
}
