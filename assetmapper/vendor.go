package assetmapper

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// vendorDownloadConcurrency caps in-flight Fetch calls during
// [Vendor.applyResolution]. Eight is a comfortable parallelism for
// a CDN like jspm.io — enough to amortise network latency without
// looking like a thundering herd.
const vendorDownloadConcurrency = 8

// VendorDir is the conventional subdirectory under the asset root
// where vendored package files live. Matches the convention path the
// [Importmap] uses to resolve vendored entries (vendor/<key>.js or
// vendor/<key>.css), so vendored files are picked up by [Compile] and
// served by [Mapper.Handler] like any other asset.
const VendorDir = "vendor"

// PackageRequest is one (name, version) pair as supplied by the user
// to [Vendor.Require]. Empty Version requests the resolver's notion
// of "latest stable".
type PackageRequest struct {
	Name    string
	Version string
}

// ResolvedPackage is one bare-specifier mapping after a
// [PackageResolver] has expanded transitive dependencies and chosen
// concrete versions. Specifier is what JS code writes in import
// statements (e.g. "react"); URL is the upstream fetch location;
// the local on-disk path is derived as VendorDir/<Specifier>.<ext>.
type ResolvedPackage struct {
	Specifier string
	Version   string
	Type      string // "js" (default) or "css"
	URL       string
}

// Resolution is the resolver's expansion of a set of PackageRequests
// to the full transitive closure. Order is not significant; callers
// must download every package returned, not just the originally
// requested ones, because the requested package's source typically
// imports the others by bare specifier and would not resolve at
// runtime without them in the importmap.
type Resolution struct {
	Packages []ResolvedPackage
}

// PackageResolver fetches and resolves vendored package files.
// Implementations talk to an upstream registry / CDN (jspm.io for the
// default [JSPMResolver]). The interface is decoupled from the HTTP
// transport so tests can stub it.
//
// Resolve takes a list of requests and returns every package needed
// to satisfy them — including transitive deps — each with an
// upstream URL.
//
// Fetch downloads the bytes at url. The same Resolution may produce
// many Fetch calls (one per package); implementations should honour
// ctx for cancellation and timeouts.
type PackageResolver interface {
	Resolve(ctx context.Context, reqs []PackageRequest) (*Resolution, error)
	Fetch(ctx context.Context, url string) ([]byte, error)
}

// Vendor manages the vendored portion of an [Importmap]. Use
// [Vendor.Require] to add a package (resolving and downloading
// transitive deps), [Vendor.Remove] to take one out, [Vendor.Prune]
// to garbage-collect orphaned files.
//
// Vendor methods are NOT safe for concurrent use: Require, Remove,
// and Prune all mutate the on-disk vendor directory and the
// in-memory Importmap without internal locking. The CLI uses Vendor
// from a single-shot command so this is a non-issue; library users
// wiring Vendor into a long-lived service should serialise calls
// externally.
//
// After mutating, callers persist the importmap to disk with
// [Importmap.Save] — Vendor never writes importmap.json itself so
// the file write can be atomic at the caller's choice (e.g. inside a
// CLI command that handles save + version control as one step).
type Vendor struct {
	// Resolver supplies the upstream resolution + download. Required.
	Resolver PackageResolver
	// VendorDir is the on-disk directory where vendored files live.
	// For a project whose asset root is <project>/assets, this is
	// typically <project>/assets/vendor.
	VendorDir string
	// Importmap holds the in-memory importmap. Mutated by Require /
	// Remove. Required.
	Importmap *Importmap
}

// Require resolves pkg@version (and transitive deps) via the
// configured PackageResolver, downloads each file to VendorDir,
// rewrites internal upstream URLs inside JS files to bare specifiers
// (so the downloaded code is browser-runnable against the importmap),
// and registers each package in [Vendor.Importmap]. An empty version
// asks the resolver for "latest".
//
// Existing entries are overwritten, so Require doubles as an update
// operation.
func (v *Vendor) Require(ctx context.Context, pkg, version string) error {
	if err := v.validate(); err != nil {
		return err
	}
	if pkg == "" {
		return fmt.Errorf("assetmapper.Vendor.Require: empty package name")
	}
	res, err := v.Resolver.Resolve(ctx, []PackageRequest{{Name: pkg, Version: version}})
	if err != nil {
		return fmt.Errorf("assetmapper.Vendor.Require: resolve %s: %w", pkg, err)
	}
	return v.applyResolution(ctx, res)
}

// Remove deletes a vendored package and its importmap entry. Does
// NOT cascade to transitive deps (which may be shared); call
// [Vendor.Prune] afterwards to garbage-collect any vendored files
// no longer referenced from any importmap entry.
func (v *Vendor) Remove(specifier string) error {
	if err := v.validate(); err != nil {
		return err
	}
	entry, ok := v.Importmap.Entries[specifier]
	if !ok {
		return fmt.Errorf("assetmapper.Vendor.Remove: %q not in importmap", specifier)
	}
	if entry.Version == "" {
		return fmt.Errorf("assetmapper.Vendor.Remove: %q is a local entry (no version) — edit importmap.json directly", specifier)
	}
	ext := ".js"
	if entry.Type == "css" {
		ext = ".css"
	}
	dst := filepath.Join(v.VendorDir, filepath.FromSlash(specifier+ext))
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("assetmapper.Vendor.Remove: %s: %w", dst, err)
	}
	delete(v.Importmap.Entries, specifier)
	return nil
}

// Prune deletes files under [Vendor.VendorDir] that are not
// referenced by any vendored entry in [Vendor.Importmap]. Useful
// after [Vendor.Remove] (which intentionally does not cascade to
// transitive deps) to garbage-collect orphaned files; also clears
// out leftovers when an entry is hand-edited out of importmap.json.
//
// Returns the file paths (relative to VendorDir, forward-slash
// separated) of every file removed, sorted alphabetically. A missing
// VendorDir is treated as already-pruned and returns an empty
// slice with no error. Empty directories left behind after deletion
// are cleaned up best-effort.
//
// Prune NEVER touches importmap.json; it only deletes files. Run it
// after persisting any importmap mutations.
func (v *Vendor) Prune() ([]string, error) {
	if err := v.validate(); err != nil {
		return nil, err
	}

	expected := make(map[string]struct{}, len(v.Importmap.Entries))
	for spec, entry := range v.Importmap.Entries {
		if entry.Version == "" {
			continue // local entries don't live under VendorDir
		}
		ext := ".js"
		if entry.Type == "css" {
			ext = ".css"
		}
		expected[filepath.FromSlash(spec+ext)] = struct{}{}
	}

	if _, err := os.Stat(v.VendorDir); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("assetmapper.Vendor.Prune: stat %s: %w", v.VendorDir, err)
	}

	var removed []string
	err := filepath.WalkDir(v.VendorDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(v.VendorDir, p)
		if err != nil {
			return err
		}
		if _, keep := expected[rel]; keep {
			return nil
		}
		if err := os.Remove(p); err != nil {
			return fmt.Errorf("remove %s: %w", p, err)
		}
		removed = append(removed, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return removed, fmt.Errorf("assetmapper.Vendor.Prune: %w", err)
	}

	// Tidy up empty directories left behind by file removal. Failure
	// to remove a directory (e.g. it's still non-empty, or perms)
	// is non-fatal — Prune's contract is about files, not dirs.
	pruneEmptyDirs(v.VendorDir)

	sort.Strings(removed)
	return removed, nil
}

// pruneEmptyDirs removes empty subdirectories of root (but never
// root itself), bottom-up. os.Remove on a non-empty directory fails
// silently — we just skip those.
func pruneEmptyDirs(root string) {
	var dirs []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && p != root {
			dirs = append(dirs, p)
		}
		return nil
	})
	// Deepest first so a directory that becomes empty after its
	// children are removed is itself a candidate this pass.
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, d := range dirs {
		_ = os.Remove(d)
	}
}

func (v *Vendor) validate() error {
	if v.Resolver == nil {
		return fmt.Errorf("assetmapper.Vendor: nil Resolver")
	}
	if v.Importmap == nil {
		return fmt.Errorf("assetmapper.Vendor: nil Importmap")
	}
	if v.VendorDir == "" {
		return fmt.Errorf("assetmapper.Vendor: empty VendorDir")
	}
	return nil
}

// applyResolution stages every download in memory, then commits
// files + importmap mutations as a batch. A failure in the download
// phase (the common failure mode — network, 5xx from jspm.io) leaves
// the vendor dir and importmap completely untouched, so the caller's
// next Importmap.Save persists exactly the state it had before
// Require was called.
//
// A failure during the commit-to-disk phase (rare — permissions,
// disk full) can leave some files written and others not, BUT the
// importmap is mutated last and as a single in-process step so it
// always reflects either the prior state (if any disk write failed)
// or the full new state (if every write succeeded). Re-running
// Require is idempotent and recovers from any partial disk state.
func (v *Vendor) applyResolution(ctx context.Context, res *Resolution) error {
	if len(res.Packages) == 0 {
		return nil
	}

	// Build a URL → specifier index used to rewrite the imports
	// inside each downloaded JS file. Without this rewrite the file
	// would still reference upstream URLs (jspm.io) at runtime,
	// defeating the point of vendoring.
	urlToSpec := make(map[string]string, len(res.Packages))
	for _, p := range res.Packages {
		urlToSpec[p.URL] = p.Specifier
	}

	// Stage 1 — network. Fetch everything in memory; any failure
	// aborts before disk or importmap is touched.
	staged, err := v.fetchAll(ctx, res.Packages, urlToSpec)
	if err != nil {
		return err
	}

	// Stage 2 — disk. Mkdir up front, then write each staged file.
	// Importmap mutation is deferred to Stage 3 so a write failure
	// here doesn't leave the importmap claiming files that don't
	// exist.
	if err := os.MkdirAll(v.VendorDir, 0o755); err != nil {
		return fmt.Errorf("assetmapper.Vendor: create %s: %w", v.VendorDir, err)
	}
	for _, s := range staged {
		if err := os.MkdirAll(filepath.Dir(s.dst), 0o755); err != nil {
			return fmt.Errorf("assetmapper.Vendor: mkdir for %s: %w", s.pkg.Specifier, err)
		}
		if err := os.WriteFile(s.dst, s.content, 0o644); err != nil {
			return fmt.Errorf("assetmapper.Vendor: write %s: %w", s.dst, err)
		}
	}

	// Stage 3 — commit importmap entries. In-process map mutation;
	// cannot fail.
	for _, s := range staged {
		v.Importmap.Entries[s.pkg.Specifier] = ImportmapEntry{
			Version: s.pkg.Version,
			Type:    s.pkg.Type,
		}
	}
	return nil
}

// stagedPackage is the in-memory result of a single Fetch: the
// rewritten content plus the on-disk path it will land at during the
// commit phase.
type stagedPackage struct {
	pkg     ResolvedPackage
	content []byte
	dst     string
}

// fetchAll downloads every package in parallel (bounded by
// [vendorDownloadConcurrency]), rewriting JS imports against
// urlToSpec. Returns the full staged slice on success, or the first
// error on any failure (with cancellation propagated to in-flight
// workers via a derived context). Distinct slice indices means
// goroutines never share an element, so no mutex is needed for the
// per-package result.
func (v *Vendor) fetchAll(ctx context.Context, pkgs []ResolvedPackage, urlToSpec map[string]string) ([]stagedPackage, error) {
	staged := make([]stagedPackage, len(pkgs))

	derived, cancel := context.WithCancel(ctx)
	defer cancel()

	sem := make(chan struct{}, vendorDownloadConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

dispatch:
	for i, p := range pkgs {
		select {
		case <-derived.Done():
			break dispatch
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(i int, p ResolvedPackage) {
			defer wg.Done()
			defer func() { <-sem }()

			content, err := v.Resolver.Fetch(derived, p.URL)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("assetmapper.Vendor: fetch %s: %w", p.URL, err)
					cancel()
				}
				mu.Unlock()
				return
			}
			ext := ".js"
			if p.Type == "css" {
				ext = ".css"
			}
			if ext == ".js" {
				content = rewriteVendoredJS(content, urlToSpec)
			}
			staged[i] = stagedPackage{
				pkg:     p,
				content: content,
				dst:     filepath.Join(v.VendorDir, filepath.FromSlash(p.Specifier+ext)),
			}
		}(i, p)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return staged, nil
}

// rewriteVendoredJS replaces upstream URL references inside content
// with the bare specifier they map to. Anything not in urlToSpec is
// left as-is: relative imports stay relative; external URLs we don't
// recognise (a CDN reference inside the vendored source that wasn't
// part of the resolution) survive intact and the browser will fail
// at runtime if they're unreachable — surfacing the issue loudly
// rather than silently.
func rewriteVendoredJS(content []byte, urlToSpec map[string]string) []byte {
	var refs []ref
	for _, m := range jsImportRE.FindAllSubmatchIndex(content, -1) {
		spec := string(content[m[2]:m[3]])
		resolved := ""
		if _, ok := urlToSpec[spec]; ok {
			resolved = spec
		}
		refs = append(refs, ref{
			spec:     spec,
			resolved: resolved,
			start:    m[2],
			end:      m[3],
		})
	}
	return rewriteRefs(content, refs, func(r ref) string {
		return urlToSpec[r.spec]
	})
}
