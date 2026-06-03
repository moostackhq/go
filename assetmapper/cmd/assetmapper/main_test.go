package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moostackhq/go/assetmapper"
	"github.com/moostackhq/go/cli"
)

// run executes the root command tree with captured stdout/stderr.
// Tests pass absolute paths via the root --source / --public /
// --importmap / --vendor flags so no os.Chdir is needed; that lets
// every test call t.Parallel().
func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := root.Exec(args,
		cli.WithStdout(&out),
		cli.WithStderr(&errOut),
		cli.WithSignalContext(context.Background()),
	)
	return code, out.String(), errOut.String()
}

// projectFlags returns root-flag args that point at dir's conventional
// layout, suitable for prepending to a subcommand invocation.
func projectFlags(dir string) []string {
	return []string{
		"--source", filepath.Join(dir, "assets"),
		"--public", filepath.Join(dir, "public", "assets"),
		"--importmap", filepath.Join(dir, "importmap.json"),
		"--vendor", filepath.Join(dir, "assets", "vendor"),
	}
}

// stubResolver fakes [assetmapper.PackageResolver] for tests that
// shouldn't hit the network.
type stubResolver struct {
	resolution *assetmapper.Resolution
	fetched    map[string][]byte
}

func (s *stubResolver) Resolve(ctx context.Context, reqs []assetmapper.PackageRequest) (*assetmapper.Resolution, error) {
	return s.resolution, nil
}

func (s *stubResolver) Fetch(ctx context.Context, url string) ([]byte, error) {
	return s.fetched[url], nil
}

// withStub swaps the resolver for a stub for the duration of the
// test. Mutates a package-level var, so the helper does NOT enable
// t.Parallel within the same goroutine — tests that need parallelism
// must construct their stubs without sharing.
//
// Most resolver-touching tests are intentionally NOT parallel for
// this reason; they're explicitly noted below.
func withStub(t *testing.T, r assetmapper.PackageResolver) {
	t.Helper()
	prev := resolverOverride
	resolverOverride = r
	t.Cleanup(func() { resolverOverride = prev })
}

// --- root + tree shape ---

func TestRoot_HasAllSubcommands(t *testing.T) {
	t.Parallel()
	want := []string{"compile", "list", "validate", "require", "remove", "prune", "update", "outdated"}
	got := map[string]bool{}
	for _, sc := range root.Subcommands {
		got[sc.Name] = true
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("subcommand %q missing", name)
		}
	}
}

func TestRoot_VersionFlag(t *testing.T) {
	t.Parallel()
	code, out, _ := run(t, "--version")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "0.1.0") {
		t.Errorf("--version output = %q, want version string", out)
	}
}

// --- compile ---

func TestCompile_HappyPath(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "assets", "app.js"),
		[]byte("export default {}"), 0o644); err != nil {
		t.Fatal(err)
	}

	args := append(projectFlags(tmp), "compile")
	code, out, errOut := run(t, args...)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "compiled 1 asset(s)") {
		t.Errorf("stdout = %q, want compile summary", out)
	}
	if _, err := os.Stat(filepath.Join(tmp, "public", "assets", "manifest.json")); err != nil {
		t.Errorf("manifest.json not produced: %v", err)
	}
}

func TestCompile_RespectsCustomPaths(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "ui"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "ui", "app.js"),
		[]byte("export default {}"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, _, errOut := run(t,
		"--source", filepath.Join(tmp, "ui"),
		"--public", filepath.Join(tmp, "dist"),
		"compile",
	)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(tmp, "dist", "manifest.json")); err != nil {
		t.Errorf("manifest.json missing at custom --public path: %v", err)
	}
}

// --- list ---

func TestList_EmptyImportmap(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	args := append(projectFlags(tmp), "list")
	code, out, _ := run(t, args...)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "(no entries)") {
		t.Errorf("stdout = %q, want no-entries placeholder", out)
	}
}

func TestList_ShowsEntries(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}
	im.Entries["react"] = assetmapper.ImportmapEntry{Version: "18.2.0"}
	im.Entries["styles"] = assetmapper.ImportmapEntry{
		Path: "styles/main.css", Type: "css", Entrypoint: true,
	}
	if err := im.Save(filepath.Join(tmp, "importmap.json")); err != nil {
		t.Fatal(err)
	}

	args := append(projectFlags(tmp), "list")
	code, out, _ := run(t, args...)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{
		"app -> app.js [entrypoint]",
		"react @ 18.2.0",
		"styles -> styles/main.css [css] [entrypoint]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestList_MissingImportmapFileTreatedAsEmpty(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	args := append(projectFlags(tmp), "list")
	code, out, _ := run(t, args...)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(out, "(no entries)") {
		t.Errorf("stdout = %q", out)
	}
}

// --- require (resolver-touching tests are serial: shared resolverOverride) ---

func TestRequire_VendorsPackageAndUpdatesImportmap(t *testing.T) {
	tmp := t.TempDir()
	withStub(t, &stubResolver{
		resolution: &assetmapper.Resolution{
			Packages: []assetmapper.ResolvedPackage{
				{Specifier: "react", Version: "18.2.0", Type: "js",
					URL: "https://example.com/react.js"},
				{Specifier: "scheduler", Version: "0.23.0", Type: "js",
					URL: "https://example.com/scheduler.js"},
			},
		},
		fetched: map[string][]byte{
			"https://example.com/react.js":     []byte("//react"),
			"https://example.com/scheduler.js": []byte("//scheduler"),
		},
	})

	args := append(projectFlags(tmp), "require", "react", "18.2.0")
	code, out, errOut := run(t, args...)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errOut)
	}
	for _, want := range []string{"+ react@18.2.0", "+ scheduler@0.23.0"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	got, err := assetmapper.LoadImportmap(filepath.Join(tmp, "importmap.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Entries["react"].Version != "18.2.0" {
		t.Errorf("react version = %q", got.Entries["react"].Version)
	}
}

// --- remove ---

func TestRemove_DropsEntryAndFile(t *testing.T) {
	tmp := t.TempDir()
	withStub(t, &stubResolver{
		resolution: &assetmapper.Resolution{
			Packages: []assetmapper.ResolvedPackage{
				{Specifier: "react", Version: "18.2.0", Type: "js",
					URL: "https://example.com/react.js"},
			},
		},
		fetched: map[string][]byte{"https://example.com/react.js": []byte("//react")},
	})

	if code, _, errOut := run(t, append(projectFlags(tmp), "require", "react", "18.2.0")...); code != 0 {
		t.Fatalf("seed: %s", errOut)
	}
	code, out, errOut := run(t, append(projectFlags(tmp), "remove", "react")...)
	if code != 0 {
		t.Fatalf("remove exit = %d, stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "- react") {
		t.Errorf("stdout = %q, want removal marker", out)
	}
	if _, err := os.Stat(filepath.Join(tmp, "assets", "vendor", "react.js")); !os.IsNotExist(err) {
		t.Errorf("vendor file still present: %v", err)
	}
	got, _ := assetmapper.LoadImportmap(filepath.Join(tmp, "importmap.json"))
	if _, ok := got.Entries["react"]; ok {
		t.Error("react still in importmap after remove")
	}
}

// --- update ---

func TestUpdate_BumpsVersion(t *testing.T) {
	tmp := t.TempDir()
	withStub(t, &stubResolver{
		resolution: &assetmapper.Resolution{
			Packages: []assetmapper.ResolvedPackage{
				{Specifier: "react", Version: "18.0.0", Type: "js",
					URL: "https://example.com/react-18.0.js"},
			},
		},
		fetched: map[string][]byte{"https://example.com/react-18.0.js": []byte("//v1")},
	})
	if code, _, errOut := run(t, append(projectFlags(tmp), "require", "react", "18.0.0")...); code != 0 {
		t.Fatalf("seed: %s", errOut)
	}

	withStub(t, &stubResolver{
		resolution: &assetmapper.Resolution{
			Packages: []assetmapper.ResolvedPackage{
				{Specifier: "react", Version: "18.2.0", Type: "js",
					URL: "https://example.com/react-18.2.js"},
			},
		},
		fetched: map[string][]byte{"https://example.com/react-18.2.js": []byte("//v2")},
	})

	code, out, errOut := run(t, append(projectFlags(tmp), "update", "react")...)
	if code != 0 {
		t.Fatalf("update exit = %d, stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "react: 18.0.0 -> 18.2.0") {
		t.Errorf("stdout = %q, want bump message", out)
	}
}

func TestUpdate_NoArgsRequiresAllFlag(t *testing.T) {
	// Reflexively running `assetmapper update` without args used to
	// silently update everything. Now it errors unless --all is given.
	tmp := t.TempDir()
	withStub(t, &stubResolver{}) // never called

	code, _, errOut := run(t, append(projectFlags(tmp), "update")...)
	if code == 0 {
		t.Fatal("expected non-zero exit for no args + no --all")
	}
	if !strings.Contains(errOut, "--all") {
		t.Errorf("stderr should mention --all; got %q", errOut)
	}
}

func TestUpdate_AllFlagUpdatesEveryVendoredEntry(t *testing.T) {
	tmp := t.TempDir()
	withStub(t, &stubResolver{
		resolution: &assetmapper.Resolution{
			Packages: []assetmapper.ResolvedPackage{
				{Specifier: "react", Version: "18.0.0", Type: "js",
					URL: "https://example.com/react.js"},
				{Specifier: "lodash", Version: "4.17.0", Type: "js",
					URL: "https://example.com/lodash.js"},
			},
		},
		fetched: map[string][]byte{
			"https://example.com/react.js":  []byte("//react"),
			"https://example.com/lodash.js": []byte("//lodash"),
		},
	})
	if code, _, errOut := run(t, append(projectFlags(tmp), "require", "react")...); code != 0 {
		t.Fatalf("seed: %s", errOut)
	}

	withStub(t, &stubResolver{
		resolution: &assetmapper.Resolution{
			Packages: []assetmapper.ResolvedPackage{
				{Specifier: "react", Version: "18.2.0", Type: "js",
					URL: "https://example.com/react.js"},
				{Specifier: "lodash", Version: "4.17.21", Type: "js",
					URL: "https://example.com/lodash.js"},
			},
		},
		fetched: map[string][]byte{
			"https://example.com/react.js":  []byte("//react"),
			"https://example.com/lodash.js": []byte("//lodash"),
		},
	})

	code, out, errOut := run(t, append(projectFlags(tmp), "update", "--all")...)
	if code != 0 {
		t.Fatalf("update --all exit=%d stderr=%s", code, errOut)
	}
	for _, want := range []string{"react:", "lodash:"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// --- outdated ---

func TestOutdated_ListsNewerVersions(t *testing.T) {
	tmp := t.TempDir()
	withStub(t, &stubResolver{
		resolution: &assetmapper.Resolution{
			Packages: []assetmapper.ResolvedPackage{
				{Specifier: "react", Version: "18.0.0", Type: "js",
					URL: "https://example.com/react.js"},
			},
		},
		fetched: map[string][]byte{"https://example.com/react.js": []byte("x")},
	})
	run(t, append(projectFlags(tmp), "require", "react", "18.0.0")...)

	withStub(t, &stubResolver{
		resolution: &assetmapper.Resolution{
			Packages: []assetmapper.ResolvedPackage{
				{Specifier: "react", Version: "18.2.0", Type: "js",
					URL: "https://example.com/react.js"},
			},
		},
	})
	code, out, errOut := run(t, append(projectFlags(tmp), "outdated")...)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "react: 18.0.0 -> 18.2.0") {
		t.Errorf("stdout = %q, want outdated line", out)
	}
}

func TestOutdated_AllUpToDate(t *testing.T) {
	tmp := t.TempDir()
	withStub(t, &stubResolver{
		resolution: &assetmapper.Resolution{
			Packages: []assetmapper.ResolvedPackage{
				{Specifier: "react", Version: "18.2.0", Type: "js",
					URL: "https://example.com/react.js"},
			},
		},
		fetched: map[string][]byte{"https://example.com/react.js": []byte("x")},
	})
	run(t, append(projectFlags(tmp), "require", "react", "18.2.0")...)

	code, out, _ := run(t, append(projectFlags(tmp), "outdated")...)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(out, "up to date") {
		t.Errorf("stdout = %q, want all-up-to-date message", out)
	}
}

func TestOutdated_CheckFlagExitsNonZero(t *testing.T) {
	// CI use: --check makes outdated packages a hard failure so the
	// pipeline can block.
	tmp := t.TempDir()
	withStub(t, &stubResolver{
		resolution: &assetmapper.Resolution{
			Packages: []assetmapper.ResolvedPackage{
				{Specifier: "react", Version: "18.0.0", Type: "js",
					URL: "https://example.com/react.js"},
			},
		},
		fetched: map[string][]byte{"https://example.com/react.js": []byte("x")},
	})
	run(t, append(projectFlags(tmp), "require", "react", "18.0.0")...)

	withStub(t, &stubResolver{
		resolution: &assetmapper.Resolution{
			Packages: []assetmapper.ResolvedPackage{
				{Specifier: "react", Version: "18.2.0", Type: "js",
					URL: "https://example.com/react.js"},
			},
		},
	})
	code, out, errOut := run(t, append(projectFlags(tmp), "outdated", "--check")...)
	if code == 0 {
		t.Fatalf("expected non-zero exit; stdout=%q stderr=%q", out, errOut)
	}
	if !strings.Contains(out, "react: 18.0.0 -> 18.2.0") {
		t.Errorf("outdated list missing from stdout:\n%s", out)
	}
}

func TestOutdated_CheckFlagSucceedsWhenUpToDate(t *testing.T) {
	tmp := t.TempDir()
	withStub(t, &stubResolver{
		resolution: &assetmapper.Resolution{
			Packages: []assetmapper.ResolvedPackage{
				{Specifier: "react", Version: "18.2.0", Type: "js",
					URL: "https://example.com/react.js"},
			},
		},
		fetched: map[string][]byte{"https://example.com/react.js": []byte("x")},
	})
	run(t, append(projectFlags(tmp), "require", "react", "18.2.0")...)

	code, _, errOut := run(t, append(projectFlags(tmp), "outdated", "--check")...)
	if code != 0 {
		t.Fatalf("--check with up-to-date should exit 0; got %d stderr=%s", code, errOut)
	}
}

// --- ErrOutdated sentinel ---

func TestErrOutdated_IsExported(t *testing.T) {
	t.Parallel()
	if ErrOutdated == nil {
		t.Fatal("ErrOutdated is nil")
	}
	wrapped := errors.New("wrapper: " + ErrOutdated.Error())
	if errors.Is(wrapped, ErrOutdated) {
		t.Error("wrapped error matches ErrOutdated by sentinel match; harmless but unexpected")
	}
	if !errors.Is(ErrOutdated, ErrOutdated) {
		t.Error("errors.Is(ErrOutdated, ErrOutdated) should be true")
	}
}

// --- prune ---

func TestPrune_RemovesOrphanedFiles(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	vendorDir := filepath.Join(tmp, "assets", "vendor")
	if err := os.MkdirAll(vendorDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"react.js", "stray.js"} {
		if err := os.WriteFile(filepath.Join(vendorDir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// importmap has only react.
	im := assetmapper.NewImportmap()
	im.Entries["react"] = assetmapper.ImportmapEntry{Version: "18.2.0"}
	if err := im.Save(filepath.Join(tmp, "importmap.json")); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := run(t, append(projectFlags(tmp), "prune")...)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "- stray.js") {
		t.Errorf("stdout = %q, want removal line for stray.js", out)
	}
	if strings.Contains(out, "react.js") {
		t.Errorf("registered file appeared in prune output: %s", out)
	}
	if _, err := os.Stat(filepath.Join(vendorDir, "stray.js")); !os.IsNotExist(err) {
		t.Errorf("stray.js still present: %v", err)
	}
}

func TestPrune_NothingToPrune(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	code, out, errOut := run(t, append(projectFlags(tmp), "prune")...)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "nothing to prune") {
		t.Errorf("stdout = %q, want nothing-to-prune message", out)
	}
}

// --- validate ---

func TestValidate_HappyPath(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "assets", "app.js"),
		[]byte("export default {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}
	if err := im.Save(filepath.Join(tmp, "importmap.json")); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := run(t, append(projectFlags(tmp), "validate")...)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%s, stdout=%s", code, errOut, out)
	}
	// No manifest in this setup → "(no manifest)" disclaimer, not
	// "manifest verified".
	if !strings.Contains(out, "(no manifest)") {
		t.Errorf("stdout = %q, want '(no manifest)' disclaimer when no compile has happened", out)
	}
	if strings.Contains(out, "manifest verified") {
		t.Errorf("stdout falsely claims 'manifest verified' when no manifest exists: %s", out)
	}
}

func TestValidate_HappyPathWithManifest(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "assets", "app.js"),
		[]byte("export default {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}
	if err := im.Save(filepath.Join(tmp, "importmap.json")); err != nil {
		t.Fatal(err)
	}

	// Compile so a real manifest + hashed file exist on disk.
	if code, _, errOut := run(t, append(projectFlags(tmp), "compile")...); code != 0 {
		t.Fatalf("compile setup: %s", errOut)
	}

	code, out, errOut := run(t, append(projectFlags(tmp), "validate")...)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "1 manifest entries verified") {
		t.Errorf("stdout = %q, want '1 manifest entries verified'", out)
	}
}

func TestValidate_NoImportmapJustReportsZero(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	code, out, _ := run(t, append(projectFlags(tmp), "validate")...)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(out, "0 importmap entries") {
		t.Errorf("stdout = %q, want '0 importmap entries'", out)
	}
}

func TestValidate_DetectsMissingLocalFile(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	// importmap references app.js but it doesn't exist on disk.
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}
	if err := im.Save(filepath.Join(tmp, "importmap.json")); err != nil {
		t.Fatal(err)
	}

	code, out, _ := run(t, append(projectFlags(tmp), "validate")...)
	if code == 0 {
		t.Fatal("expected non-zero exit for missing local file")
	}
	if !strings.Contains(out, `importmap "app"`) {
		t.Errorf("stdout missing 'importmap \"app\"': %s", out)
	}
}

func TestValidate_DetectsMissingVendorFile(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	// importmap claims react is vendored but file isn't there.
	im := assetmapper.NewImportmap()
	im.Entries["react"] = assetmapper.ImportmapEntry{Version: "18.2.0"}
	if err := im.Save(filepath.Join(tmp, "importmap.json")); err != nil {
		t.Fatal(err)
	}

	code, out, _ := run(t, append(projectFlags(tmp), "validate")...)
	if code == 0 {
		t.Fatal("expected non-zero exit for missing vendor file")
	}
	if !strings.Contains(out, `importmap "react"`) {
		t.Errorf("stdout missing 'importmap \"react\"': %s", out)
	}
}

func TestValidate_DetectsMalformedEntry(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	im := assetmapper.NewImportmap()
	im.Entries["bad"] = assetmapper.ImportmapEntry{Path: "app.js", Version: "1.0.0"}
	if err := im.Save(filepath.Join(tmp, "importmap.json")); err != nil {
		t.Fatal(err)
	}

	code, out, _ := run(t, append(projectFlags(tmp), "validate")...)
	if code == 0 {
		t.Fatal("expected non-zero exit for malformed entry")
	}
	if !strings.Contains(out, "both path and version") {
		t.Errorf("stdout missing diagnostic: %s", out)
	}
}

func TestValidate_DetectsMissingCompiledFile(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "assets", "app.js"),
		[]byte("export default {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}
	_ = im.Save(filepath.Join(tmp, "importmap.json"))

	// Manually craft a manifest pointing at a non-existent compiled
	// file. Use the new schema so LoadManifest accepts it.
	publicDir := filepath.Join(tmp, "public", "assets")
	if err := os.MkdirAll(publicDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bogus := &assetmapper.Manifest{
		URLPrefix: "/assets/",
		Entries:   map[string]string{"app.js": "app-deadbeef.js"},
	}
	if err := bogus.Save(publicDir); err != nil {
		t.Fatal(err)
	}

	code, out, _ := run(t, append(projectFlags(tmp), "validate")...)
	if code == 0 {
		t.Fatal("expected non-zero exit for missing compiled file")
	}
	if !strings.Contains(out, "manifest") {
		t.Errorf("stdout missing manifest diagnostic: %s", out)
	}
	if !strings.Contains(out, "app-deadbeef.js") {
		t.Errorf("stdout missing hashed name reference: %s", out)
	}
}

func TestValidate_AggregatesAllIssues(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	im := assetmapper.NewImportmap()
	im.Entries["missing"] = assetmapper.ImportmapEntry{Path: "nope.js"}
	im.Entries["bad"] = assetmapper.ImportmapEntry{Path: "app.js", Version: "1.0.0"}
	im.Entries["empty"] = assetmapper.ImportmapEntry{}
	if err := im.Save(filepath.Join(tmp, "importmap.json")); err != nil {
		t.Fatal(err)
	}

	code, out, _ := run(t, append(projectFlags(tmp), "validate")...)
	if code == 0 {
		t.Fatal("expected non-zero exit")
	}
	// All three issues should appear in the output.
	for _, want := range []string{`"missing"`, `"bad"`, `"empty"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
