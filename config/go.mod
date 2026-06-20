module github.com/moostackhq/go/config

go 1.25.0

require (
	github.com/moostackhq/go/validation v0.0.0-00010101000000-000000000000
	gopkg.in/yaml.v3 v3.0.1
)

replace github.com/moostackhq/go/validation => ../validation
