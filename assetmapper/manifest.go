package assetmapper

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Manifest is the logical → public-filename map produced by Compile
// and read by [Mapper] in prod mode. Persisted as JSON inside the
// public asset directory (sibling to the hashed files) so the same
// artifact a CDN serves is also the source of truth at runtime.
//
// JSON shape (Entries' keys sorted for diff stability):
//
//	{
//	  "url_prefix": "/assets/",
//	  "entries": {
//	    "app.js": "app-7a1b2c3d.js",
//	    "images/logo.png": "images/logo-deadbeef.png"
//	  }
//	}
//
// URLPrefix captures the value [Compile] was invoked with so [New]
// can refuse a runtime [Config.URLPrefix] that disagrees — otherwise
// the references baked into compiled JS / CSS (which use the
// compile-time prefix) would point at a different URL than the one
// the runtime hands out via [Mapper.Asset], silently half-loading
// the site.
type Manifest struct {
	URLPrefix string            `json:"url_prefix,omitempty"`
	Entries   map[string]string `json:"entries"`
}

// ManifestFilename is the conventional file name used by [Manifest.Save]
// and [LoadManifest] inside the public asset directory.
const ManifestFilename = "manifest.json"

// NewManifest returns an empty manifest.
func NewManifest() *Manifest {
	return &Manifest{Entries: map[string]string{}}
}

// LoadManifest reads a manifest from publicDir/manifest.json. Returns
// an error wrapping the underlying I/O or JSON failure; callers that
// need to distinguish "file missing" from "file malformed" can use
// [os.IsNotExist] on the wrapped error.
func LoadManifest(publicDir string) (*Manifest, error) {
	path := filepath.Join(publicDir, ManifestFilename)
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("assetmapper.LoadManifest: open %s: %w", path, err)
	}
	defer f.Close()
	return ParseManifest(f)
}

// ParseManifest decodes a manifest from r. Exposed for tests and for
// callers that store the manifest somewhere other than the filesystem
// (e.g. an embed.FS shipped alongside the binary).
func ParseManifest(r io.Reader) (*Manifest, error) {
	var m Manifest
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		return nil, fmt.Errorf("assetmapper.ParseManifest: %w", err)
	}
	if m.Entries == nil {
		m.Entries = map[string]string{}
	}
	return &m, nil
}

// Save writes the manifest to publicDir/manifest.json with sorted
// keys (so consecutive compiles produce stable diffs). The directory
// must already exist; Save does not create it.
func (m *Manifest) Save(publicDir string) error {
	path := filepath.Join(publicDir, ManifestFilename)
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("assetmapper.Manifest.Save: create %s: %w", path, err)
	}
	if err := m.Write(f); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Write encodes the manifest to w as JSON with two-space indentation.
// Entries' keys are sorted by [json.MarshalIndent] (Go 1.12+
// deterministic behaviour) so consecutive writes produce stable diffs.
func (m *Manifest) Write(w io.Writer) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	_, err = w.Write([]byte("\n"))
	return err
}

// Lookup returns the public file name for a logical path, or
// ("", false) if absent.
func (m *Manifest) Lookup(logicalPath string) (string, bool) {
	v, ok := m.Entries[logicalPath]
	return v, ok
}
