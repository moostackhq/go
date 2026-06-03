package assetmapper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultJSPMBaseURL is the official jspm.io generator endpoint.
// Override [JSPMResolver.BaseURL] to point at a mirror.
const DefaultJSPMBaseURL = "https://api.jspm.io"

// JSPMResolver implements [PackageResolver] against jspm.io's
// /generate endpoint. The endpoint accepts a list of npm-style
// install specifiers and returns a browser importmap covering the
// requested packages plus their transitive dependencies.
//
// Construct with [NewJSPMResolver]; the zero value is unusable.
type JSPMResolver struct {
	// Client is the HTTP client used for both Resolve (POST to
	// /generate) and Fetch (GET upstream package URLs). Callers can
	// set a timeout, custom transport, or proxy here.
	Client *http.Client
	// BaseURL is the jspm.io API root. Empty means
	// [DefaultJSPMBaseURL].
	BaseURL string
}

// NewJSPMResolver returns a resolver wired to jspm.io. Pass nil for
// client to get an [http.Client] with a 30s timeout per call.
func NewJSPMResolver(client *http.Client) *JSPMResolver {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &JSPMResolver{
		Client:  client,
		BaseURL: DefaultJSPMBaseURL,
	}
}

// Resolve POSTs to /generate with the requested packages and
// flattens the returned imports + scopes into a single
// [Resolution]. Bare specifiers appearing in scopes (transitive
// deps) are surfaced as top-level packages so they can be downloaded
// alongside the directly requested ones.
//
// A limitation of the flat output: two packages depending on
// different versions of a third lose the scoping that would
// disambiguate them in the browser. In practice this is rare for
// browser-facing JS; document and accept.
func (j *JSPMResolver) Resolve(ctx context.Context, reqs []PackageRequest) (*Resolution, error) {
	if len(reqs) == 0 {
		return &Resolution{}, nil
	}
	install := make([]string, len(reqs))
	for i, r := range reqs {
		if r.Version != "" {
			install[i] = r.Name + "@" + r.Version
		} else {
			install[i] = r.Name
		}
	}

	payload, err := json.Marshal(map[string]any{
		"install":  install,
		"env":      []string{"browser", "production"},
		"provider": "jspm.io",
	})
	if err != nil {
		return nil, err
	}

	base := j.BaseURL
	if base == "" {
		base = DefaultJSPMBaseURL
	}
	endpoint := strings.TrimSuffix(base, "/") + "/generate"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := j.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jspm.io: POST /generate: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("jspm.io: POST /generate: status %d: %s", resp.StatusCode, body)
	}

	var gen jspmGenerateResponse
	if err := json.NewDecoder(resp.Body).Decode(&gen); err != nil {
		return nil, fmt.Errorf("jspm.io: decode response: %w", err)
	}
	return jspmFlatten(&gen), nil
}

// Fetch downloads a single package file by URL with a context-bound
// HTTP GET. Non-2xx responses surface as errors so the caller can
// abort the entire vendoring step rather than ending up with a
// corrupt vendor/.
func (j *JSPMResolver) Fetch(ctx context.Context, raw string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	resp, err := j.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jspm.io: fetch %s: %w", raw, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jspm.io: fetch %s: status %d", raw, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// jspmGenerateResponse mirrors the JSON shape of /generate. Only the
// fields we need are decoded; jspm sometimes returns extra fields
// (staticDeps, dynamicDeps) which we ignore — preloading walks the
// source dep graph instead.
type jspmGenerateResponse struct {
	Map struct {
		Imports map[string]string            `json:"imports"`
		Scopes  map[string]map[string]string `json:"scopes"`
	} `json:"map"`
}

// jspmFlatten turns the nested imports+scopes shape into a flat
// resolution. A specifier appearing in multiple scopes is kept only
// once (first wins); see Resolve's doc for the trade-off.
func jspmFlatten(g *jspmGenerateResponse) *Resolution {
	seen := map[string]bool{}
	var pkgs []ResolvedPackage
	add := func(spec, u string) {
		if seen[spec] || spec == "" || u == "" {
			return
		}
		seen[spec] = true
		pkgs = append(pkgs, ResolvedPackage{
			Specifier: spec,
			Version:   versionFromJSPMURL(u),
			Type:      typeFromURL(u),
			URL:       u,
		})
	}
	for spec, u := range g.Map.Imports {
		add(spec, u)
	}
	for _, scope := range g.Map.Scopes {
		for spec, u := range scope {
			add(spec, u)
		}
	}
	return &Resolution{Packages: pkgs}
}

// versionFromJSPMURL pulls "18.2.0" out of a URL like
// "https://ga.jspm.io/npm:react@18.2.0/index.js". Returns "" for
// URLs that don't follow the npm:<name>@<version>/<file> shape.
//
// Scoped packages (e.g. "@radix-ui/themes") look like
// "https://ga.jspm.io/npm:@radix-ui/themes@1.0.0/index.js"; the
// scope contains a slash that is NOT a path separator, so we cannot
// split on the first slash. Instead, locate the version separator
// (the LAST "@" in the path) and read until the next slash.
func versionFromJSPMURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	p := strings.TrimPrefix(parsed.Path, "/")
	if i := strings.Index(p, ":"); i >= 0 {
		p = p[i+1:]
	}
	// p is now "<package>@<version>[/<file>]" with <package>
	// possibly containing "/" (scoped). The LAST "@" is the
	// version separator; an "@" at position 0 is a scoped package
	// with no version specified.
	at := strings.LastIndex(p, "@")
	if at <= 0 {
		return ""
	}
	rest := p[at+1:]
	if slash := strings.IndexByte(rest, '/'); slash >= 0 {
		rest = rest[:slash]
	}
	return rest
}

// typeFromURL classifies a vendored file by extension. JS is the
// default; only CSS is detected explicitly today.
func typeFromURL(u string) string {
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	if strings.HasSuffix(u, ".css") {
		return "css"
	}
	return "js"
}
