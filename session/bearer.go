package session

import (
	"net/http"
	"strings"
)

// Bearer is a [Token] that reads the session ID from a request
// header (default "Authorization: Bearer <sid>") and pushes any
// minted or rotated SID to a response header (default
// "X-Session-Token").
//
// Bearer transports have no built-in mechanism for the server to
// hand the client a new token mid-request — unlike cookies, the
// browser does not persist arbitrary response headers. The
// auto-emit on the response header here is a conservative
// fallback: clients that capture it stay in sync; clients that
// ignore it keep working with whatever token they already have
// (until it expires).
//
// Production SPA/mobile apps usually do not rely on this auto-emit
// at all — they hit explicit login/refresh endpoints that return
// the token in the response body and own the token lifecycle. The
// fallback exists so [Multi]-mode hybrids (Cookie + Bearer) and
// quick prototypes work out of the box.
type Bearer struct {
	// ReadHeader is the request header parsed for the inbound
	// session ID. Empty defaults to "Authorization".
	ReadHeader string

	// Scheme is the prefix expected on the read header value (as
	// in "Bearer <sid>"). Empty defaults to "Bearer". Matching is
	// case-insensitive.
	Scheme string

	// WriteHeader is the response header used to convey minted or
	// rotated session IDs back to the client. Empty defaults to
	// "X-Session-Token".
	WriteHeader string
}

const (
	defaultBearerReadHeader  = "Authorization"
	defaultBearerScheme      = "Bearer"
	defaultBearerWriteHeader = "X-Session-Token"
)

func (b Bearer) readHeader() string {
	if b.ReadHeader == "" {
		return defaultBearerReadHeader
	}
	return b.ReadHeader
}

func (b Bearer) scheme() string {
	if b.Scheme == "" {
		return defaultBearerScheme
	}
	return b.Scheme
}

func (b Bearer) writeHeader() string {
	if b.WriteHeader == "" {
		return defaultBearerWriteHeader
	}
	return b.WriteHeader
}

// Read extracts the session ID from the configured request header.
// The header value must be "<Scheme> <sid>" with any single run of
// whitespace as the separator; the scheme match is case-insensitive.
func (b Bearer) Read(r *http.Request) (string, bool) {
	raw := r.Header.Get(b.readHeader())
	if raw == "" {
		return "", false
	}
	// Split on the first run of whitespace. Browser/intermediary
	// implementations vary on whether multiple spaces are
	// preserved, so a strict prefix check is too brittle.
	parts := strings.Fields(raw)
	if len(parts) != 2 {
		return "", false
	}
	if !strings.EqualFold(parts[0], b.scheme()) {
		return "", false
	}
	return parts[1], true
}

// Write sets the configured response header to sid. Expiry from
// [TokenWriteOptions] is ignored — bearer transports have no
// standard expiration mechanism.
func (b Bearer) Write(w http.ResponseWriter, sid string, _ TokenWriteOptions) {
	w.Header().Set(b.writeHeader(), sid)
}

func (b Bearer) Clear(w http.ResponseWriter) {
	// Empty header signals "drop your token." A delete would be
	// indistinguishable from "no change" since clients only see
	// what the server sends, so an explicit empty value is the
	// honest payload.
	w.Header().Set(b.writeHeader(), "")
}

// Multi composes multiple [Token] implementations. Read returns the
// first non-empty value the members produce, in order. Write and
// Clear are applied to every member.
//
// Use it for hybrid apps that accept session IDs over more than one
// transport: e.g. Multi{Cookie{...}, Bearer{}} accepts both a cookie
// (server-side render) and an Authorization header (AJAX), and on
// every commit emits both a Set-Cookie and a X-Session-Token so
// either client mode stays in sync.
type Multi []Token

// Read returns the session ID from the first member that finds
// one, in declaration order. Returns ("", false) when every member
// misses.
func (m Multi) Read(r *http.Request) (string, bool) {
	for _, t := range m {
		if sid, ok := t.Read(r); ok {
			return sid, true
		}
	}
	return "", false
}

// Write fans the call out to every member in declaration order, so
// each transport receives the same sid.
func (m Multi) Write(w http.ResponseWriter, sid string, opts TokenWriteOptions) {
	for _, t := range m {
		t.Write(w, sid, opts)
	}
}

// Clear fans the call out to every member in declaration order.
func (m Multi) Clear(w http.ResponseWriter) {
	for _, t := range m {
		t.Clear(w)
	}
}
