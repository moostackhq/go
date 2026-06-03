package assetmapper

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// CompileOptions controls [Compile]. The zero value is valid; defaults
// are filled in by Compile itself.
type CompileOptions struct {
	// URLPrefix is the absolute URL prefix used when rewriting
	// references in JS / CSS files. Defaults to "/assets/".
	//
	// The value is persisted in [Manifest.URLPrefix] and verified at
	// [New] time: a runtime [Config.URLPrefix] that disagrees causes
	// New to fail rather than silently half-loading pages whose
	// scripts request a different URL than the one served. Setting
	// the same value in both places (or relying on the same default
	// at both sites) is the supported pattern.
	URLPrefix string
}

// Compile walks every asset in srcRoots, rewrites internal references
// (JS import / export specifiers, CSS url() and @import) to point at
// content-hashed filenames, copies each rewritten asset into
// publicDir, and writes [ManifestFilename] alongside.
//
// Resolution semantics match [Mapper.Asset]: roots are walked in
// order; a logical path discovered in an earlier root shadows the
// same path in later roots.
//
// publicDir is created (recursively, 0o755) if missing. Stale
// .assetmapper-tmp-*.tmp files (orphans from a previous crashed
// compile) are removed best-effort BEFORE any new work starts, so a
// Compile that succeeds always leaves publicDir tidy with no temps.
// Existing compiled files are not removed; re-compiling unchanged
// sources produces idempotent overwrites.
//
// Reference rewriting only touches paths that resolve to a known
// asset in srcRoots. Bare specifiers (importmap names like "react"),
// external URLs (http://, data:), and paths that don't match any
// asset are left untouched. Cycles in the asset dep graph surface
// as a [*CycleError].
//
// Compile is NOT safe for concurrent invocation against the same
// publicDir. Two concurrent calls would race on the temp-file
// rename step and on the manifest.json write; the disk would
// converge to a valid state (same content, same hashes) but the
// races aren't worth defending. Serialise externally if you somehow
// need it.
func Compile(srcRoots []Root, publicDir string, opts ...CompileOptions) (*Manifest, error) {
	if len(srcRoots) == 0 {
		return nil, fmt.Errorf("assetmapper.Compile: at least one Root is required")
	}
	for i, r := range srcRoots {
		if r.FS == nil {
			return nil, fmt.Errorf("assetmapper.Compile: Roots[%d].FS is nil", i)
		}
		if err := validateMount(r.MountAt); err != nil {
			return nil, fmt.Errorf("assetmapper.Compile: Roots[%d].MountAt: %w", i, err)
		}
	}
	var o CompileOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	normalizedPrefix, err := normalizeURLPrefix(o.URLPrefix)
	if err != nil {
		return nil, fmt.Errorf("assetmapper.Compile: %w", err)
	}
	o.URLPrefix = normalizedPrefix

	if err := os.MkdirAll(publicDir, 0o755); err != nil {
		return nil, fmt.Errorf("assetmapper.Compile: create publicDir: %w", err)
	}

	// GC stale temp files left behind by a crashed previous run.
	// The prefix .assetmapper-tmp- is distinctive enough that
	// matching files in publicDir are unambiguously ours; we do
	// NOT match looser patterns like .assetmapper-*.tmp that could
	// snag a hand-placed file. Best-effort: remove failures (perms,
	// races) are ignored — they don't affect this run's correctness,
	// just the disk-usage win.
	if stale, _ := filepath.Glob(filepath.Join(publicDir, ".assetmapper-tmp-*.tmp")); len(stale) > 0 {
		for _, p := range stale {
			_ = os.Remove(p)
		}
	}

	// Pass 1: walk roots once, gathering every asset's metadata.
	// JS/CSS contents are read into memory (they need to be rewritten
	// later); every other asset stays on disk and is streamed during
	// pass 2 so an asset tree heavy with images/fonts/videos doesn't
	// peak memory at total-tree size.
	assets, err := collectAssets(srcRoots)
	if err != nil {
		return nil, err
	}

	hashedNames := make(map[string]string, len(assets))
	outputOwner := make(map[string]string, len(assets))

	// Pass 2: stream-hash + write every non-JS/CSS asset. These
	// don't have refs to rewrite, so their hash is purely content-
	// based and can be computed independently of any dep graph.
	// Sorted iteration so collision errors mention assets in
	// deterministic order across runs.
	var streamables []string
	for logical, a := range assets {
		if a.kind != kindJS && a.kind != kindCSS {
			streamables = append(streamables, logical)
		}
	}
	sort.Strings(streamables)
	for _, logical := range streamables {
		a := assets[logical]
		hash, tmpPath, err := streamHashWrite(a.root.FS, a.subPath, publicDir)
		if err != nil {
			return nil, fmt.Errorf("assetmapper.Compile: stream %s: %w", logical, err)
		}
		hashed := hashedName(logical, hash)
		if cerr := checkCollision(logical, hashed, assets, outputOwner); cerr != nil {
			_ = os.Remove(tmpPath)
			return nil, cerr
		}
		dst := filepath.Join(publicDir, filepath.FromSlash(hashed))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			_ = os.Remove(tmpPath)
			return nil, fmt.Errorf("assetmapper.Compile: mkdir for %s: %w", logical, err)
		}
		if err := os.Rename(tmpPath, dst); err != nil {
			_ = os.Remove(tmpPath)
			return nil, fmt.Errorf("assetmapper.Compile: rename %s: %w", dst, err)
		}
		outputOwner[hashed] = logical
		hashedNames[logical] = hashed
	}

	// Pass 3: extract refs from JS/CSS, build dep graph among JS/CSS
	// nodes only. Non-JS/CSS targets (images, fonts) are dropped
	// from the graph because pass 2 has already hashed them — their
	// hashes will be looked up directly from hashedNames during the
	// pass-5 rewrite. Including them as graph nodes would leave the
	// topo sort with unsatisfiable indegrees and flag spurious cycles.
	deps := make(map[string][]string)
	refsByAsset := make(map[string][]ref)
	for logical, a := range assets {
		if a.kind != kindJS && a.kind != kindCSS {
			continue
		}
		deps[logical] = nil
		refs := extractRefs(logical, a.content, a.kind)
		refsByAsset[logical] = refs
		for _, r := range refs {
			if r.resolved == "" {
				continue
			}
			target, ok := assets[r.resolved]
			if !ok {
				continue
			}
			if target.kind != kindJS && target.kind != kindCSS {
				continue
			}
			deps[logical] = append(deps[logical], r.resolved)
		}
	}

	// Pass 4: topo sort so each JS/CSS asset's deps are hashed
	// before the asset itself. Non-JS/CSS deps are already hashed
	// from pass 2.
	order, err := topoSort(deps)
	if err != nil {
		return nil, err
	}

	// Pass 5: rewrite refs, hash the rewritten content, write the
	// JS/CSS asset. Same collision checks against the unified
	// outputOwner map (which already contains pass 2's entries).
	for _, logical := range order {
		a := assets[logical]
		rewritten := a.content
		if refs := refsByAsset[logical]; len(refs) > 0 {
			rewritten = rewriteRefs(a.content, refs, func(r ref) string {
				target, ok := hashedNames[r.resolved]
				if !ok {
					return r.spec
				}
				return o.URLPrefix + target
			})
		}
		hash := hashContent(rewritten)
		hashed := hashedName(logical, hash)
		if cerr := checkCollision(logical, hashed, assets, outputOwner); cerr != nil {
			return nil, cerr
		}
		outputOwner[hashed] = logical
		hashedNames[logical] = hashed

		dst := filepath.Join(publicDir, filepath.FromSlash(hashed))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil, fmt.Errorf("assetmapper.Compile: mkdir for %s: %w", logical, err)
		}
		if err := os.WriteFile(dst, rewritten, 0o644); err != nil {
			return nil, fmt.Errorf("assetmapper.Compile: write %s: %w", dst, err)
		}
	}

	manifest := NewManifest()
	manifest.URLPrefix = o.URLPrefix
	for logical, hashed := range hashedNames {
		manifest.Entries[logical] = hashed
	}
	if err := manifest.Save(publicDir); err != nil {
		return nil, err
	}
	return manifest, nil
}

// checkCollision applies the two pre-write defences against silent
// last-writer-wins overwrites in publicDir.
//
//  1. Two distinct logical paths producing the same compiled
//     filename (8-char SHA-256 collision or adversarial naming).
//  2. The compiled filename equals the literal source path of
//     another asset (e.g. foo.js hashes to 12345678 and a source
//     file foo-12345678.js also exists). Indistinguishable at the
//     URL level and confusing in publicDir.
func checkCollision(logical, hashed string, assets map[string]*collectedAsset, outputOwner map[string]string) error {
	if other, dup := outputOwner[hashed]; dup {
		return fmt.Errorf("assetmapper.Compile: %q and %q both compile to %q (hash collision or naming overlap); rename one of them",
			other, logical, hashed)
	}
	if _, shadowed := assets[hashed]; shadowed {
		return fmt.Errorf("assetmapper.Compile: %q compiles to %q, which is also a literal source path; rename one of them",
			logical, hashed)
	}
	return nil
}

// collectedAsset is the in-memory shape of an asset during compile.
// JS / CSS files have their full content read into memory because
// rewriting needs random access; other types leave content on disk
// (root + subPath are the addressable reference) and get streamed by
// [streamHashWrite] during pass 2.
type collectedAsset struct {
	root    Root
	subPath string
	kind    assetKind
	content []byte // nil for non-JS/CSS (streamed instead)
}

func collectAssets(roots []Root) (map[string]*collectedAsset, error) {
	assets := make(map[string]*collectedAsset)
	for _, root := range roots {
		err := fs.WalkDir(root.FS, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			logical := p
			if root.MountAt != "" {
				logical = root.MountAt + "/" + p
			}
			if _, dup := assets[logical]; dup {
				return nil
			}
			kind := kindOf(logical)
			ca := &collectedAsset{root: root, subPath: p, kind: kind}
			if kind == kindJS || kind == kindCSS {
				content, err := fs.ReadFile(root.FS, p)
				if err != nil {
					return fmt.Errorf("read %s: %w", p, err)
				}
				ca.content = content
			}
			assets[logical] = ca
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("assetmapper.Compile: walk: %w", err)
		}
	}
	return assets, nil
}

// streamHashWrite copies srcFS[srcPath] into a temp file under
// publicDir while computing the SHA-256 of the bytes, then returns
// the truncated hash and the temp file's on-disk path. The caller
// is responsible for either renaming the temp into its final
// hashed-name destination (after collision checks) or removing it
// on any abort path.
//
// The temp file lives in publicDir so the subsequent os.Rename is
// same-filesystem (and therefore atomic on POSIX). Subdirectories
// inside publicDir for the final destination are created by the
// caller when needed.
func streamHashWrite(srcFS fs.FS, srcPath, publicDir string) (hash, tmpPath string, err error) {
	tmp, err := os.CreateTemp(publicDir, ".assetmapper-tmp-*.tmp")
	if err != nil {
		return "", "", err
	}
	tmpName := tmp.Name()

	src, err := srcFS.Open(srcPath)
	if err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", "", err
	}
	defer src.Close()

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), src); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", "", err
	}
	return hex.EncodeToString(h.Sum(nil))[:HashLength], tmpName, nil
}

