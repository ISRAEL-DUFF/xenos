// Package metrics exposes Prometheus metrics. The API and the worker are separate processes, so each serves its
// own registry on a loopback-only listener (never the public port). Every method is safe on a nil *Metrics, so
// code that records a metric needs no checks and tests need no setup.
//
// Labels stay bounded on purpose: HTTP routes are chi route patterns (not raw paths), job kinds and ISpend
// methods are fixed sets, and nothing is labelled by user, VM or hostname.
package metrics

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/israel-duff/xenos/internal/billing"
)

type Metrics struct {
	Reg *prometheus.Registry

	httpReqs *prometheus.CounterVec
	httpDur  *prometheus.HistogramVec
	limited  *prometheus.CounterVec
	webhooks *prometheus.CounterVec
	consoles prometheus.Gauge

	jobs   *prometheus.CounterVec
	jobDur *prometheus.HistogramVec

	ispendCalls *prometheus.CounterVec
	ispendDur   *prometheus.HistogramVec

	meterTick   prometheus.Histogram
	meterLastOK prometheus.Gauge
	ispendDown  prometheus.Gauge

	hostPool *prometheus.GaugeVec
	hostRAM  *prometheus.GaugeVec
}

// New builds a registry with Go runtime and process metrics plus Xenos' own.
func New() *Metrics {
	r := prometheus.NewRegistry()
	r.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	f := promauto(r)
	m := &Metrics{Reg: r}
	m.httpReqs = f.counter("xenos_http_requests_total", "HTTP requests by route pattern, method and status class.", "route", "method", "class")
	m.httpDur = f.histogram("xenos_http_request_duration_seconds", "HTTP request duration by route pattern and method.", prometheus.DefBuckets, "route", "method")
	m.limited = f.counter("xenos_rate_limited_total", "Requests refused by a rate limiter.", "limiter")
	m.webhooks = f.counter("xenos_webhooks_total", "iswallet webhooks by outcome.", "outcome")
	m.consoles = f.gauge1("xenos_consoles_open", "Browser consoles currently open.")
	m.jobs = f.counter("xenos_jobs_total", "Jobs run by kind and result (ok, error, interrupted).", "kind", "result")
	m.jobDur = f.histogram("xenos_job_duration_seconds", "Job handler duration by kind.", []float64{0.1, 0.5, 1, 5, 15, 60, 180, 600}, "kind")
	m.ispendCalls = f.counter("xenos_ispend_calls_total", "Calls to the wallet service by method and result.", "method", "result")
	m.ispendDur = f.histogram("xenos_ispend_call_duration_seconds", "Wallet service call duration by method.", prometheus.DefBuckets, "method")
	m.meterTick = f.histogram1("xenos_meter_tick_duration_seconds", "Duration of one metering pass.", []float64{0.1, 0.5, 1, 5, 15, 60, 180})
	m.meterLastOK = f.gauge1("xenos_meter_last_success_timestamp_seconds", "Unix time of the last metering pass that finished without error.")
	m.ispendDown = f.gauge1("xenos_meter_ispend_down", "1 when the last metering pass found the wallet service unreachable.")
	m.hostPool = f.gauge("xenos_host_pool_used_fraction", "Fraction of the VM disk pool in use, by host.", "host")
	m.hostRAM = f.gauge("xenos_host_ram_committed_fraction", "RAM promised to VMs as a fraction of physical RAM, by host.", "host")
	return m
}

// ---- recording (all nil-safe) ----

func (m *Metrics) RateLimited(limiter string) {
	if m != nil {
		m.limited.WithLabelValues(limiter).Inc()
	}
}

// Webhook records an iswallet webhook outcome: accepted, bad_signature, bad_event or error.
func (m *Metrics) Webhook(outcome string) {
	if m != nil {
		m.webhooks.WithLabelValues(outcome).Inc()
	}
}

func (m *Metrics) ConsoleOpened() {
	if m != nil {
		m.consoles.Inc()
	}
}

func (m *Metrics) ConsoleClosed() {
	if m != nil {
		m.consoles.Dec()
	}
}

// JobDone is the jobs.Queue observer.
func (m *Metrics) JobDone(kind, result string, d time.Duration) {
	if m == nil {
		return
	}
	m.jobs.WithLabelValues(kind, result).Inc()
	m.jobDur.WithLabelValues(kind).Observe(d.Seconds())
}

// MeterTick records one metering pass.
func (m *Metrics) MeterTick(d time.Duration, ok, ispendDown bool) {
	if m == nil {
		return
	}
	m.meterTick.Observe(d.Seconds())
	if ok {
		m.meterLastOK.SetToCurrentTime()
	}
	if ispendDown {
		m.ispendDown.Set(1)
	} else {
		m.ispendDown.Set(0)
	}
}

// HostCapacity records a host's pool and RAM commitment (host is "default" until there is more than one).
func (m *Metrics) HostCapacity(host string, poolFraction, ramFraction float64) {
	if m == nil {
		return
	}
	m.hostPool.WithLabelValues(host).Set(poolFraction)
	m.hostRAM.WithLabelValues(host).Set(ramFraction)
}

// ---- HTTP ----

// HTTP records each request under its route pattern. Install it with r.Use at the top of the router.
func (m *Metrics) HTTP(next http.Handler) http.Handler {
	if m == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		start := time.Now()
		next.ServeHTTP(ww, r)
		route := "unmatched"
		if rc := chi.RouteContext(r.Context()); rc != nil && rc.RoutePattern() != "" {
			route = rc.RoutePattern()
		}
		class := "hijacked" // a websocket upgrade never sets a status through the wrapper
		if s := ww.Status(); s != 0 {
			class = strconv.Itoa(s/100) + "xx"
		}
		m.httpReqs.WithLabelValues(route, r.Method, class).Inc()
		m.httpDur.WithLabelValues(route, r.Method).Observe(time.Since(start).Seconds())
	})
}

// Handler serves the registry in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Reg, promhttp.HandlerOpts{ErrorLog: nil, Registry: m.Reg})
}

// Serve runs the metrics listener until ctx ends. An empty address or "off" disables it. The address should be
// loopback: nothing here is authenticated.
func Serve(ctx context.Context, addr string, m *Metrics, log *slog.Logger) error {
	if addr == "" || addr == "off" || m == nil {
		return nil
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", m.Handler())
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sc)
	}()
	log.Info("metrics listening", "addr", addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// ---- wallet service calls ----

type instrumented struct {
	billing.ISpend
	m *Metrics
}

// WrapISpend counts and times every call the program makes to the wallet service.
func WrapISpend(is billing.ISpend, m *Metrics) billing.ISpend {
	if m == nil {
		return is
	}
	return &instrumented{is, m}
}

func (i *instrumented) observe(method string, start time.Time, err error) {
	i.m.ispendCalls.WithLabelValues(method, resultOf(err)).Inc()
	i.m.ispendDur.WithLabelValues(method).Observe(time.Since(start).Seconds())
}

// resultOf maps an error to a small set of labels.
func resultOf(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, billing.ErrInsufficientFunds):
		return "insufficient_funds"
	case errors.Is(err, billing.ErrRateLimited):
		return "rate_limited"
	case errors.Is(err, billing.ErrLiquidity):
		return "insufficient_liquidity"
	case errors.Is(err, billing.ErrQuoteExpired), errors.Is(err, billing.ErrQuoteUsed), errors.Is(err, billing.ErrQuoteUnavailable):
		return "quote"
	case errors.Is(err, billing.ErrIdempotencyKeyReused), errors.Is(err, billing.ErrCurrencyMismatch), errors.Is(err, billing.ErrScaleMismatch):
		return "contract"
	default:
		return "error"
	}
}

func (i *instrumented) CreateCustomer(ctx context.Context, key, ref, email, phone string) (c billing.Customer, err error) {
	defer func(t time.Time) { i.observe("create_customer", t, err) }(time.Now())
	return i.ISpend.CreateCustomer(ctx, key, ref, email, phone)
}

func (i *instrumented) Customer(ctx context.Context, id string) (c billing.Customer, err error) {
	defer func(t time.Time) { i.observe("customer", t, err) }(time.Now())
	return i.ISpend.Customer(ctx, id)
}

func (i *instrumented) Balances(ctx context.Context, id string) (b billing.Balances, err error) {
	defer func(t time.Time) { i.observe("balances", t, err) }(time.Now())
	return i.ISpend.Balances(ctx, id)
}

func (i *instrumented) MerchantBalance(ctx context.Context) (n int64, err error) {
	defer func(t time.Time) { i.observe("merchant_balance", t, err) }(time.Now())
	return i.ISpend.MerchantBalance(ctx)
}

func (i *instrumented) Rate(ctx context.Context) (n int64, err error) {
	defer func(t time.Time) { i.observe("rate", t, err) }(time.Now())
	return i.ISpend.Rate(ctx)
}

func (i *instrumented) Quote(ctx context.Context, id string, kobo int64) (q billing.Quote, err error) {
	defer func(t time.Time) { i.observe("quote", t, err) }(time.Now())
	return i.ISpend.Quote(ctx, id, kobo)
}

func (i *instrumented) Convert(ctx context.Context, key, id, quote string) (mv billing.Movement, err error) {
	defer func(t time.Time) { i.observe("convert", t, err) }(time.Now())
	return i.ISpend.Convert(ctx, key, id, quote)
}

func (i *instrumented) Charge(ctx context.Context, key, id string, uusdt int64, narration string) (mv billing.Movement, err error) {
	defer func(t time.Time) { i.observe("charge", t, err) }(time.Now())
	return i.ISpend.Charge(ctx, key, id, uusdt, narration)
}

func (i *instrumented) Adjust(ctx context.Context, key, id string, uusdt int64, note string) (mv billing.Movement, err error) {
	defer func(t time.Time) { i.observe("adjust", t, err) }(time.Now())
	return i.ISpend.Adjust(ctx, key, id, uusdt, note)
}
