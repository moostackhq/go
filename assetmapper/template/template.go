// Package template wires asset URL resolution, importmap rendering,
// and modulepreload generation into an [html/template] [FuncMap] so
// server-rendered pages can emit asset references without manual
// scaffolding.
//
// The package name collides with the standard library's
// [html/template]; users importing both in the same file should
// alias:
//
//	import (
//	    "html/template"
//	    asstmpl "github.com/moostackhq/go/assetmapper/template"
//	)
//
// Wiring:
//
//	tpl := template.Must(template.New("page.html").
//	    Funcs(asstmpl.FuncMap(mapper, importmap)).
//	    ParseFiles("page.html"))
//
// In the template itself:
//
//	<head>
//	  {{ importmap "app" }}
//	</head>
//	<body>
//	  <img src="{{ asset "images/logo.png" }}">
//	</body>
//
// Swapping dev / prod is a matter of constructing the [Mapper] with
// or without a [Manifest] and passing it to FuncMap. Templates do
// not change.
package template

import (
	htmltemplate "html/template"

	"github.com/moostackhq/go/assetmapper"
)

// FuncMap returns an [html/template] [FuncMap] with five helpers
// bound to mapper and im:
//
//   - asset "path" → string URL. html/template's context-aware
//     auto-escaping handles HTML attribute, URL attribute, and JS
//     contexts correctly because the helper returns a plain string,
//     not [htmltemplate.HTML].
//   - importmap "entry"... → [htmltemplate.HTML] containing the
//     <script type="importmap"> block, modulepreload links, and
//     entrypoint tags for the named entries.
//   - importmap_nonce "NONCE" "entry"... → same as importmap but
//     adds nonce="NONCE" to every <script> and <link>. Use under
//     Content-Security-Policy with script-src 'nonce-XYZ' /
//     style-src 'nonce-XYZ'.
//   - module_preload_links "entry"... → [htmltemplate.HTML] containing
//     just the <link rel="modulepreload"> tags (use when composing
//     the importmap scaffolding manually).
//   - module_preload_links_nonce "NONCE" "entry"... → same as
//     module_preload_links with nonce attributes.
//
// All helpers return errors as a second value, which html/template
// surfaces as execution errors. Common cases: missing asset, typo'd
// entrypoint name, entry not marked as entrypoint.
func FuncMap(m *assetmapper.Mapper, im *assetmapper.Importmap) htmltemplate.FuncMap {
	return htmltemplate.FuncMap{
		"asset": func(logicalPath string) (string, error) {
			return m.Asset(logicalPath)
		},
		"importmap": func(entrypoints ...string) (htmltemplate.HTML, error) {
			s, err := im.Render(m, entrypoints...)
			if err != nil {
				return "", err
			}
			return htmltemplate.HTML(s), nil
		},
		"importmap_nonce": func(nonce string, entrypoints ...string) (htmltemplate.HTML, error) {
			s, err := im.RenderWithOptions(m, assetmapper.RenderOptions{
				Entrypoints: entrypoints,
				Nonce:       nonce,
			})
			if err != nil {
				return "", err
			}
			return htmltemplate.HTML(s), nil
		},
		"module_preload_links": func(entrypoints ...string) (htmltemplate.HTML, error) {
			s, err := im.ModulePreloadLinks(m, entrypoints...)
			if err != nil {
				return "", err
			}
			return htmltemplate.HTML(s), nil
		},
		"module_preload_links_nonce": func(nonce string, entrypoints ...string) (htmltemplate.HTML, error) {
			s, err := im.ModulePreloadLinksWithOptions(m, assetmapper.RenderOptions{
				Entrypoints: entrypoints,
				Nonce:       nonce,
			})
			if err != nil {
				return "", err
			}
			return htmltemplate.HTML(s), nil
		},
	}
}
