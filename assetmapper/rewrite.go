package assetmapper

import (
	"path"
	"regexp"
	"sort"
	"strings"
)

// assetKind discriminates the rewriting strategy. Only JS and CSS
// files are scanned for internal references; other types (images,
// fonts, JSON) are copied verbatim and hashed by their raw content.
type assetKind int

const (
	kindOther assetKind = iota
	kindJS
	kindCSS
)

func kindOf(logicalPath string) assetKind {
	switch path.Ext(logicalPath) {
	case ".js", ".mjs":
		return kindJS
	case ".css":
		return kindCSS
	}
	return kindOther
}

// ref is one reference inside an asset's content that may need
// rewriting to point at a content-hashed filename.
type ref struct {
	// spec is the captured specifier as it appears in source
	// ("./foo.js"), surrounded by its delimiter (quote or no quote).
	spec string
	// resolved is the logical path the spec resolves to, or "" if
	// it's an external URL, a bare specifier (importmap-resolved),
	// or escapes the asset root.
	resolved string
	// start, end bracket the spec text inside the asset's content
	// (without the surrounding quotes).
	start, end int
}

// jsImportRE matches static AND dynamic import/export specifiers in
// JS. Permissive about what's between the keyword and the quote
// (whitespace, identifiers, braces, parens, "from", commas, "(") so
// it catches multi-line imports and dynamic import() in one shot;
// the only forbidden chars are quotes and semicolons, which would
// indicate we left the import header.
//
// False positives inside string literals and block comments are
// possible. They are filtered downstream: only specs that resolve to
// a known asset are rewritten; anything that doesn't match a real
// file is left alone.
var jsImportRE = regexp.MustCompile(
	`\b(?:import|export)\b[^'";]*?["']([^"'\n]+)["']`,
)

// cssURLRE matches url(...) with three alternations for double-,
// single-, and unquoted forms. The capture groups (groups 1/2/3) are
// mutually exclusive; the caller picks whichever is set.
var cssURLRE = regexp.MustCompile(
	`url\(\s*(?:"([^"]+)"|'([^']+)'|([^)\s'"]+))\s*\)`,
)

// cssImportRE matches @import "..." or @import '...' (the bare-string
// form). @import url("...") is covered by [cssURLRE] already.
var cssImportRE = regexp.MustCompile(
	`@import\s+["']([^"'\n]+)["']`,
)

// extractRefs scans content for references and returns each one
// alongside its byte range and resolved logical path.
func extractRefs(importerPath string, content []byte, kind assetKind) []ref {
	var refs []ref
	switch kind {
	case kindJS:
		for _, m := range jsImportRE.FindAllSubmatchIndex(content, -1) {
			refs = append(refs, refAt(importerPath, content, m[2], m[3]))
		}
	case kindCSS:
		for _, m := range cssURLRE.FindAllSubmatchIndex(content, -1) {
			s, e := pickAlternation(m, 2, 4, 6)
			if s < 0 {
				continue
			}
			refs = append(refs, refAt(importerPath, content, s, e))
		}
		for _, m := range cssImportRE.FindAllSubmatchIndex(content, -1) {
			refs = append(refs, refAt(importerPath, content, m[2], m[3]))
		}
	}
	return refs
}

func refAt(importer string, content []byte, start, end int) ref {
	spec := string(content[start:end])
	return ref{
		spec:     spec,
		resolved: resolveRef(importer, spec),
		start:    start,
		end:      end,
	}
}

// pickAlternation returns the first present (start, end) pair from
// the regex submatch indices. Used for cssURLRE's three alternations.
func pickAlternation(indices []int, groupStarts ...int) (int, int) {
	for _, gs := range groupStarts {
		if gs < len(indices) && indices[gs] >= 0 {
			return indices[gs], indices[gs+1]
		}
	}
	return -1, -1
}

// resolveRef converts a reference specifier (relative or absolute)
// into a logical asset path. Returns "" for:
//
//   - external URLs (http://, https://, //, data:),
//   - SVG fragments (#id),
//   - bare specifiers (importmap-resolved, not in our asset namespace),
//   - paths that escape the asset root via too many ../ segments.
//
// Relative paths are resolved against the importer's directory;
// absolute paths (leading /) are treated as rooted at the asset
// namespace. Query strings and URL fragments are stripped before
// resolution and not preserved.
func resolveRef(importerPath, spec string) string {
	if spec == "" {
		return ""
	}
	// Strip ?query and #fragment for resolution.
	if i := strings.IndexAny(spec, "?#"); i >= 0 {
		spec = spec[:i]
	}
	if spec == "" {
		return ""
	}
	if strings.HasPrefix(spec, "http://") ||
		strings.HasPrefix(spec, "https://") ||
		strings.HasPrefix(spec, "//") ||
		strings.HasPrefix(spec, "data:") {
		return ""
	}
	if strings.HasPrefix(spec, "/") {
		return cleanLogical(spec)
	}
	if strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../") {
		dir := path.Dir(importerPath)
		if dir == "." {
			dir = ""
		}
		resolved := path.Join(dir, spec)
		if resolved == "." || strings.HasPrefix(resolved, "../") || resolved == ".." {
			return ""
		}
		return resolved
	}
	// Bare specifier: importmap territory, not a rewrite target.
	return ""
}

// rewriteRefs returns a copy of content with each ref's spec replaced
// by the value replacement(r) returns. Refs whose replacement equals
// the original spec, or whose resolved field is empty, are left
// untouched.
func rewriteRefs(content []byte, refs []ref, replacement func(r ref) string) []byte {
	// Sort by start ascending so we can splice in one forward pass.
	sort.Slice(refs, func(i, j int) bool { return refs[i].start < refs[j].start })

	var out []byte
	cursor := 0
	for _, r := range refs {
		if r.resolved == "" {
			continue
		}
		repl := replacement(r)
		if repl == r.spec {
			continue
		}
		// Defensive: skip refs that overlap a prior rewrite (would
		// indicate two regexes matched the same byte range; rewrite
		// the first and ignore the second to avoid corruption).
		if r.start < cursor {
			continue
		}
		out = append(out, content[cursor:r.start]...)
		out = append(out, repl...)
		cursor = r.end
	}
	out = append(out, content[cursor:]...)
	return out
}

// topoSort orders assets so dependencies come before dependents. The
// returned slice is alphabetised within each "wave" of equally-ready
// nodes so consecutive compiles produce identical output. Cycles
// surface as an error listing the involved logical paths.
func topoSort(deps map[string][]string) ([]string, error) {
	dependents := map[string][]string{}
	indegree := map[string]int{}
	for node, ds := range deps {
		if _, ok := indegree[node]; !ok {
			indegree[node] = 0
		}
		for _, dep := range ds {
			dependents[dep] = append(dependents[dep], node)
			indegree[node]++
		}
	}

	var ready []string
	for node := range deps {
		if indegree[node] == 0 {
			ready = append(ready, node)
		}
	}
	sort.Strings(ready)

	order := make([]string, 0, len(deps))
	for len(ready) > 0 {
		n := ready[0]
		ready = ready[1:]
		order = append(order, n)
		var newly []string
		for _, dep := range dependents[n] {
			indegree[dep]--
			if indegree[dep] == 0 {
				newly = append(newly, dep)
			}
		}
		if len(newly) > 0 {
			ready = append(ready, newly...)
			sort.Strings(ready)
		}
	}

	if len(order) != len(deps) {
		var inCycle []string
		for node, deg := range indegree {
			if deg > 0 {
				inCycle = append(inCycle, node)
			}
		}
		sort.Strings(inCycle)
		return nil, &CycleError{Nodes: inCycle}
	}
	return order, nil
}

// CycleError reports a dependency cycle detected by Compile. Nodes
// is the alphabetised set of logical paths that participate in (or
// transitively depend on) the cycle; the cycle itself is somewhere
// in that set.
type CycleError struct {
	Nodes []string
}

func (e *CycleError) Error() string {
	return "assetmapper: dependency cycle among assets: " + strings.Join(e.Nodes, ", ")
}
