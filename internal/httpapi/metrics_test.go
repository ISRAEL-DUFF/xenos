package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/israel-duff/xenos/internal/metrics"
)

// A server with metrics counts requests by route pattern, refused attempts by limiter and bad webhooks.
func TestServerRecordsMetrics(t *testing.T) {
	env := newTestEnv(t)
	m := metrics.New()
	env.srv.Metrics = m
	ts := httptest.NewServer(env.srv.Router())
	defer ts.Close()
	c := newClient(t, ts.URL)

	for i := 0; i < 32; i++ { // the per-IP login limit is 30 per 15 minutes
		c.do("POST", "/v1/auth/login", map[string]any{"email": "nobody@x.co", "password": "wrong-password-1"}, nil)
	}
	c.do("GET", "/v1/vms/123", nil, nil)
	resp, err := http.Post(ts.URL+"/v1/webhooks/ispend", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	b, _ := io.ReadAll(rec.Body)
	out := string(b)
	for _, want := range []string{
		`xenos_rate_limited_total{limiter="login_ip"} 2`,
		`xenos_http_requests_total{class="4xx",method="GET",route="/v1/vms/{id}"} 1`,
		`xenos_webhooks_total{outcome="bad_signature"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(out, "/v1/vms/123") {
		t.Error("a raw path became a label")
	}
}

// Metrics are on their own loopback listener: the public router serves the dashboard for /metrics, never the numbers.
func TestMetricsAreNotOnThePublicRouter(t *testing.T) {
	env := newTestEnv(t)
	env.srv.Metrics = metrics.New()
	ts := httptest.NewServer(env.srv.Router())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(b), "xenos_http_requests_total") || strings.Contains(string(b), "go_goroutines") {
		t.Fatal("the metrics endpoint must not be reachable on the public port")
	}
}
