package metrics_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moostackhq/go/metrics"
)

func TestMiddleware_RecordsRequest(t *testing.T) {
	m := metrics.New(metrics.WithNamespace("test"))

	// Serve through a ServeMux so Request.Pattern (the route label) is set.
	mux := http.NewServeMux()
	mux.Handle("GET /hello/{name}", m.Middleware()(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
		})))

	req := httptest.NewRequest(http.MethodGet, "/hello/world", nil)
	mux.ServeHTTP(httptest.NewRecorder(), req)

	body := scrape(t, m)
	for _, want := range []string{
		`test_http_requests_total{`,
		`method="GET"`,
		`route="GET /hello/{name}"`, // bounded route pattern, not /hello/world
		`status="201"`,
		`test_http_request_duration_seconds_bucket`,
		`test_http_requests_in_flight`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape missing %q\n--- body ---\n%s", want, body)
		}
	}
	// Raw path must never become a label (cardinality).
	if strings.Contains(body, "/hello/world") {
		t.Errorf("raw path leaked into labels:\n%s", body)
	}
}

func TestHandler_IncludesRuntimeCollectors(t *testing.T) {
	m := metrics.New()
	body := scrape(t, m)
	if !strings.Contains(body, "go_goroutines") {
		t.Errorf("expected standard Go collector metrics, got:\n%s", body)
	}
}

func scrape(t *testing.T, m *metrics.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape returned %d", rec.Code)
	}
	return rec.Body.String()
}
