package assetmapper

import (
	"errors"
	"net/http"
	"path"
	"strings"
)

// Handler returns an [http.Handler] that serves dev assets at
// [Mapper.URLPrefix]. The handler resolves the request path back to a
// logical path (stripping the hash segment), reads the source file
// from the configured roots, and writes the response with a
// content-addressed cache header.
//
// In prod mode (the [Mapper] was built with a manifest) the handler
// rejects all requests with 404 — production traffic should be served
// directly from the compiled public directory by a static file
// server, not routed through the Mapper.
//
// The hash in the URL is treated as a pure cache buster: a request
// for a stale hash still returns the current content. The browser's
// next render computes the new URL from the manifest and refetches
// with the up-to-date hash.
func (m *Mapper) Handler() http.Handler {
	return &devHandler{mapper: m}
}

type devHandler struct {
	mapper *Mapper
}

func (h *devHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.mapper.manifest != nil {
		// Prod mode: nothing to serve here.
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Strip the URL prefix; refuse anything that does not live under it.
	if !strings.HasPrefix(r.URL.Path, h.mapper.urlPrefix) {
		http.NotFound(w, r)
		return
	}
	rel := strings.TrimPrefix(r.URL.Path, h.mapper.urlPrefix)
	logical := stripHashSegment(rel)
	if logical == "" {
		http.NotFound(w, r)
		return
	}

	c, err := h.mapper.loadDev(logical)
	if err != nil {
		if errors.Is(err, ErrAssetNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "asset error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", c.contentType)
	// no-cache (not no-store): the browser may keep the response in
	// its cache but must revalidate with the server each request.
	// Combined with ETag this is one round trip + 304 when content
	// is unchanged, and a fresh fetch the moment the dev edits a
	// file (assuming the caller wired [Mapper.Invalidate] / fsnotify
	// to the source tree). Aggressive immutable caching is a prod
	// concern; in prod publicDir is served by a static file server,
	// not this handler.
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", `"`+c.hash+`"`)
	if match := r.Header.Get("If-None-Match"); match == `"`+c.hash+`"` {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(c.content)
}

// stripHashSegment reverses [hashedName]: turns "foo/bar-abc12345.js"
// back into "foo/bar.js" so the Mapper can look it up by its logical
// name. A path without a recognisable hash suffix is returned as-is
// (covers requests for unhashed files like manifest.json, though
// those should be served separately in practice).
func stripHashSegment(rel string) string {
	if rel == "" {
		return ""
	}
	dir, base := path.Split(rel)
	if base == "" {
		return ""
	}
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	// hashedName appends "-HASH" to the stem. Recognise it by length
	// and hex shape — otherwise fall back to returning the relative
	// path unchanged.
	if len(stem) > HashLength+1 {
		dash := stem[len(stem)-HashLength-1]
		hash := stem[len(stem)-HashLength:]
		if dash == '-' && isHex(hash) {
			return dir + stem[:len(stem)-HashLength-1] + ext
		}
	}
	return rel
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(('0' <= c && c <= '9') || ('a' <= c && c <= 'f') || ('A' <= c && c <= 'F')) {
			return false
		}
	}
	return true
}
