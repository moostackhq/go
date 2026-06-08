module github.com/moostackhq/go/auth

go 1.25.0

require github.com/moostackhq/go/router v0.0.0-00010101000000-000000000000

replace github.com/moostackhq/go/router => ../router

require github.com/moostackhq/go/session v0.0.0-00010101000000-000000000000

require golang.org/x/crypto v0.52.0 // indirect

replace github.com/moostackhq/go/session => ../session
