// Command assetmapper is the CLI for managing frontend assets with
// the assetmapper library: compiling source assets to a hashed
// public directory, vendoring JS packages via jspm.io, and inspecting
// the importmap.
//
// Install:
//
//	go install github.com/moostackhq/go/assetmapper/cmd/assetmapper@latest
//
// Defaults assume a conventional layout (override with flags or
// environment variables):
//
//	./assets/          source assets        (--source / ASSETMAPPER_SOURCE)
//	./public/assets/   compiled output      (--public / ASSETMAPPER_PUBLIC)
//	./importmap.json   importmap file       (--importmap / ASSETMAPPER_IMPORTMAP)
//	./assets/vendor/   vendored packages    (--vendor / ASSETMAPPER_VENDOR)
//	/assets/           served URL prefix    (--url-prefix / ASSETMAPPER_URL_PREFIX)
//
// Subcommands:
//
//	compile    hash and copy every asset into the public dir
//	list       print importmap entries
//	validate   check importmap + manifest + filesystem consistency
//	require    vendor a package (and transitive deps) from jspm.io
//	remove     drop a vendored package
//	prune      delete vendored files no longer referenced by importmap
//	update     re-resolve and re-download vendored packages
//	outdated   list packages with a newer upstream version available
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/moostackhq/go/assetmapper"
	"github.com/moostackhq/go/cli"
)

// ---------- shared flags (declared on root, inherited by children) ----------

var (
	sourceFlag = cli.StringFlag("source").
			Default("assets").
			Env("ASSETMAPPER_SOURCE").
			Help("source asset directory")

	publicFlag = cli.StringFlag("public").
			Default("public/assets").
			Env("ASSETMAPPER_PUBLIC").
			Help("public output directory for compile")

	importmapFlag = cli.StringFlag("importmap").
			Default("importmap.json").
			Env("ASSETMAPPER_IMPORTMAP").
			Help("path to importmap.json")

	vendorFlag = cli.StringFlag("vendor").
			Default("assets/vendor").
			Env("ASSETMAPPER_VENDOR").
			Help("vendored-packages directory")

	urlPrefixFlag = cli.StringFlag("url-prefix").
			Default("/assets/").
			Env("ASSETMAPPER_URL_PREFIX").
			Help("public URL prefix used in compiled refs")
)

// ---------- per-subcommand args ----------

var (
	requirePkgArg = cli.StringArg("package").Required().
			Help("npm-style package name (e.g. react, @radix-ui/themes)")
	requireVersionArg = cli.StringArg("version").
				Help("specific version; omit for latest stable")

	removePkgArg = cli.StringArg("package").Required().
			Help("specifier from importmap")

	updatePkgsArg = cli.StringSliceArg("packages").Variadic().
			Help("packages to update; omit with --all to update every vendored entry")

	updateAllFlag = cli.BoolFlag("all").
			Help("update every vendored entry when no packages are listed")

	outdatedCheckFlag = cli.BoolFlag("check").
				Help("exit non-zero when any vendored package is outdated (for CI)")
)

// ---------- subcommands ----------

var compileCmd = &cli.Command{
	Name: "compile",
	Help: "hash and copy every asset into the public dir, write manifest.json",
	Examples: []cli.Example{
		{Cmd: "assetmapper compile", Help: "use the defaults (./assets → ./public/assets)"},
		{Cmd: "assetmapper --source ui --public dist/static compile", Help: "non-default layout"},
	},
	Run: func(ctx cli.Context) error {
		roots := []assetmapper.Root{{FS: os.DirFS(sourceFlag.Get(ctx))}}
		manifest, err := assetmapper.Compile(roots, publicFlag.Get(ctx),
			assetmapper.CompileOptions{URLPrefix: urlPrefixFlag.Get(ctx)})
		if err != nil {
			return err
		}
		fmt.Fprintf(ctx.Stdout(), "compiled %d asset(s) into %s\n",
			len(manifest.Entries), publicFlag.Get(ctx))
		return nil
	},
}

var validateCmd = &cli.Command{
	Name: "validate",
	Help: "check importmap + manifest + filesystem consistency",
	Long: "Walks every importmap entry and (if manifest.json exists) every manifest entry, " +
		"reporting issues like: malformed entries (path+version both set, or neither), " +
		"missing local source files, missing vendored package files, manifest entries that " +
		"point at non-existent compiled files. Exits non-zero if any issue is found, " +
		"suitable for CI gating.",
	Examples: []cli.Example{
		{Cmd: "assetmapper validate", Help: "run all checks; exit 1 on any issue"},
	},
	Run: func(ctx cli.Context) error {
		var issues []string
		issuef := func(format string, args ...any) {
			issues = append(issues, fmt.Sprintf(format, args...))
		}

		im, err := loadOrEmptyImportmap(importmapFlag.Get(ctx))
		if err != nil {
			return fmt.Errorf("load importmap: %w", err)
		}

		srcDir := sourceFlag.Get(ctx)
		mapper, err := assetmapper.New(assetmapper.Config{
			Roots: []assetmapper.Root{{FS: os.DirFS(srcDir)}},
		})
		if err != nil {
			return fmt.Errorf("build mapper for %s: %w", srcDir, err)
		}

		// 1. Importmap entries: shape + on-disk existence.
		for _, key := range sortedKeys(im.Entries) {
			entry := im.Entries[key]
			switch {
			case entry.Path == "" && entry.Version == "":
				issuef("importmap %q: entry has neither path nor version", key)
				continue
			case entry.Path != "" && entry.Version != "":
				issuef("importmap %q: entry has both path and version (use one)", key)
				continue
			}
			logical := entry.Path
			if logical == "" {
				ext := ".js"
				if entry.Type == "css" {
					ext = ".css"
				}
				logical = "vendor/" + key + ext
			}
			if _, err := mapper.Asset(logical); err != nil {
				issuef("importmap %q: %v", key, err)
			}
		}

		// 2. Manifest entries: target file must exist in publicDir.
		// Track whether a manifest was found so the success message
		// distinguishes "manifest verified" from "no manifest yet".
		var (
			manifestFound bool
			manifestCount int
		)
		publicDir := publicFlag.Get(ctx)
		manifestPath := filepath.Join(publicDir, assetmapper.ManifestFilename)
		switch _, err := os.Stat(manifestPath); {
		case err == nil:
			manifest, err := assetmapper.LoadManifest(publicDir)
			if err != nil {
				issuef("manifest: %v", err)
				break
			}
			manifestFound = true
			manifestCount = len(manifest.Entries)
			mkeys := make([]string, 0, len(manifest.Entries))
			for k := range manifest.Entries {
				mkeys = append(mkeys, k)
			}
			sort.Strings(mkeys)
			for _, k := range mkeys {
				hashed := manifest.Entries[k]
				full := filepath.Join(publicDir, filepath.FromSlash(hashed))
				if _, err := os.Stat(full); os.IsNotExist(err) {
					issuef("manifest %q: %q missing at %s", k, hashed, full)
				}
			}
		case os.IsNotExist(err):
			// No manifest yet (nothing compiled). Skip — not an error.
		default:
			issuef("manifest: stat: %v", err)
		}

		if len(issues) > 0 {
			for _, m := range issues {
				fmt.Fprintln(ctx.Stdout(), m)
			}
			return fmt.Errorf("%d validation issue(s)", len(issues))
		}
		if manifestFound {
			fmt.Fprintf(ctx.Stdout(), "OK: %d importmap entries, %d manifest entries verified\n",
				len(im.Entries), manifestCount)
		} else {
			fmt.Fprintf(ctx.Stdout(), "OK: %d importmap entries (no manifest)\n", len(im.Entries))
		}
		return nil
	},
}

var listCmd = &cli.Command{
	Name: "list",
	Help: "print importmap entries (local + vendored)",
	Run: func(ctx cli.Context) error {
		im, err := loadOrEmptyImportmap(importmapFlag.Get(ctx))
		if err != nil {
			return err
		}
		if len(im.Entries) == 0 {
			fmt.Fprintln(ctx.Stdout(), "(no entries)")
			return nil
		}
		for _, k := range sortedKeys(im.Entries) {
			entry := im.Entries[k]
			switch {
			case entry.Path != "":
				fmt.Fprintf(ctx.Stdout(), "%s -> %s", k, entry.Path)
			case entry.Version != "":
				fmt.Fprintf(ctx.Stdout(), "%s @ %s", k, entry.Version)
			default:
				fmt.Fprintf(ctx.Stdout(), "%s (incomplete)", k)
			}
			if entry.Type == "css" {
				fmt.Fprint(ctx.Stdout(), " [css]")
			}
			if entry.Entrypoint {
				fmt.Fprint(ctx.Stdout(), " [entrypoint]")
			}
			fmt.Fprintln(ctx.Stdout())
		}
		return nil
	},
}

var requireCmd = &cli.Command{
	Name: "require",
	Help: "vendor a package (and its transitive deps) into the vendor dir",
	Args: cli.Args(requirePkgArg, requireVersionArg),
	Examples: []cli.Example{
		{Cmd: "assetmapper require react", Help: "vendor latest stable react"},
		{Cmd: "assetmapper require react 18.2.0", Help: "pin a specific version"},
	},
	Run: func(ctx cli.Context) error {
		v, im, err := newVendor(ctx)
		if err != nil {
			return err
		}
		before := keySet(im.Entries)
		if err := v.Require(ctx, requirePkgArg.Get(ctx), requireVersionArg.Get(ctx)); err != nil {
			return err
		}
		for _, k := range sortedKeys(im.Entries) {
			if !before[k] {
				fmt.Fprintf(ctx.Stdout(), "+ %s@%s\n", k, im.Entries[k].Version)
			}
		}
		return im.Save(importmapFlag.Get(ctx))
	},
}

var removeCmd = &cli.Command{
	Name: "remove",
	Help: "drop a vendored package and its importmap entry",
	Args: cli.Args(removePkgArg),
	Run: func(ctx cli.Context) error {
		v, im, err := newVendor(ctx)
		if err != nil {
			return err
		}
		spec := removePkgArg.Get(ctx)
		if err := v.Remove(spec); err != nil {
			return err
		}
		fmt.Fprintf(ctx.Stdout(), "- %s\n", spec)
		return im.Save(importmapFlag.Get(ctx))
	},
}

var pruneCmd = &cli.Command{
	Name: "prune",
	Help: "delete vendored files not referenced by any importmap entry",
	Long: "Useful after `remove` (which doesn't cascade to transitive " +
		"deps) or after hand-editing importmap.json to drop entries. " +
		"Walks the vendor dir, deletes anything not registered as a " +
		"vendored entry, and cleans up the empty directories left behind.",
	Run: func(ctx cli.Context) error {
		v, _, err := newVendor(ctx)
		if err != nil {
			return err
		}
		removed, err := v.Prune()
		if err != nil {
			return err
		}
		if len(removed) == 0 {
			fmt.Fprintln(ctx.Stdout(), "nothing to prune")
			return nil
		}
		for _, p := range removed {
			fmt.Fprintf(ctx.Stdout(), "- %s\n", p)
		}
		return nil
	},
}

var updateCmd = &cli.Command{
	Name:  "update",
	Help:  "re-resolve and re-download vendored packages to their latest versions",
	Args:  cli.Args(updatePkgsArg),
	Flags: cli.Flags(updateAllFlag),
	Examples: []cli.Example{
		{Cmd: "assetmapper update react", Help: "update a single package"},
		{Cmd: "assetmapper update --all", Help: "update every vendored entry"},
	},
	Run: func(ctx cli.Context) error {
		v, im, err := newVendor(ctx)
		if err != nil {
			return err
		}
		targets := updatePkgsArg.Get(ctx)
		if len(targets) == 0 {
			if !updateAllFlag.Get(ctx) {
				return cli.UsageError("specify package(s) to update or pass --all to update every vendored entry")
			}
			for k, e := range im.Entries {
				if e.Version != "" {
					targets = append(targets, k)
				}
			}
			sort.Strings(targets)
		}
		for _, pkg := range targets {
			entry, ok := im.Entries[pkg]
			if !ok || entry.Version == "" {
				fmt.Fprintf(ctx.Stderr(), "skip %s (not a vendored entry)\n", pkg)
				continue
			}
			prev := entry.Version
			if err := v.Require(ctx, pkg, ""); err != nil {
				return fmt.Errorf("update %s: %w", pkg, err)
			}
			now := im.Entries[pkg].Version
			if prev == now {
				fmt.Fprintf(ctx.Stdout(), "%s: %s (unchanged)\n", pkg, prev)
			} else {
				fmt.Fprintf(ctx.Stdout(), "%s: %s -> %s\n", pkg, prev, now)
			}
		}
		return im.Save(importmapFlag.Get(ctx))
	},
}

var outdatedCmd = &cli.Command{
	Name:  "outdated",
	Help:  "list vendored packages with a newer upstream version available",
	Flags: cli.Flags(outdatedCheckFlag),
	Examples: []cli.Example{
		{Cmd: "assetmapper outdated", Help: "print outdated packages"},
		{Cmd: "assetmapper outdated --check", Help: "exit 1 if any are outdated (CI use)"},
	},
	Run: func(ctx cli.Context) error {
		resolver := newResolver()
		im, err := loadOrEmptyImportmap(importmapFlag.Get(ctx))
		if err != nil {
			return err
		}
		any := false
		for _, pkg := range sortedKeys(im.Entries) {
			entry := im.Entries[pkg]
			if entry.Version == "" {
				continue
			}
			res, err := resolver.Resolve(ctx, []assetmapper.PackageRequest{{Name: pkg}})
			if err != nil {
				return fmt.Errorf("resolve %s: %w", pkg, err)
			}
			for _, p := range res.Packages {
				if p.Specifier == pkg && p.Version != "" && p.Version != entry.Version {
					fmt.Fprintf(ctx.Stdout(), "%s: %s -> %s\n", pkg, entry.Version, p.Version)
					any = true
					break
				}
			}
		}
		if !any {
			fmt.Fprintln(ctx.Stdout(), "all vendored packages up to date")
			return nil
		}
		if outdatedCheckFlag.Get(ctx) {
			return ErrOutdated
		}
		return nil
	},
}

// ErrOutdated is returned by `outdated --check` when at least one
// vendored package has a newer upstream version. Surfaced as a
// distinct error so CI pipelines can branch on errors.Is.
var ErrOutdated = errors.New("vendored packages are outdated")

// ---------- root ----------

var root = &cli.Command{
	Name:    "assetmapper",
	Version: "0.1.0",
	Help:    "manage frontend assets (compile, importmap, vendor packages)",
	Long: "assetmapper compiles source assets to a content-hashed public " +
		"directory, manages an importmap, and vendors JS packages from " +
		"jspm.io. State lives in two files on disk: importmap.json (committed " +
		"to git, describes what the page loads) and manifest.json (the deploy " +
		"artifact, maps logical paths to hashed filenames).",
	Flags: cli.Flags(sourceFlag, publicFlag, importmapFlag, vendorFlag, urlPrefixFlag),
	Subcommands: []*cli.Command{
		compileCmd, listCmd, validateCmd, requireCmd, removeCmd, pruneCmd, updateCmd, outdatedCmd,
	},
}

func main() {
	os.Exit(root.Exec(os.Args[1:]))
}

// ---------- helpers ----------

// resolverOverride is set by tests to swap the upstream JSPM resolver
// for a stub. Production code path runs newResolver() which returns
// the real [assetmapper.JSPMResolver].
var resolverOverride assetmapper.PackageResolver

func newResolver() assetmapper.PackageResolver {
	if resolverOverride != nil {
		return resolverOverride
	}
	return assetmapper.NewJSPMResolver(nil)
}

func newVendor(ctx cli.Context) (*assetmapper.Vendor, *assetmapper.Importmap, error) {
	im, err := loadOrEmptyImportmap(importmapFlag.Get(ctx))
	if err != nil {
		return nil, nil, err
	}
	return &assetmapper.Vendor{
		Resolver:  newResolver(),
		VendorDir: vendorFlag.Get(ctx),
		Importmap: im,
	}, im, nil
}

func loadOrEmptyImportmap(path string) (*assetmapper.Importmap, error) {
	if path == "" {
		return assetmapper.NewImportmap(), nil
	}
	im, err := assetmapper.LoadImportmap(path)
	if errors.Is(err, os.ErrNotExist) {
		return assetmapper.NewImportmap(), nil
	}
	return im, err
}

func sortedKeys(m map[string]assetmapper.ImportmapEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func keySet(m map[string]assetmapper.ImportmapEntry) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}
