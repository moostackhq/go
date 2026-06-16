// Package template loads a tree of HTML page templates with a
// Hugo-inspired layout-by-section model and renders them through
// an http.ResponseWriter.
//
// Layout: pages live under sections (subdirectories of the root).
// Each section may declare its own [_layout.html] and section-local
// partials ([_*.html] other than the layout); the conventional
// section [_default] is the fallback for everything not overridden.
// A page in section S uses S's layout and partials if present,
// otherwise the _default ones. There is no separate "shared
// partials" directory — _default IS the shared set.
//
// Page keys: pages in _default are addressed by bare basename
// ("home"); pages in any other section are addressed by section
// path ("public/status"). Render takes the key.
//
// Example tree:
//
//	templates/
//	├── _default/                ← fallback layout + partials + pages
//	│   ├── _layout.html
//	│   ├── _flash.html          ← partial shared across sections
//	│   ├── home.html            ← key: "home"
//	│   └── monitors.html        ← key: "monitors"
//	└── public/                  ← section override
//	    ├── _layout.html         ← used by pages here, instead of _default's
//	    └── status.html          ← key: "public/status"
package template

import (
	"bytes"
	"fmt"
	htmltpl "html/template"
	"io/fs"
	"net/http"
	"path"
	"sort"
	"strings"
)

// FuncMap is an alias for [html/template.FuncMap] so callers can
// declare template helpers without importing the stdlib package
// alongside this one.
type FuncMap = htmltpl.FuncMap

// DefaultSection is the conventional section name whose layout
// and partials act as the fallback for every other section.
const DefaultSection = "_default"

// LayoutFile is the conventional filename whose presence marks a
// section's base layout.
const LayoutFile = "_layout.html"

// Set is a parsed collection of pages, each bound to its section's
// layout and partials. Safe for concurrent use; construct once at
// boot via [Load].
type Set struct {
	pages map[string]*htmltpl.Template // page key → template (root = LayoutFile)
}

// Load walks dir under fsys and parses every page it finds.
//
// Discovery:
//
//   - dir/<section>/ is a section. The conventional section
//     [DefaultSection] is the fallback used when another section
//     omits its own layout or partials.
//   - dir/<section>/[LayoutFile] is the section's base layout.
//     A section without a [LayoutFile] uses DefaultSection's.
//   - dir/<section>/_*.html (other than LayoutFile) are partials,
//     parsed into every page in that section. Section partials
//     shadow DefaultSection's on filename collision.
//   - Every other dir/<section>/*.html is a page, keyed by basename
//     when section == DefaultSection, or section/basename otherwise.
//
// Each entry in funcMaps is merged into the running FuncMap that
// [DefaultFuncs] seeds. Order is priority: [DefaultFuncs] first
// (lowest), then funcMaps[0], funcMaps[1], … so a later map
// overrides earlier ones (and any caller can override a default).
// Nil entries are tolerated and contribute nothing.
//
// Errors: a section that has pages but no resolvable layout (no
// [LayoutFile] in itself or in DefaultSection) is a hard error;
// so is any parse error in any file.
func Load(fsys fs.FS, dir string, funcMaps ...FuncMap) (*Set, error) {
	merged := DefaultFuncs()
	for _, fm := range funcMaps {
		for k, v := range fm {
			merged[k] = v
		}
	}

	sections, err := readSections(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("template.Load: %w", err)
	}
	defSection, hasDefault := sections[DefaultSection]
	out := &Set{pages: map[string]*htmltpl.Template{}}

	for name, sec := range sections {
		layoutPath := sec.layoutPath
		partials := append([]string{}, sec.partialPaths...)
		if name != DefaultSection && hasDefault {
			// Layout fallback: section without its own layout inherits
			// DefaultSection's.
			if layoutPath == "" {
				layoutPath = defSection.layoutPath
			}
			// Partial fallback: every _default partial whose basename
			// the section did not override is appended. Section-local
			// partials shadow on filename collision.
			localNames := map[string]bool{}
			for _, p := range partials {
				localNames[path.Base(p)] = true
			}
			for _, p := range defSection.partialPaths {
				if !localNames[path.Base(p)] {
					partials = append(partials, p)
				}
			}
		}

		if layoutPath == "" {
			if len(sec.pagePaths) == 0 {
				continue
			}
			return nil, fmt.Errorf("template.Load: section %q has %d page(s) but no %s (and no %s/%s fallback)",
				name, len(sec.pagePaths), LayoutFile, DefaultSection, LayoutFile)
		}

		for _, pagePath := range sec.pagePaths {
			files := append([]string{layoutPath}, partials...)
			files = append(files, pagePath)
			t, err := htmltpl.New(LayoutFile).Funcs(merged).ParseFS(fsys, files...)
			if err != nil {
				return nil, fmt.Errorf("template.Load: parse %s: %w", pagePath, err)
			}
			key := pageKey(name, pagePath)
			if _, exists := out.pages[key]; exists {
				return nil, fmt.Errorf("template.Load: duplicate page key %q (last from %s)", key, pagePath)
			}
			out.pages[key] = t
		}
	}

	if len(out.pages) == 0 {
		return nil, fmt.Errorf("template.Load: no pages found under %s", dir)
	}
	return out, nil
}

// Render executes the named page through its layout and returns any
// error. Page name is the [Set]'s key — bare basename for
// DefaultSection, "<section>/<basename>" otherwise.
//
// The page is rendered into a buffer first, so a missing page or a
// template-execution error is returned with nothing written to w —
// the caller can still send a proper status (e.g. 500). On success
// the Content-Type header is set and the buffered HTML is flushed.
// Whether a render error becomes a 500, a logged event, or a visible
// dev error page is the caller's policy.
func (s *Set) Render(w http.ResponseWriter, name string, data any) error {
	t, ok := s.pages[name]
	if !ok {
		return fmt.Errorf("template.Render: unknown page %q", name)
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, LayoutFile, data); err != nil {
		return fmt.Errorf("template.Render %q: %w", name, err)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, err := buf.WriteTo(w)
	return err
}

// Names returns the registered page keys, sorted.
func (s *Set) Names() []string {
	out := make([]string, 0, len(s.pages))
	for k := range s.pages {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// section captures one (direct-child) directory under the root.
type section struct {
	layoutPath   string   // dir/<section>/_layout.html when present, else ""
	partialPaths []string // dir/<section>/_*.html (excluding the layout)
	pagePaths    []string // dir/<section>/*.html (non-underscore-prefixed)
}

// readSections walks dir's direct children. Subdirectories become
// sections. Non-directory entries directly under dir are ignored —
// pages must live in a section.
func readSections(fsys fs.FS, dir string) (map[string]*section, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	out := map[string]*section{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		secName := e.Name()
		sub := path.Join(dir, secName)
		files, err := fs.ReadDir(fsys, sub)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", sub, err)
		}
		sec := &section{}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			name := f.Name()
			if !strings.HasSuffix(name, ".html") {
				continue
			}
			full := path.Join(sub, name)
			switch {
			case name == LayoutFile:
				sec.layoutPath = full
			case strings.HasPrefix(name, "_"):
				sec.partialPaths = append(sec.partialPaths, full)
			default:
				sec.pagePaths = append(sec.pagePaths, full)
			}
		}
		sort.Strings(sec.partialPaths)
		sort.Strings(sec.pagePaths)
		out[secName] = sec
	}
	return out, nil
}

// pageKey computes the public name a page is rendered under: bare
// basename for the default section, "<section>/<basename>" for
// every other section.
func pageKey(sectionName, pagePath string) string {
	base := strings.TrimSuffix(path.Base(pagePath), ".html")
	if sectionName == DefaultSection {
		return base
	}
	return sectionName + "/" + base
}
