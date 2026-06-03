package assetmapper

import (
	"fmt"
	"io/fs"
	"mime"
	"path"
)

// Asset returns the public URL for the given logical path. In dev
// mode the content is read and hashed on demand (cached for the life
// of the process); in prod mode the manifest is consulted. Returns
// [ErrAssetNotFound] when the logical path is unknown.
//
// Logical paths are forward-slash separated and rooted at the asset
// namespace (no leading slash). Paths with ".." segments are rejected
// as not found.
func (m *Mapper) Asset(logicalPath string) (string, error) {
	cleaned := cleanLogical(logicalPath)
	if cleaned == "" {
		return "", fmt.Errorf("%w: %q", ErrAssetNotFound, logicalPath)
	}

	if m.manifest != nil {
		// Prod mode: trust the manifest. Missing entry is a hard
		// error — the caller compiled with a stale view of the
		// assets.
		hashedRel, ok := m.manifest.Lookup(cleaned)
		if !ok {
			return "", fmt.Errorf("%w: %s (not in manifest)", ErrAssetNotFound, logicalPath)
		}
		return m.urlPrefix + hashedRel, nil
	}

	// Dev mode: resolve, hash on first read, cache.
	c, err := m.loadDev(cleaned)
	if err != nil {
		return "", err
	}
	return m.urlPrefix + hashedName(cleaned, c.hash), nil
}

// loadDev resolves a logical path, returning the cached asset (or
// reading + hashing on first access).
func (m *Mapper) loadDev(logicalPath string) (*cachedAsset, error) {
	if v, ok := m.devCache.Load(logicalPath); ok {
		return v.(*cachedAsset), nil
	}
	root, sub, err := m.resolveFile(logicalPath)
	if err != nil {
		return nil, err
	}
	content, err := fs.ReadFile(root.FS, sub)
	if err != nil {
		return nil, fmt.Errorf("assetmapper: read %s: %w", logicalPath, err)
	}
	c := &cachedAsset{
		content:     content,
		hash:        hashContent(content),
		contentType: contentTypeFor(logicalPath),
	}
	// LoadOrStore: another goroutine may have raced us; if so prefer
	// the existing entry to keep memory aliasing tidy.
	actual, _ := m.devCache.LoadOrStore(logicalPath, c)
	return actual.(*cachedAsset), nil
}

// contentTypeFor picks a MIME type from the file extension. Falls
// back to application/octet-stream for unknown extensions so the
// browser does not sniff the content (which can mis-render text as
// HTML on some servers, an XSS vector).
func contentTypeFor(logicalPath string) string {
	if t := mime.TypeByExtension(path.Ext(logicalPath)); t != "" {
		return t
	}
	return "application/octet-stream"
}

// Invalidate drops any cached (content, hash) for the given logical
// path so the next [Mapper.Asset] or handler request re-reads from
// disk. Intended for dev hot-reload integrations watching the source
// tree with fsnotify; production callers built with a [Manifest]
// don't have a dev cache and Invalidate is a no-op for them.
func (m *Mapper) Invalidate(logicalPath string) {
	cleaned := cleanLogical(logicalPath)
	if cleaned == "" {
		return
	}
	m.devCache.Delete(cleaned)
}

// ClearCache drops every cached asset so subsequent reads re-hash
// from disk. Cheaper than restart for dev tools that detect "many
// files changed at once" (git checkout, branch switch).
//
// Has no effect in prod mode (Mapper built with a Manifest); the
// dev cache is unused there.
func (m *Mapper) ClearCache() {
	m.devCache.Clear()
}
