package assetmapper

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"
	"sync"
)

// ImportmapFilename is the conventional file name for the project's
// importmap (typically committed at the repo root).
const ImportmapFilename = "importmap.json"

// Importmap is the project's mapping from bare specifiers (like
// "react" or "app") to assets. The browser receives this map inside
// a <script type="importmap"> tag and uses it to resolve module
// imports.
//
// Entries are one of two shapes:
//
//   - Local: Path set, Version empty. Resolved through [Mapper.Asset]
//     so it tracks dev/prod hashing automatically.
//   - Vendored: Version set, Path empty. Resolved against the
//     convention path vendor/<key>.js (or .css). Vendored files are
//     downloaded by [Vendor.Require]; they live under the mapper's
//     asset roots like any other file.
//
// Importmap is loaded from [ImportmapFilename] in the project root,
// edited in place by [Vendor] (or the CLI), and rendered into HTML
// at request time via [Importmap.Render].
type Importmap struct {
	Entries map[string]ImportmapEntry

	// preloadCache memoises the preload dep graph per (mapper,
	// entrypoints) combination. Populated in prod mode (Mapper with
	// a Manifest) where source files don't change at runtime; in
	// dev mode the graph is re-walked every call so file edits show
	// up immediately.
	//
	// Cache invalidation: never automatic. Mutating Entries after
	// the first Render in prod can return stale preload URLs;
	// construct a fresh Importmap if Entries change at runtime.
	//
	// Bounds: one entry per distinct (Mapper, entrypoints) tuple.
	// Production typically sees a handful — long-lived Mappers
	// and a small set of template-defined entrypoint names. A
	// service that constructs a fresh Mapper per request (atypical)
	// would leak one entry per request; rebuild the Importmap
	// periodically or wrap with a custom LRU.
	//
	// Reload from disk ([LoadImportmap] / [ParseImportmap]) produces
	// a fresh Importmap with an empty cache — sync.Map is not
	// serialised in the on-disk JSON.
	preloadCache sync.Map // map[preloadCacheKey]preloadResult
}

// preloadCacheKey is the comparable identity for one cached preload
// graph result. Mapper pointer ensures separate Mappers don't share
// a cache entry; the joined entrypoints string covers ordering and
// composition.
type preloadCacheKey struct {
	mapper      *Mapper
	entrypoints string
}

// ImportmapEntry is one bare-specifier mapping.
type ImportmapEntry struct {
	// Path is the logical asset path for local entries. Mutually
	// exclusive with Version.
	Path string `json:"path,omitempty"`
	// Version is the package version for vendored entries. Mutually
	// exclusive with Path.
	Version string `json:"version,omitempty"`
	// Type is "js" (default) or "css". Affects how Render emits the
	// entrypoint tag:
	//
	//   - js  → <script type="module">import "name";</script>
	//   - css → <link rel="stylesheet" href="...">
	//
	// Type also controls the conventional file extension when
	// resolving Vendored entries (vendor/<key>.css vs vendor/<key>.js).
	Type string `json:"type,omitempty"`
	// Entrypoint, when true, makes the entry eligible to be passed
	// by name to [Importmap.Render]. Non-entrypoint entries appear
	// in the importmap (so JS imports can reach them) but cannot be
	// requested as page entrypoints.
	Entrypoint bool `json:"entrypoint,omitempty"`
}

// NewImportmap returns an empty importmap.
func NewImportmap() *Importmap {
	return &Importmap{Entries: map[string]ImportmapEntry{}}
}

// LoadImportmap reads an importmap from path. Use [os.IsNotExist]
// on the wrapped error to distinguish "file missing" from "file
// malformed".
func LoadImportmap(path string) (*Importmap, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("assetmapper.LoadImportmap: open %s: %w", path, err)
	}
	defer f.Close()
	return ParseImportmap(f)
}

// ParseImportmap decodes an importmap from r. Unknown JSON fields are
// rejected so typos in importmap.json surface as errors rather than
// silently dropped data.
func ParseImportmap(r io.Reader) (*Importmap, error) {
	var entries map[string]ImportmapEntry
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&entries); err != nil {
		return nil, fmt.Errorf("assetmapper.ParseImportmap: %w", err)
	}
	if entries == nil {
		entries = map[string]ImportmapEntry{}
	}
	return &Importmap{Entries: entries}, nil
}

// Save writes the importmap to path with sorted keys and two-space
// indentation. The directory must already exist.
func (im *Importmap) Save(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("assetmapper.Importmap.Save: create %s: %w", path, err)
	}
	if err := im.Write(f); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Write encodes the importmap as indented JSON. Entries' keys are
// sorted by [json.MarshalIndent] (Go 1.12+ deterministic behaviour)
// so consecutive writes produce stable diffs.
func (im *Importmap) Write(w io.Writer) error {
	data, err := json.MarshalIndent(im.Entries, "", "  ")
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	_, err = w.Write([]byte("\n"))
	return err
}

// RenderOptions controls [Importmap.RenderWithOptions] and
// [Importmap.ModulePreloadLinksWithOptions]. The zero value is the
// same shape as the variadic forms with no entrypoints and no nonce.
type RenderOptions struct {
	// Entrypoints names the importmap entries that should produce
	// page entrypoint tags (and seed the modulepreload graph walk
	// for [Importmap.RenderWithOptions]). Empty means importmap-only
	// output.
	Entrypoints []string

	// Nonce, when non-empty, is set as the nonce="..." attribute on
	// every <script> and <link> tag emitted. Required when
	// Content-Security-Policy declares 'nonce-XYZ' for script-src
	// and/or style-src. The same value is applied to all tags;
	// users with split script-src / style-src nonces should call
	// the underlying methods separately and compose the output
	// themselves.
	Nonce string
}

// Render is the simple-positional form: equivalent to
// [Importmap.RenderWithOptions] with [RenderOptions.Entrypoints] set
// from entrypoints and no nonce. Use RenderWithOptions when CSP
// nonces are needed.
func (im *Importmap) Render(m *Mapper, entrypoints ...string) (string, error) {
	return im.RenderWithOptions(m, RenderOptions{Entrypoints: entrypoints})
}

// RenderWithOptions returns the HTML to inject into the page <head>.
// The output is ordered:
//
//  1. <script type="importmap">{...}</script> — every entry resolved
//     to its public URL via mapper.
//  2. <link rel="modulepreload"> tags — one per JS module
//     transitively reachable from any JS entrypoint, so the browser
//     can begin fetching deps in parallel with the importmap parse.
//  3. <link rel="preload" as="style"> tags — one per CSS file
//     reached via `import "./x.css"` from JS. CSS entrypoints get
//     the full stylesheet link in step 4 instead.
//  4. <link rel="stylesheet"> tags — one per CSS entrypoint.
//  5. <script type="module">import "name";</script> tags — one per
//     JS entrypoint. The browser uses the importmap (parsed in step
//     1) to resolve the bare specifier to a URL (already cached
//     thanks to step 2).
//
// When [RenderOptions.Nonce] is non-empty, every emitted <script>
// and <link> tag carries nonce="...".
//
// Returns an error if any entrypoint name is missing from the map or
// is not marked as Entrypoint=true (a guardrail against typos that
// would otherwise silently load nothing).
//
// The output is plain text (string); callers using html/template
// should wrap it in [template.HTML] to bypass attribute escaping.
// See the assetmapper/template satellite for ready-made helpers.
func (im *Importmap) RenderWithOptions(m *Mapper, opts RenderOptions) (string, error) {
	if m == nil {
		return "", fmt.Errorf("assetmapper.Importmap.Render: nil Mapper")
	}
	if err := im.validateEntrypoints(opts.Entrypoints); err != nil {
		return "", fmt.Errorf("assetmapper.Importmap.Render: %w", err)
	}

	keys := make([]string, 0, len(im.Entries))
	for k := range im.Entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	resolved := make(map[string]string, len(keys))
	for _, k := range keys {
		url, err := im.resolveEntry(m, k, im.Entries[k])
		if err != nil {
			return "", fmt.Errorf("assetmapper.Importmap.Render: resolve %q: %w", k, err)
		}
		resolved[k] = url
	}

	nonce := nonceAttr(opts.Nonce)

	var out strings.Builder
	// json.Marshal escapes < > & by default, so embedding inside a
	// <script> tag is safe even for URLs that include those chars.
	out.WriteString(`<script type="importmap"`)
	out.WriteString(nonce)
	out.WriteString(`>{"imports":{`)
	for i, k := range keys {
		if i > 0 {
			out.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		vb, _ := json.Marshal(resolved[k])
		out.Write(kb)
		out.WriteByte(':')
		out.Write(vb)
	}
	out.WriteString("}}</script>")

	preloads, err := im.preloadGraph(m, opts.Entrypoints)
	if err != nil {
		return "", fmt.Errorf("assetmapper.Importmap.Render: build preload graph: %w", err)
	}
	for _, url := range preloads.JSURLs {
		out.WriteString("\n")
		out.WriteString(`<link rel="modulepreload" href="`)
		out.WriteString(html.EscapeString(url))
		out.WriteString(`"`)
		out.WriteString(nonce)
		out.WriteString(">")
	}
	// CSS reached from JS imports: preload-hint as stylesheet so
	// the browser can start the fetch in parallel with the JS
	// parse that will eventually attach them. CSS entrypoints get
	// the full <link rel="stylesheet"> tag below instead.
	for _, url := range preloads.CSSURLs {
		out.WriteString("\n")
		out.WriteString(`<link rel="preload" as="style" href="`)
		out.WriteString(html.EscapeString(url))
		out.WriteString(`"`)
		out.WriteString(nonce)
		out.WriteString(">")
	}

	for _, name := range opts.Entrypoints {
		out.WriteString("\n")
		entry := im.Entries[name]
		switch entry.Type {
		case "css":
			out.WriteString(`<link rel="stylesheet" href="`)
			out.WriteString(html.EscapeString(resolved[name]))
			out.WriteString(`"`)
			out.WriteString(nonce)
			out.WriteString(">")
		default:
			out.WriteString(`<script type="module"`)
			out.WriteString(nonce)
			out.WriteString(">import ")
			nb, _ := json.Marshal(name)
			out.Write(nb)
			out.WriteString(";</script>")
		}
	}
	return out.String(), nil
}

// ModulePreloadLinks is the simple-positional form: equivalent to
// [Importmap.ModulePreloadLinksWithOptions] with
// [RenderOptions.Entrypoints] set from entrypoints and no nonce.
func (im *Importmap) ModulePreloadLinks(m *Mapper, entrypoints ...string) (string, error) {
	return im.ModulePreloadLinksWithOptions(m, RenderOptions{Entrypoints: entrypoints})
}

// ModulePreloadLinksWithOptions returns just the
// <link rel="modulepreload"> tags — the JS-only subset of what
// [Importmap.RenderWithOptions] emits in its step 2. Order is
// deterministic: entrypoints first (in the order given), then
// transitive JS deps in depth-first order, deduplicated.
//
// CSS files (entrypoints or imported-from-JS) are NOT included by
// this method by design — the name promises modulepreload, which
// only applies to JS. CSS-from-JS preloads (<link rel="preload"
// as="style">) are emitted by [Importmap.RenderWithOptions]
// directly; callers composing manually can compute their own.
//
// When [RenderOptions.Nonce] is non-empty, every emitted tag carries
// nonce="...".
func (im *Importmap) ModulePreloadLinksWithOptions(m *Mapper, opts RenderOptions) (string, error) {
	if m == nil {
		return "", fmt.Errorf("assetmapper.Importmap.ModulePreloadLinks: nil Mapper")
	}
	if err := im.validateEntrypoints(opts.Entrypoints); err != nil {
		return "", fmt.Errorf("assetmapper.Importmap.ModulePreloadLinks: %w", err)
	}
	result, err := im.preloadGraph(m, opts.Entrypoints)
	if err != nil {
		return "", fmt.Errorf("assetmapper.Importmap.ModulePreloadLinks: %w", err)
	}
	nonce := nonceAttr(opts.Nonce)
	var out strings.Builder
	for i, url := range result.JSURLs {
		if i > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(`<link rel="modulepreload" href="`)
		out.WriteString(html.EscapeString(url))
		out.WriteString(`"`)
		out.WriteString(nonce)
		out.WriteString(">")
	}
	return out.String(), nil
}

// nonceAttr returns ` nonce="VALUE"` (leading space included) when
// nonce is non-empty, or "" otherwise. The value is HTML-attribute
// escaped so a nonce containing quotes can't break out of the
// attribute. (Per the CSP spec nonces are base64-ish and shouldn't
// contain HTML-special chars, but defence in depth is cheap.)
func nonceAttr(nonce string) string {
	if nonce == "" {
		return ""
	}
	return ` nonce="` + html.EscapeString(nonce) + `"`
}

// validateEntrypoints checks every name is in the importmap and is
// marked as an entrypoint. Centralised so Render and
// ModulePreloadLinks report identical errors for the same input.
func (im *Importmap) validateEntrypoints(entrypoints []string) error {
	for _, name := range entrypoints {
		entry, ok := im.Entries[name]
		if !ok {
			return fmt.Errorf("entrypoint %q not in importmap", name)
		}
		if !entry.Entrypoint {
			return fmt.Errorf("entry %q is not marked as entrypoint (set \"entrypoint\": true in importmap.json)", name)
		}
	}
	return nil
}

// preloadResult is the two-bucket output of a preload graph walk.
// JS modules go into JSURLs (emitted as <link rel="modulepreload">),
// CSS files reachable from JS imports go into CSSURLs (emitted as
// <link rel="preload" as="style">). CSS entrypoints are excluded
// from both — they're already handled by Render's <link rel="stylesheet">.
type preloadResult struct {
	JSURLs  []string
	CSSURLs []string
}

// preloadGraph walks the dependency graph from each JS entrypoint
// and returns the URLs to preload, partitioned by kind, in DFS order
// with duplicates removed. CSS entrypoints contribute nothing (their
// link rel="stylesheet" tag already triggers immediate fetch).
//
// In prod mode the result is memoised — source files don't change
// at runtime, so the same (mapper, entrypoints) tuple always
// produces the same URLs. Dev mode re-walks every call so file
// edits surface immediately.
//
// Files that cannot be read (because they're outside the configured
// roots, e.g. a deploy that shipped only the compiled artifact) are
// preloaded themselves but their transitive deps are not walked.
// This degrades gracefully: the entrypoint still preloads, only the
// parallelism on its sub-deps is lost.
func (im *Importmap) preloadGraph(m *Mapper, entrypoints []string) (preloadResult, error) {
	if m.manifest != nil {
		key := preloadCacheKey{mapper: m, entrypoints: strings.Join(entrypoints, "\x00")}
		if v, ok := im.preloadCache.Load(key); ok {
			return v.(preloadResult), nil
		}
		result, err := im.computePreloadGraph(m, entrypoints)
		if err != nil {
			return preloadResult{}, err
		}
		// Race-tolerant store: a concurrent miss might also compute
		// and store; the values are identical, so last-writer-wins
		// is harmless.
		im.preloadCache.Store(key, result)
		return result, nil
	}
	return im.computePreloadGraph(m, entrypoints)
}

// computePreloadGraph is the uncached worker. Reached via
// preloadGraph; broken out so the cache lookup stays small.
func (im *Importmap) computePreloadGraph(m *Mapper, entrypoints []string) (preloadResult, error) {
	var js, css []string
	seen := map[string]bool{}

	var visit func(logical string) error
	visit = func(logical string) error {
		if logical == "" || seen[logical] {
			return nil
		}
		seen[logical] = true

		kind := kindOf(logical)
		if kind != kindJS && kind != kindCSS {
			// Images / fonts / JSON: browser handles after the
			// referrer parses; no preload hint emitted.
			return nil
		}

		url, err := m.Asset(logical)
		if err != nil {
			return fmt.Errorf("resolve URL for %q: %w", logical, err)
		}

		if kind == kindCSS {
			// CSS reached from a JS import (modern `import "./x.css"`
			// pattern). Emit <link rel="preload" as="style">. Don't
			// recurse into the CSS's own @import / url() refs —
			// preload-hinting transitive CSS rarely pays off and
			// would tax the head with many extra tags.
			css = append(css, url)
			return nil
		}

		js = append(js, url)

		// Best-effort source read for transitive deps.
		root, sub, err := m.resolveFile(logical)
		if err != nil {
			return nil
		}
		content, err := fs.ReadFile(root.FS, sub)
		if err != nil {
			return nil
		}

		for _, r := range extractRefs(logical, content, kindJS) {
			if r.resolved != "" {
				if err := visit(r.resolved); err != nil {
					return err
				}
				continue
			}
			// Bare specifier: try the importmap.
			entry, ok := im.Entries[r.spec]
			if !ok {
				continue
			}
			if err := visit(logicalForEntry(r.spec, entry)); err != nil {
				return err
			}
		}
		return nil
	}

	for _, name := range entrypoints {
		entry := im.Entries[name]
		if entry.Type == "css" {
			continue
		}
		if err := visit(logicalForEntry(name, entry)); err != nil {
			return preloadResult{}, err
		}
	}
	return preloadResult{JSURLs: js, CSSURLs: css}, nil
}

// logicalForEntry returns the logical asset path that backs an
// importmap entry — either the user-supplied Path (local entry) or
// the vendor/<spec>.<ext> convention path (vendored entry).
func logicalForEntry(spec string, entry ImportmapEntry) string {
	if entry.Path != "" {
		return entry.Path
	}
	if entry.Version == "" {
		return ""
	}
	ext := ".js"
	if entry.Type == "css" {
		ext = ".css"
	}
	return "vendor/" + spec + ext
}

// resolveEntry turns one importmap entry into a public URL. Local
// entries (Path set) round-trip through the mapper. Vendored entries
// (Version set) use the convention path vendor/<key>(.js|.css) — the
// CLI ensures the file actually exists at that path.
func (im *Importmap) resolveEntry(m *Mapper, key string, entry ImportmapEntry) (string, error) {
	if entry.Path == "" && entry.Version == "" {
		return "", fmt.Errorf("entry has neither \"path\" (local) nor \"version\" (vendored)")
	}
	if entry.Path != "" && entry.Version != "" {
		return "", fmt.Errorf("entry has both \"path\" and \"version\"; pick one")
	}
	if entry.Path != "" {
		return m.Asset(entry.Path)
	}
	ext := ".js"
	if entry.Type == "css" {
		ext = ".css"
	}
	return m.Asset("vendor/" + key + ext)
}
