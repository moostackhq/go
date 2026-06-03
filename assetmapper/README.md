# assetmapper

Hash, serve, and vendor frontend assets from Go — no Node, no bundler. Inspired by Symfony AssetMapper: native browser features (ES modules + importmaps) make the JS toolchain mostly unnecessary in 2024.

## Features

| Feature | What it gives you |
|---|---|
| Content-hashed URLs | `Mapper.Asset("app.js")` → `/assets/app-7a1b2c3d.js`. URL changes when content changes; safe to serve with `Cache-Control: immutable`. |
| Dev / prod from one Mapper | Pass a `Manifest` for prod (manifest-driven lookups). Omit it for dev (lazy hash on first read, in-memory cache). Same `Asset` API. |
| Multi-root with mount prefixes | `[]Root{{FS: userFS}, {FS: libFS, MountAt: "lib"}}`. First match wins so user assets shadow library defaults. Works with `embed.FS` for satellite-shipped assets. |
| HTTP handler in dev | `Mapper.Handler()` is an `http.Handler` with ETag, 304 on `If-None-Match`, HEAD, method-not-allowed. Stale hash in URL still serves current content. |
| Compile to a deploy artifact | `Compile(roots, publicDir)` walks every asset, hashes, rewrites internal refs, copies to `name-HASH.ext`, writes `manifest.json` with sorted keys. Idempotent for unchanged input. |
| Reference rewriting | JS `import` / `export` / dynamic `import()` and CSS `url()` / `@import` rewritten to point at hashed filenames. Bare specifiers and external URLs left alone. Cycles surface as `*CycleError`. |
| Transitive cache busting | Changing `util.js` changes `app.js`'s hash too, because `app.js`'s rewritten content embeds the new util URL. Topo-sorted compile guarantees correct ordering. |
| Importmap + entrypoints | `Importmap.Render(mapper, "app")` emits `<script type="importmap">` + modulepreload links + `<script type="module">import "app";</script>`. CSS entrypoints emit `<link rel="stylesheet">`. |
| Module preload | `Render` walks the JS import graph from each entrypoint and emits `<link rel="modulepreload">` per transitive JS dep, plus `<link rel="preload" as="style">` for CSS files imported from JS (`import "./x.css"`). Browser fetches in parallel with the importmap parse. |
| CSP nonce support | `RenderWithOptions(mapper, RenderOptions{Entrypoints: ..., Nonce: nonce})` adds `nonce="..."` to every emitted `<script>` and `<link>` so the output is loadable under `Content-Security-Policy: script-src 'nonce-XYZ'` / `style-src 'nonce-XYZ'`. Same shape for `ModuleModulePreloadLinksWithOptions`. |
| Vendor packages via jspm.io | `Vendor.Require("react", "18.2.0")` resolves transitive deps via jspm.io, downloads each file, rewrites upstream URLs to bare specifiers, drops files under `assets/vendor/`, updates `importmap.json`. |
| Pluggable resolver | `PackageResolver` interface; default is `JSPMResolver` against `api.jspm.io/generate`, swappable for a mirror or stub. |
| html/template integration | `assetmapper/template` ships a `FuncMap` with `asset`, `importmap`, `module_preload_links` — drop into `template.New(...).Funcs(...)`. |
| Standalone CLI | `assetmapper/cmd/assetmapper` is an installable binary for compile / require / list workflows. See [`cmd/assetmapper/README.md`](cmd/assetmapper/README.md). |

## Install

```bash
go get github.com/moostackhq/go/assetmapper
```

Stdlib only in the core. The vendor flow's JSPM resolver uses `net/http`.

## Quickstart

Dev: serve assets from source, no compile step.

```go
package main

import (
    "errors"
    "html/template"
    "net/http"
    "os"

    "github.com/moostackhq/go/assetmapper"
    asstmpl "github.com/moostackhq/go/assetmapper/template"
)

func main() {
    mapper, _ := assetmapper.New(assetmapper.Config{
        Roots: []assetmapper.Root{{FS: os.DirFS("assets")}},
    })
    im, err := assetmapper.LoadImportmap("importmap.json")
    if errors.Is(err, os.ErrNotExist) {
        im = assetmapper.NewImportmap()
    }

    page := template.Must(template.New("page.html").
        Funcs(asstmpl.FuncMap(mapper, im)).
        ParseFiles("page.html"))

    mux := http.NewServeMux()
    mux.Handle("/assets/", mapper.Handler())
    mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
        _ = page.Execute(w, nil)
    })
    _ = http.ListenAndServe(":8080", mux)
}
```

Prod: pre-compile, load the manifest, serve `publicDir` with any static-file server.

```go
manifest, _ := assetmapper.LoadManifest("public/assets")
mapper, _ := assetmapper.New(assetmapper.Config{
    Roots:    []assetmapper.Root{{FS: os.DirFS("assets")}},
    Manifest: manifest,
})
// mapper.Handler() refuses requests in prod; serve public/assets/ statically.
```

In a template:

```html
<head>
  {{ importmap "app" }}
</head>
<body>
  <img src="{{ asset "images/logo.png" }}">
</body>
```

The same template runs in dev and prod — only the Mapper differs.

## Concepts

### Logical path → public URL

The developer writes `app.js`, `images/logo.png` (logical paths). The browser sees `/assets/app-7a1b2c3d.js`, `/assets/images/logo-cafef00d.png` (public URLs). `Mapper.Asset(logicalPath)` is the only resolver users call; `URLPrefix` defaults to `/assets/` and is configurable.

### Manifest

`Manifest` is the `(URLPrefix, map[logicalPath]publicFileName)` produced by `Compile` and consumed in prod mode. Persisted as `manifest.json` inside the public output directory — `Entries`' keys are alphabetically sorted for diff stability — so the same artifact a CDN serves is also the source of truth at runtime.

`URLPrefix` is stored in the manifest so `New` can detect drift: if `Compile` ran with `/assets/` baked into every rewritten reference but the runtime `Config.URLPrefix` is `/static/`, `New` refuses at boot rather than letting the site half-load (browser hits `/static/app-HASH.js` for the script tag but the script's content imports `/assets/util-HASH.js`).

### Importmap

`Importmap` is the project's `importmap.json` (committed to git). Entries are one of:

- **Local**: `Path` set. Resolved via `Mapper.Asset` so dev/prod hashing is automatic.
- **Vendored**: `Version` set. File lives at `vendor/<key>.js` (or `.css`) by convention; downloaded by `Vendor.Require`.

Either kind can carry `Entrypoint: true` to participate in `Render`'s entrypoint script tags.

### Vendor

`Vendor.Require` resolves a package via the configured `PackageResolver` (default: jspm.io), downloads it and every transitive dep, rewrites upstream URLs inside the downloaded JS to bare specifiers, and saves under `assets/vendor/<spec>.js` (or `.css`). The on-disk vendor directory is committed to git so production deploys never touch the network.

### Compile

`Compile(roots, publicDir, opts...)`:

1. Walks every asset under every root (first-root-wins shadowing for duplicates).
2. Extracts JS and CSS references per file; builds a dep graph.
3. Topo-sorts (cycle → `*CycleError`).
4. For each asset in dep order: rewrites refs using the now-known hashes of deps, hashes the rewritten content, writes `publicDir/name-HASH.ext`.
5. Writes `manifest.json`.

Hashing the *rewritten* content is what makes transitive cache-busting work: changing a leaf file changes every ancestor's hash, so a stale CSS file can't end up pointing at a deleted hashed image.

## Reference rewriting

What gets rewritten:

- JS: `import x from "./util.js"`, `export {a} from "./util.js"`, `export * from "./util.js"`, dynamic `import("./lazy.js")` — single-line and multi-line, all quote styles.
- CSS: `url(...)` with no quotes / single / double, and `@import` with bare string or `url(...)`.

What's left alone:

- Bare specifiers (`import "react"`) — handled by the importmap.
- External URLs (`http://`, `https://`, `//`, `data:`).
- SVG fragments (`url(#myGradient)`).
- Any specifier that doesn't resolve to a known asset (probably a typo — left for the dev to fix).

### Known limitation

The regex sees inside string literals and comments, so a line like `// import "./real-file.js"` referencing an actual asset would be rewritten. Workaround: don't put valid-looking import syntax in comments that reference real files. Hasn't been a problem in practice.

## Importmap rendering

`Importmap.Render(mapper, entrypoints...)` emits in this order:

1. `<script type="importmap">{...}</script>` — every entry, resolved to its public URL.
2. `<link rel="modulepreload" href="...">` — one per JS module reachable from any JS entrypoint, DFS-ordered, deduplicated.
3. `<link rel="preload" as="style" href="...">` — one per CSS file reached via `import "./x.css"` from JS.
4. `<link rel="stylesheet" href="...">` — one per CSS entrypoint.
5. `<script type="module">import "name";</script>` — one per JS entrypoint. The browser uses the importmap (step 1) to resolve the bare specifier; the module is already cached thanks to step 2.

`Importmap.ModulePreloadLinks(mapper, entrypoints...)` returns just step 2, for callers composing the HTML themselves.

### CSP nonces

Apps running under a strict `Content-Security-Policy` (e.g. `script-src 'self' 'nonce-XYZ'`) need every `<script>` and `<link>` tag to carry the per-request nonce. Use the `WithOptions` variants:

```go
html, err := im.RenderWithOptions(mapper, assetmapper.RenderOptions{
    Entrypoints: []string{"app"},
    Nonce:       req.CSPNonce, // your per-request nonce
})
```

The same nonce is applied to every emitted `<script>` and `<link>`. The template satellite ships matching `importmap_nonce` / `module_preload_links_nonce` helpers — see [`assetmapper/template`](template/).

## Vendoring

```go
v := &assetmapper.Vendor{
    Resolver:  assetmapper.NewJSPMResolver(nil), // nil = default 30s-timeout client
    VendorDir: "assets/vendor",
    Importmap: im,
}
if err := v.Require(ctx, "react", "18.2.0"); err != nil { ... }
_ = im.Save("importmap.json")
```

Transitive deps are downloaded alongside the requested package, with upstream URLs in JS files rewritten to bare specifiers so the importmap resolves them. `Vendor.Remove(spec)` deletes the file and importmap entry but does NOT cascade to transitive deps (which may be shared).

Most users will reach for the CLI (`assetmapper require react`) rather than calling this directly.

## Integration packages

- `assetmapper/template` — html/template `FuncMap` exposing `asset`, `importmap`, `module_preload_links`.
- `assetmapper/cmd/assetmapper` — standalone CLI binary; see its own [README](cmd/assetmapper/README.md).

## Limitations

- **No Sass / TypeScript compilation.** Out of scope. Write modern CSS + vanilla JS; for preprocessor needs use a separate build step that drops files into the asset roots.
- **Source files needed for full preloading.** `ModulePreloadLinks` reads JS source to walk the import graph. Deployments that ship only the compiled output get preloads for entrypoints but not their transitive deps.
- **jspm.io is a network dependency for `Vendor.Require`.** Vendored files are committed to git, so production deploys never need network — only `require` / `update` do.
- **Conflicting transitive versions collapse.** Two packages depending on different versions of the same third package lose the scoping that would disambiguate them in jspm.io's nested importmap output. Rare in practice for browser-facing JS.
- **Importmap browser support.** Solid since 2023 (Safari 16.4 was last). Older browsers need a polyfill (e.g. `es-module-shims`).
