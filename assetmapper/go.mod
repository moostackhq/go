module github.com/moostackhq/go/assetmapper

go 1.26.1

require github.com/moostackhq/go/cli v0.0.0

require (
	golang.org/x/sys v0.44.0 // indirect
	golang.org/x/term v0.43.0 // indirect
)

// Local sibling — remove this replace when the cli module is published
// and a real version is pinned in the require block above.
replace github.com/moostackhq/go/cli => ../cli
