package metrics

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/israel-duff/xenos/internal/billing"
	"github.com/israel-duff/xenos/internal/testutil"
)

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("scrape = %d", rec.Code)
	}
	return rec.Body.String()
}

func TestRoutesAreLabelledByPatternNotPath(t *testing.T) {
	m := New()
	r := chi.NewRouter()
	r.Use(m.HTTP)
	r.Get("/v1/vms/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	r.Post("/v1/auth/login", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(401) })
	for _, p := range []string{"/v1/vms/1", "/v1/vms/987654", "/nothing/here/12345"} {
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", p, nil))
	}
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/auth/login", nil))

	out := scrape(t, m)
	for _, want := range []string{
		`xenos_http_requests_total{class="2xx",method="GET",route="/v1/vms/{id}"} 2`,
		`xenos_http_requests_total{class="4xx",method="GET",route="unmatched"} 1`,
		`xenos_http_requests_total{class="4xx",method="POST",route="/v1/auth/login"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in:\n%s", want, grep(out, "xenos_http_requests_total"))
		}
	}
	for _, leaked := range []string{"987654", "12345"} {
		if strings.Contains(out, leaked) {
			t.Errorf("a raw path (%s) leaked into a label: unbounded cardinality", leaked)
		}
	}
}

func grep(s, sub string) string {
	var b strings.Builder
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			b.WriteString(l + "\n")
		}
	}
	return b.String()
}

func TestRecordingHelpers(t *testing.T) {
	m := New()
	m.RateLimited("login_ip")
	m.Webhook("bad_signature")
	m.ConsoleOpened()
	m.ConsoleOpened()
	m.ConsoleClosed()
	m.JobDone("vm.provision", "ok", 2*time.Second)
	m.JobDone("vm.provision", "error", time.Second)
	m.MeterTick(time.Second, true, false)
	m.HostCapacity("default", 0.4, 0.7)
	out := scrape(t, m)
	for _, want := range []string{
		`xenos_rate_limited_total{limiter="login_ip"} 1`, `xenos_webhooks_total{outcome="bad_signature"} 1`, `xenos_consoles_open 1`,
		`xenos_jobs_total{kind="vm.provision",result="ok"} 1`, `xenos_jobs_total{kind="vm.provision",result="error"} 1`,
		`xenos_host_pool_used_fraction{host="default"} 0.4`, `xenos_host_ram_committed_fraction{host="default"} 0.7`, `xenos_meter_ispend_down 0`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if !strings.Contains(out, "xenos_meter_last_success_timestamp_seconds 1.") {
		t.Error("a successful pass records its time")
	}
	// A pass that fails does not move the success time, and a down wallet service is flagged.
	m.MeterTick(time.Second, false, true)
	if !strings.Contains(scrape(t, m), "xenos_meter_ispend_down 1") {
		t.Error("ispend down flag")
	}
}

func TestNilMetricsIsHarmless(t *testing.T) {
	var m *Metrics
	m.RateLimited("x")
	m.Webhook("x")
	m.ConsoleOpened()
	m.ConsoleClosed()
	m.JobDone("k", "ok", time.Second)
	m.MeterTick(time.Second, true, false)
	m.HostCapacity("h", 0, 0)
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	if rec := httptest.NewRecorder(); true {
		m.HTTP(h).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
		if rec.Code != 200 {
			t.Fatal("a nil Metrics must pass requests through")
		}
	}
	f := billing.NewFake(150_000)
	if WrapISpend(f, nil) != billing.ISpend(f) {
		t.Fatal("wrapping with nil metrics must return the original")
	}
	if err := Serve(context.Background(), "off", New(), slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
}

func TestISpendCallsAreCountedByResult(t *testing.T) {
	m := New()
	f := billing.NewFake(150_000)
	w := WrapISpend(f, m)
	ctx := context.Background()
	c, err := w.CreateCustomer(ctx, "k", "user:1", "a@x.co", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Charge(ctx, "c1", c.ID, 6000, "n"); !errors.Is(err, billing.ErrInsufficientFunds) {
		t.Fatalf("an empty wallet: %v", err)
	}
	f.Credit(c.ID, 100_000)
	if _, err := w.Charge(ctx, "c2", c.ID, 6000, "n"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Balances(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	out := scrape(t, m)
	for _, want := range []string{
		`xenos_ispend_calls_total{method="charge",result="insufficient_funds"} 1`,
		`xenos_ispend_calls_total{method="charge",result="ok"} 1`,
		`xenos_ispend_calls_total{method="balances",result="ok"} 1`,
		`xenos_ispend_calls_total{method="create_customer",result="ok"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in:\n%s", want, grep(out, "xenos_ispend_calls_total"))
		}
	}
}

func TestDatabaseGauges(t *testing.T) {
	st := testutil.DB(t)
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO users (id, email, password_hash) VALUES (1, 'a@x.co', 'x')`,
		`INSERT INTO vms (user_id, region, plan_id, template_id, proxmox_vmid, hostname, state)
		 SELECT 1, 'r', p.id, t.id, 100 + g, 'h' || g, CASE WHEN g = 3 THEN 'stopped' ELSE 'running' END
		 FROM plans p, templates t, generate_series(1,3) g WHERE p.slug='nano' AND t.slug='ubuntu-24.04'`,
		`INSERT INTO usage_charges (user_id, vm_id, hour, amount_uusdt, status)
		 SELECT 1, v.id, now() - interval '3 hours', 6000, 'pending' FROM vms v WHERE v.hostname = 'h1'`,
		`INSERT INTO usage_charges (user_id, vm_id, hour, amount_uusdt, status)
		 SELECT 1, v.id, now() - interval '2 hours', 7000, 'unpaid' FROM vms v WHERE v.hostname = 'h2'`,
		`INSERT INTO ip_addresses (address, gateway, region) VALUES ('10.0.0.1', '10.0.0.254', 'r'), ('10.0.0.2', '10.0.0.254', 'r')`,
		`INSERT INTO jobs (kind, payload, status) VALUES ('vm.provision', '{}', 'queued'), ('vm.provision', '{}', 'failed')`,
	} {
		if _, err := st.Pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	m := New()
	m.RegisterDB(st)
	out := scrape(t, m)
	for _, want := range []string{
		`xenos_vms{state="running"} 2`, `xenos_vms{state="stopped"} 1`,
		`xenos_usage_charges_open{status="pending"} 1`, `xenos_usage_charges_open_uusdt{status="unpaid"} 7000`,
		`xenos_unpaid_total_uusdt 7000`, `xenos_ipv4_free 2`, `xenos_ipv4_total 2`,
		`xenos_jobs_pending{kind="vm.provision",status="failed"} 1`, `xenos_jobs_pending{kind="vm.provision",status="queued"} 1`,
		`xenos_worker_heartbeat_age_seconds -1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, grep(out, "xenos_"))
		}
	}
	// The oldest pending charge is about three hours old.
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "xenos_oldest_pending_charge_age_seconds ") {
			var v float64
			if _, err := fmtSscan(l, &v); err != nil || v < 3*3600-60 || v > 3*3600+120 {
				t.Errorf("oldest pending age: %q", l)
			}
		}
	}
	// Cached: a change inside the TTL is not visible yet.
	if _, err := st.Pool.Exec(ctx, `UPDATE vms SET state='deleted', deleted_at=now()`); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(scrape(t, m), `xenos_vms{state="running"} 2`) {
		t.Error("the collector should answer from its cache within the TTL")
	}
}

func fmtSscan(line string, v *float64) (int, error) {
	f := strings.Fields(line)
	if len(f) != 2 {
		return 0, errors.New("unexpected line")
	}
	return fmtSscanf(f[1], v)
}

// Every Xenos metric an alert rule mentions must exist, so a typo or a rename cannot leave a rule silently dead.
func TestAlertRulesOnlyUseMetricsThatExist(t *testing.T) {
	st := testutil.DB(t)
	m := New()
	m.RegisterDB(st)
	// Touch every labelled vector so it shows up in a scrape.
	m.RateLimited("x")
	m.Webhook("accepted")
	m.ConsoleOpened()
	m.JobDone("k", "ok", time.Second)
	m.MeterTick(time.Second, true, false)
	m.HostCapacity("default", 0, 0)
	m.HTTP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	w := WrapISpend(billing.NewFake(1), m)
	_, _ = w.Rate(context.Background())
	out := scrape(t, m)
	declared := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "# TYPE ") {
			declared[strings.Fields(l)[2]] = true
		}
	}
	// Series that only exist once the database holds matching rows are described by the collector.
	for _, n := range []string{"xenos_vms", "xenos_jobs_pending", "xenos_usage_charges_open", "xenos_conversions_open",
		"xenos_oldest_pending_charge_age_seconds", "xenos_unpaid_total_uusdt", "xenos_ipv4_free", "xenos_worker_heartbeat_age_seconds"} {
		declared[n] = true
	}
	raw, err := os.ReadFile("../../deploy/prometheus/alerts.yml")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, name := range regexp.MustCompile(`xenos_[a-z0-9_]+`).FindAllString(string(raw), -1) {
		base := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(name, "_bucket"), "_count"), "_sum")
		seen++
		if !declared[name] && !declared[base] {
			t.Errorf("alerts.yml uses %s, which no code exports", name)
		}
	}
	if seen < 10 {
		t.Fatalf("expected the rules to mention many metrics, found %d", seen)
	}
}
