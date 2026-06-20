# metrics

Prometheus HTTP server metrics for Go: a middleware that records request count, duration, and in-flight requests, plus a `/metrics` handler.

## Usage

```go
m := metrics.New(metrics.WithNamespace("demo"))

r.Use(m.Middleware())            // instrument every request
r.Handle("/metrics", m.Handler())
```

The middleware is router-agnostic — it returns the de facto stdlib `func(http.Handler) http.Handler`, so it composes with any net/http router.

## Metrics

| Name | Type | Labels |
|---|---|---|
| `<ns>_http_requests_total` | counter | method, route, status |
| `<ns>_http_request_duration_seconds` | histogram | method, route, status |
| `<ns>_http_requests_in_flight` | gauge | — |

Plus the standard Go runtime and process collectors (`go_goroutines`, `process_cpu_seconds_total`, …).

**Cardinality:** the `route` label is the *matched route pattern* (`Request.Pattern`, Go 1.22+) — e.g. `GET /monitors/{id}/check`, never the raw `/monitors/42/check`. It's empty for unmatched requests. So label cardinality is bounded by your route table, not by traffic.

## Options

```go
metrics.New(
    metrics.WithNamespace("demo"),                 // metric name prefix
    metrics.WithBuckets([]float64{.01, .1, 1, 10}), // duration histogram buckets
)
```

## Registry

Each `Metrics` owns a private `prometheus.Registry` (not the global default), so multiple instances don't collide and tests stay isolated. `Handler()` scrapes that registry.

## Status

Reference code. One dependency: `github.com/prometheus/client_golang` — the de facto standard client.
