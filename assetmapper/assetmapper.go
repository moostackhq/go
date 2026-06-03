// Package assetmapper maps logical asset paths to versioned public URLs
// without a JS bundler. Inspired by Symfony's AssetMapper component: in
// development the asset source is served directly with content-hashed
// URLs; in production a compile step copies every asset to a public
// directory with the hash embedded in the filename and writes a
// manifest the runtime reads to resolve URLs.
//
// Surface:
//
//   - [Mapper] resolves logical paths to public URLs and (in dev mode)
//     serves the source files via [Mapper.Handler].
//   - [Compile] walks every asset, hashes, rewrites internal JS / CSS
//     references to point at hashed filenames, and writes a manifest.
//   - [Importmap] renders the browser's importmap, modulepreload links,
//     and entrypoint script tags.
//   - [Vendor] downloads JS packages via jspm.io into assets/vendor
//     and registers them in the importmap.
//
// The HTTP and template surfaces ship as plain interfaces
// ([http.Handler] and html/template helpers) so users can wire the
// mapper into any router or template setup.
package assetmapper

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"sync"
)

// ErrAssetNotFound is returned by [Mapper.Asset] when the logical path
// is not registered in any configured root (dev mode) or absent from
// the manifest (prod mode).
var ErrAssetNotFound = errors.New("asset not found")

// Root binds an [fs.FS] (typically [embed.FS] or [os.DirFS]) into the
// mapper's logical namespace. Multiple roots are searched in the order
// they were configured; the first match wins, so user-supplied assets
// can shadow library-supplied defaults by being listed first.
//
// MountAt prefixes every file path in this root with the given
// segment. A satellite library shipping its own assets typically picks
// a unique mount (e.g. "jobs") so its file names cannot collide with
// the user's:
//
//	//go:embed assets/*
//	var jobsAssets embed.FS
//
//	Root{FS: jobsAssets, MountAt: "jobs"}  // files appear as "jobs/foo.css"
//
// Empty MountAt mounts the root directly at the logical namespace
// root.
type Root struct {
	FS      fs.FS
	MountAt string
}

// Config controls [New]. URLPrefix defaults to "/assets/" when empty.
// Manifest is nil in dev mode and non-nil in prod mode; callers in
// prod load it from disk (see [Manifest.Load]) and pass it here.
type Config struct {
	Roots     []Root
	URLPrefix string
	Manifest  *Manifest
}

// Mapper is the runtime entry point. Construct with [New]; resolve
// asset URLs with [Mapper.Asset]; serve dev assets with
// [Mapper.Handler].
//
// A Mapper is safe for concurrent use.
type Mapper struct {
	roots     []Root
	urlPrefix string
	manifest  *Manifest // nil = dev mode

	// devCache memoises (content, hash, contentType) per logical path
	// so repeated Asset/Handler calls do not re-read or re-hash the
	// file. Restart invalidates; that is the explicit dev contract.
	devCache sync.Map // map[string]*cachedAsset
}

type cachedAsset struct {
	content     []byte
	hash        string
	contentType string
}

// New constructs a Mapper.
func New(cfg Config) (*Mapper, error) {
	if len(cfg.Roots) == 0 {
		return nil, fmt.Errorf("assetmapper.New: at least one Root is required")
	}
	for i, r := range cfg.Roots {
		if r.FS == nil {
			return nil, fmt.Errorf("assetmapper.New: Roots[%d].FS is nil", i)
		}
		if err := validateMount(r.MountAt); err != nil {
			return nil, fmt.Errorf("assetmapper.New: Roots[%d].MountAt: %w", i, err)
		}
	}
	prefix, err := normalizeURLPrefix(cfg.URLPrefix)
	if err != nil {
		return nil, fmt.Errorf("assetmapper.New: %w", err)
	}
	// When loading a manifest, refuse to start if the prefix it was
	// compiled with disagrees with what we'll be serving — otherwise
	// rewritten refs in JS / CSS point at one URL while Mapper.Asset
	// hands callers a different one, and the page half-loads.
	// Lenient when manifest.URLPrefix is empty (hand-built test
	// fixtures don't need to set it).
	if cfg.Manifest != nil && cfg.Manifest.URLPrefix != "" {
		manifestPrefix, err := normalizeURLPrefix(cfg.Manifest.URLPrefix)
		if err != nil {
			return nil, fmt.Errorf("assetmapper.New: manifest URLPrefix invalid: %w", err)
		}
		if manifestPrefix != prefix {
			return nil, fmt.Errorf("assetmapper.New: URLPrefix mismatch: Config %q vs Manifest %q (the manifest was compiled with a different prefix — recompile or set Config.URLPrefix to match)", prefix, manifestPrefix)
		}
	}
	return &Mapper{
		roots:     cfg.Roots,
		urlPrefix: prefix,
		manifest:  cfg.Manifest,
	}, nil
}

// normalizeURLPrefix applies the canonical normalisation for an asset
// URL prefix: defaults empty to "/assets/", requires a leading slash,
// collapses repeated slashes via path.Clean, and re-adds the trailing
// slash that path.Clean strips. Shared between [New] and [Compile]
// so both produce identical strings for comparison.
func normalizeURLPrefix(p string) (string, error) {
	if p == "" {
		p = "/assets/"
	}
	if !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("URLPrefix %q must start with /", p)
	}
	cleaned := path.Clean(p)
	if !strings.HasSuffix(cleaned, "/") {
		cleaned += "/"
	}
	return cleaned, nil
}

// URLPrefix returns the resolved URL prefix (always with a trailing
// slash). Useful when wiring the [Mapper.Handler] into a router.
func (m *Mapper) URLPrefix() string { return m.urlPrefix }

// resolveFile locates the logical path in the configured roots,
// returning the matching Root and the file path inside that root's
// fs.FS. Returns ErrAssetNotFound when no root contains the path.
//
// MountAt is stripped from logicalPath when matched: a logical path
// "jobs/foo.css" against a root mounted at "jobs" resolves to "foo.css"
// inside that root's fs.FS.
func (m *Mapper) resolveFile(logicalPath string) (Root, string, error) {
	cleaned := cleanLogical(logicalPath)
	if cleaned == "" {
		return Root{}, "", fmt.Errorf("%w: empty logical path", ErrAssetNotFound)
	}
	for _, r := range m.roots {
		sub := cleaned
		if r.MountAt != "" {
			if !strings.HasPrefix(cleaned, r.MountAt+"/") && cleaned != r.MountAt {
				continue
			}
			sub = strings.TrimPrefix(cleaned, r.MountAt)
			sub = strings.TrimPrefix(sub, "/")
			if sub == "" {
				// Logical path is exactly the mount; only directories
				// can sit at the mount root, so this is not an asset.
				continue
			}
		}
		if _, err := fs.Stat(r.FS, sub); err == nil {
			return r, sub, nil
		}
	}
	return Root{}, "", fmt.Errorf("%w: %s", ErrAssetNotFound, logicalPath)
}

// cleanLogical normalises a logical path: forward slashes, no leading
// slash, no "." or ".." segments. Returns "" for an invalid path.
func cleanLogical(p string) string {
	if p == "" {
		return ""
	}
	p = strings.TrimPrefix(p, "/")
	cleaned := path.Clean(p)
	if cleaned == "." || strings.HasPrefix(cleaned, "../") || cleaned == ".." {
		return ""
	}
	return cleaned
}

// validateMount checks that a MountAt prefix is a forward-slash path
// with no leading / trailing slash, no traversal ("." or ".."), and
// no empty segments. Empty input is valid (mount-at-root). Returns a
// human-readable error describing what's wrong.
func validateMount(m string) error {
	if m == "" {
		return nil
	}
	if strings.HasPrefix(m, "/") {
		return fmt.Errorf("%q has a leading slash", m)
	}
	if strings.HasSuffix(m, "/") {
		return fmt.Errorf("%q has a trailing slash", m)
	}
	for _, seg := range strings.Split(m, "/") {
		switch seg {
		case "":
			return fmt.Errorf("%q has an empty segment", m)
		case ".", "..":
			return fmt.Errorf("%q has a %q segment", m, seg)
		}
	}
	if path.Clean(m) != m {
		return fmt.Errorf("%q is not normalised (path.Clean would change it)", m)
	}
	return nil
}
