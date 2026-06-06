package middleware

import (
	"net"
	"net/http"
	"strings"

	"github.com/moostackhq/go/router"
)

// RealIP returns middleware that overwrites [http.Request.RemoteAddr]
// with the client IP extracted from upstream-proxy headers, in this
// order:
//
//  1. X-Real-IP (nginx convention)
//  2. X-Forwarded-For — the leftmost entry (the original client)
//  3. Forwarded — the leftmost "for=" of the RFC 7239 standard
//     header used by Envoy and recent Apache
//
// The original port from RemoteAddr is preserved if present, so
// downstream code that does [net.SplitHostPort] keeps working.
// IPv6 addresses are bracketed as [addr]:port per RFC 3986.
//
// Trust caveat: only enable RealIP when the request actually comes
// from a proxy you control. A direct client can forge any of these
// headers. Stripping them at the edge proxy / load balancer is the
// standard hardening.
func RealIP() router.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ip := realIP(r); ip != "" {
				if _, port, err := net.SplitHostPort(r.RemoteAddr); err == nil && port != "" {
					r.RemoteAddr = net.JoinHostPort(ip, port)
				} else {
					r.RemoteAddr = ip
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

func realIP(r *http.Request) string {
	if h := strings.TrimSpace(r.Header.Get("X-Real-IP")); h != "" {
		return h
	}
	if h := r.Header.Get("X-Forwarded-For"); h != "" {
		// Leftmost is the originating client.
		if i := strings.IndexByte(h, ','); i >= 0 {
			h = h[:i]
		}
		return strings.TrimSpace(h)
	}
	if h := r.Header.Get("Forwarded"); h != "" {
		return forwardedFor(h)
	}
	return ""
}

// forwardedFor returns the leftmost "for=" value from an RFC 7239
// Forwarded header, stripped of quotes and port. Returns "" if
// absent, malformed, obfuscated (RFC 7239 allows opaque tokens
// like for=_hidden or for=unknown that we can't surface as an IP),
// or an unbracketed IPv6 (RFC requires brackets; without them an
// unbracketed string with multiple colons can't be safely split
// into address-and-port).
//
// CR or LF anywhere in the header rejects the whole value — in
// production stdlib's header parser already filters those out at
// request time, but failing closed here defends against the
// httptest.NewRequest path (which is more permissive) and any
// future transport that lets such bytes through.
//
// Format examples it handles:
//
//	for=192.0.2.60
//	for="192.0.2.60:4711"
//	for="[2001:db8::1]:4711"
//	for=192.0.2.60;proto=https;by=203.0.113.43
//	for=192.0.2.60, for=198.51.100.17     (multiple proxies — leftmost wins)
//	For=192.0.2.60                        (param name is case-insensitive)
//	For = 192.0.2.60                      (BWS around "=" per RFC 7230)
func forwardedFor(h string) string {
	if strings.ContainsAny(h, "\r\n") {
		return ""
	}
	// Leftmost forwarded element only.
	if i := strings.IndexByte(h, ','); i >= 0 {
		h = h[:i]
	}
	for _, part := range strings.Split(h, ";") {
		// Each parameter is "key=value" with optional whitespace
		// around the equals sign per RFC 7230 BWS.
		eq := strings.IndexByte(part, '=')
		if eq < 0 {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(part[:eq]), "for") {
			continue
		}
		val := strings.TrimSpace(part[eq+1:])
		if l := len(val); l >= 2 && val[0] == '"' && val[l-1] == '"' {
			val = val[1 : l-1]
		}
		// Bracketed IPv6, optionally with port: [2001:db8::1] or
		// [2001:db8::1]:4711.
		if strings.HasPrefix(val, "[") {
			if end := strings.IndexByte(val, ']'); end > 0 {
				return val[1:end]
			}
			return "" // malformed
		}
		// Unbracketed: must be IPv4 (with optional port). Reject
		// multi-colon unbracketed values — RFC 7239 requires
		// brackets for IPv6, and an unbracketed value like
		// "2001:db8::1:4711" is ambiguous (where does the address
		// end and the port begin?). Jamming it into RemoteAddr
		// downstream would break net.SplitHostPort for everyone.
		switch strings.Count(val, ":") {
		case 0:
			// Bare IPv4 — keep as-is.
		case 1:
			// IPv4 with port — drop the port.
			val = val[:strings.IndexByte(val, ':')]
		default:
			return "" // malformed unbracketed IPv6
		}
		if val == "" || strings.HasPrefix(val, "_") || strings.EqualFold(val, "unknown") {
			return ""
		}
		return val
	}
	return ""
}
