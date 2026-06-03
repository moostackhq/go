# assetmapper (CLI)

Standalone binary for managing frontend assets with the [assetmapper library](../../README.md): hash + compile to a public directory, vendor JS packages from jspm.io, inspect the importmap.

## Install

```bash
go install github.com/moostackhq/go/assetmapper/cmd/assetmapper@latest
```

Verify:

```bash
assetmapper --version
```

## Quickstart

```bash
# 1. Add some source assets.
mkdir -p assets
cat > assets/app.js <<'EOF'
import { greet } from "./greet.js";
greet("world");
EOF
cat > assets/greet.js <<'EOF'
export function greet(name) { console.log("hello, " + name); }
EOF

# 2. Vendor a JS package (needs network — hits jspm.io).
assetmapper require lodash

# 3. Compile for production.
assetmapper compile

# 4. Inspect.
assetmapper list
```

After `compile`, hashed files live under `public/assets/` alongside `manifest.json`. The runtime serves that directory; the source `assets/` is the editable input.

## Subcommands

| Command | What it does |
|---|---|
| `compile` | Walk every asset, hash + rewrite refs, copy to `--public`, write `manifest.json`. |
| `list` | Print importmap entries (local + vendored). |
| `validate` | Check importmap + manifest + filesystem consistency: missing source files, missing vendored packages, malformed entries, manifest entries pointing at non-existent compiled files. Exits non-zero on any issue (CI gating). |
| `require <pkg> [version]` | Vendor a package and its transitive deps from jspm.io; update `importmap.json`. Empty version = latest stable. |
| `remove <pkg>` | Drop a vendored package and its importmap entry (does NOT cascade to transitive deps — follow up with `prune`). |
| `prune` | Delete vendored files no longer referenced by any importmap entry. Use after `remove` or after hand-editing `importmap.json`. |
| `update <pkg ...>` | Re-resolve and re-download the listed vendored packages to latest. Use `--all` instead of listing to update every vendored entry (explicit so reflexive `update` doesn't surprise). |
| `outdated` | List vendored packages with a newer upstream version available. Use `--check` to exit non-zero when any are outdated (for CI). |

`require`, `update`, and `outdated` hit the network (jspm.io). `compile`, `list`, and `remove` are offline.

## Flags and environment

Flags live on the root and inherit to every subcommand. Each has an env-var override.

| Flag | Env | Default | What it controls |
|---|---|---|---|
| `--source` | `ASSETMAPPER_SOURCE` | `assets` | Source asset directory walked by `compile`. |
| `--public` | `ASSETMAPPER_PUBLIC` | `public/assets` | Output directory for `compile`. |
| `--importmap` | `ASSETMAPPER_IMPORTMAP` | `importmap.json` | Path to importmap.json. |
| `--vendor` | `ASSETMAPPER_VENDOR` | `assets/vendor` | Directory `require` / `remove` / `update` write into. |
| `--url-prefix` | `ASSETMAPPER_URL_PREFIX` | `/assets/` | Public URL prefix used when rewriting refs at compile time. |

## Expected layout

The defaults assume:

```
.
├── assets/                      # source assets (--source)
│   ├── app.js
│   ├── images/
│   │   └── logo.png
│   └── vendor/                  # downloaded packages (--vendor)
│       ├── react.js
│       └── scheduler.js
├── public/assets/               # compiled output (--public)
│   ├── app-7a1b2c3d.js
│   ├── images/logo-cafef00d.png
│   └── manifest.json
└── importmap.json               # bare-specifier → entry (--importmap)
```

Both `importmap.json` and `assets/vendor/` are meant to be committed to git — production deploys never need to call out to jspm.io. The deploy artifact is `public/assets/`.

## Typical workflow

```bash
# Add a package.
assetmapper require react
# + react@18.2.0
# + scheduler@0.23.0

# See what's installed.
assetmapper list

# Stay current.
assetmapper outdated
# react: 18.2.0 -> 18.3.0
assetmapper update react
# react: 18.2.0 -> 18.3.0

# Drop a package and clean up orphaned transitive deps.
assetmapper remove react
# - react
assetmapper prune
# - scheduler.js

# Build for prod.
assetmapper compile
# compiled 24 asset(s) into public/assets
```

## CI examples

Fail CI when vendored packages are out of date:

```bash
assetmapper outdated --check
```

Exits 0 when everything is current, non-zero (with the diff printed to stdout) otherwise.

Fail CI when importmap or compiled artifact is inconsistent:

```bash
assetmapper validate
```

Catches malformed importmap entries, missing source / vendored files, and manifest entries pointing at non-existent compiled outputs. Exits non-zero with one issue per line on stdout.

Produce the deploy artifact during a release pipeline:

```bash
assetmapper compile
# Ship public/assets/ — the manifest.json inside it is what the runtime reads.
```

## When to skip the CLI

The CLI handles single-source-directory layouts. If you need:

- Multiple asset roots (e.g. embedding a library's `embed.FS` alongside your project's assets), or
- Custom resolvers (mirror, registry other than jspm.io), or
- Compile from inside a Go program instead of a shell, or
- `validate` against a multi-root setup (the CLI's `validate` only checks `--source`),

…use the [library](../../README.md) directly. Five lines of Go cover all of them.

## See also

- [Library README](../../README.md) for the embeddable Go API and conceptual background.
