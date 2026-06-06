// Package middleware ships the building blocks every HTTP server
// needs:
//
//   - [RequestID] — per-request correlation IDs
//   - [Logger] — one structured slog record per request
//   - [Recover] — panic-to-500 with stack logging
//   - [Timeout] — context deadline per request
//   - [RealIP] — rewrite RemoteAddr from proxy headers
//   - [Compress] — gzip responses when the client accepts
//   - [CORS] — Cross-Origin Resource Sharing with preflight
//   - [StripSlashes] — normalise /users/ → /users before routing
//
// Each is exposed as a [router.Middleware] you drop into
// [github.com/moostackhq/go/router.Router.Use]:
//
//	r := router.New()
//	r.Use(
//	    middleware.RequestID(),
//	    middleware.Logger(),
//	    middleware.Recover(),
//	    middleware.Timeout(30*time.Second),
//	    middleware.CompressWith(middleware.CompressOptions{MinSize: 2048}),
//	)
//
// Loggers default to [slog.Default]; use the *With variants to
// supply a custom logger (e.g., per-environment levels). Most
// middleware take their options via a *With form or an Options
// struct — see each function's godoc for the full surface.
package middleware
