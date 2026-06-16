package assetmapper

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
)

// Build is the convention-over-configuration constructor for the
// standard dev/prod wiring. It returns the four things every web
// app needs from this library:
//
//   - *Mapper that resolves logical asset paths to public URLs.
//   - *Importmap parsed from sourceDir/importmap.json (empty when
//     the file is absent — the "no vendored deps yet" case).
//   - http.Handler ready to mount under [Mapper.URLPrefix].
//   - error if any step fails.
//
// Mode is detected from the filesystem:
//
//   - publicDir/[ManifestFilename] present → prod. Mapper roots are
//     publicDir + the loaded manifest; the handler is an
//     http.FileServer over publicDir with the URL prefix stripped.
//     No caching policy is set — front with a CDN, or wrap the
//     returned handler with cache-control middleware if you serve
//     in-process.
//   - publicDir/[ManifestFilename] absent → dev. Mapper roots are
//     sourceDir; the handler is the library's [Mapper.Handler]
//     (lazy hash, ETag, no-cache).
//
// Build covers the standard shape. Apps that need different wiring
// (alternate handler, embed.FS roots, signed URLs, multi-root
// mounts) construct the Mapper directly via [New] + [LoadManifest]
// + [LoadImportmap] + [Mapper.Handler].
func Build(sourceDir, publicDir string) (*Mapper, *Importmap, http.Handler, error) {
	im, err := loadImportmapOrEmpty(filepath.Join(sourceDir, "importmap.json"))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("assetmapper.Build: importmap: %w", err)
	}

	if _, err := os.Stat(filepath.Join(publicDir, ManifestFilename)); err == nil {
		mf, err := LoadManifest(publicDir)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("assetmapper.Build: manifest: %w", err)
		}
		m, err := New(Config{
			Roots:    []Root{{FS: os.DirFS(publicDir)}},
			Manifest: mf,
		})
		if err != nil {
			return nil, nil, nil, fmt.Errorf("assetmapper.Build: prod mapper: %w", err)
		}
		h := http.StripPrefix(m.URLPrefix(), http.FileServer(http.Dir(publicDir)))
		return m, im, h, nil
	}

	m, err := New(Config{
		Roots: []Root{{FS: os.DirFS(sourceDir)}},
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("assetmapper.Build: dev mapper: %w", err)
	}
	return m, im, m.Handler(), nil
}

func loadImportmapOrEmpty(path string) (*Importmap, error) {
	im, err := LoadImportmap(path)
	if err == nil {
		return im, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return NewImportmap(), nil
	}
	return nil, err
}
