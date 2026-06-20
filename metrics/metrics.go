// Package metrics provides Prometheus HTTP server metrics: a middleware
// that records request count, duration, and in-flight requests, plus an
// HTTP handler that exposes them (and the standard Go/process collectors)
// for scraping.
//
//	m := metrics.New(metrics.WithNamespace("demo"))
//	r.Use(m.Middleware())     // record every request
//	r.Handle("/metrics", m.Handler())
//
// The middleware is router-agnostic — it returns the de facto stdlib
// shape func(http.Handler) http.Handler.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds the HTTP collectors registered to a private registry.
// Build with [New].
type Metrics struct {
	reg      *prometheus.Registry
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	inflight prometheus.Gauge
	route    func(*http.Request) string
}

type config struct {
	namespace string
	buckets   []float64
	route     func(*http.Request) string
}

// Option configures [New].
type Option func(*config)

// WithNamespace prefixes every metric name with namespace + "_".
func WithNamespace(namespace string) Option {
	return func(c *config) { c.namespace = namespace }
}

// WithBuckets sets the duration histogram buckets (default
// [prometheus.DefBuckets]).
func WithBuckets(buckets []float64) Option {
	return func(c *config) { c.buckets = buckets }
}

// WithRouteFunc sets how the route label is derived from a request. The
// default reads Request.Pattern (the stdlib ServeMux match). Routers that
// expose the matched pattern elsewhere — e.g. via a context accessor —
// can supply it here so the label is the route template, not the raw
// path. Returning "" is fine (unmatched/untracked).
func WithRouteFunc(fn func(*http.Request) string) Option {
	return func(c *config) {
		if fn != nil {
			c.route = fn
		}
	}
}

// New builds the collectors and registers them, along with the standard
// Go runtime and process collectors, on a private registry.
func New(opts ...Option) *Metrics {
	cfg := config{
		buckets: prometheus.DefBuckets,
		route:   func(r *http.Request) string { return r.Pattern },
	}
	for _, o := range opts {
		o(&cfg)
	}

	labels := []string{"method", "route", "status"}
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: cfg.namespace, Subsystem: "http", Name: "requests_total",
			Help: "Total HTTP requests by method, route, and status.",
		}, labels),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: cfg.namespace, Subsystem: "http", Name: "request_duration_seconds",
			Help: "HTTP request latency in seconds.", Buckets: cfg.buckets,
		}, labels),
		inflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: cfg.namespace, Subsystem: "http", Name: "requests_in_flight",
			Help: "HTTP requests currently being served.",
		}),
		route: cfg.route,
	}
	m.reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.requests, m.duration, m.inflight,
	)
	return m
}

// Handler returns the /metrics scrape handler for this registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// Middleware records request count, duration, and in-flight count for
// every request it wraps. The route label comes from the route function
// (see [WithRouteFunc]; default Request.Pattern) — a bounded route
// template, never the raw path; empty for unmatched requests.
func (m *Metrics) Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			m.inflight.Inc()
			defer m.inflight.Dec()

			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sw, r)

			labels := prometheus.Labels{
				"method": r.Method,
				"route":  m.route(r), // bounded route template; "" when unmatched
				"status": strconv.Itoa(sw.status),
			}
			m.requests.With(labels).Inc()
			m.duration.With(labels).Observe(time.Since(start).Seconds())
		})
	}
}
