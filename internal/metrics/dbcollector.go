package metrics

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/israel-duff/xenos/internal/store"
)

// dbCollector reports state that lives in Postgres. It runs at scrape time with a short cache, so a scrape
// storm cannot become a query storm, and is registered only in the worker (one instance), so the numbers are not
// reported twice. Every query is cheap: it counts small sets (open charges, live VMs, queued jobs).
type dbCollector struct {
	st  *store.Store
	ttl time.Duration

	mu      sync.Mutex
	at      time.Time
	metrics []prometheus.Metric
	errs    prometheus.Counter

	vms, jobs, charges, chargesAmt, convs *prometheus.Desc
	oldest, unpaid, ipsFree, ipsTotal     *prometheus.Desc
	heartbeat, webhookFail                *prometheus.Desc
}

// RegisterDB adds the database-derived metrics to the registry.
func (m *Metrics) RegisterDB(st *store.Store) {
	if m == nil || st == nil {
		return
	}
	d := func(name, help string, labels ...string) *prometheus.Desc {
		return prometheus.NewDesc(name, help, labels, nil)
	}
	c := &dbCollector{st: st, ttl: 15 * time.Second,
		errs:        prometheus.NewCounter(prometheus.CounterOpts{Name: "xenos_metrics_collect_errors_total", Help: "Database queries the metrics collector could not run."}),
		vms:         d("xenos_vms", "VMs by state (deleted ones excluded).", "state"),
		jobs:        d("xenos_jobs_pending", "Jobs queued, running or failed, by kind and status.", "kind", "status"),
		charges:     d("xenos_usage_charges_open", "Hourly charges not yet collected, by status (pending or unpaid).", "status"),
		chargesAmt:  d("xenos_usage_charges_open_uusdt", "Micro-USDT in charges not yet collected, by status.", "status"),
		convs:       d("xenos_conversions_open", "Naira conversions that are pending or failed, by status.", "status"),
		oldest:      d("xenos_oldest_pending_charge_age_seconds", "Age of the oldest charge still pending (0 when none): the wallet service may be unreachable."),
		unpaid:      d("xenos_unpaid_total_uusdt", "Micro-USDT customers owe because their wallets were empty."),
		ipsFree:     d("xenos_ipv4_free", "Free IPv4 addresses in the pool."),
		ipsTotal:    d("xenos_ipv4_total", "IPv4 addresses in the pool."),
		heartbeat:   d("xenos_worker_heartbeat_age_seconds", "Seconds since the worker last wrote its heartbeat (-1 if never)."),
		webhookFail: d("xenos_webhook_signature_failures_1h", "Webhook requests with a bad signature in the last hour."),
	}
	m.Reg.MustRegister(c, c.errs)
}

func (c *dbCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.vms, c.jobs, c.charges, c.chargesAmt, c.convs, c.oldest, c.unpaid, c.ipsFree, c.ipsTotal, c.heartbeat, c.webhookFail} {
		ch <- d
	}
}

func (c *dbCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.at) > c.ttl {
		c.metrics, c.at = c.gather(), time.Now()
	}
	for _, m := range c.metrics {
		ch <- m
	}
}

func (c *dbCollector) gather() []prometheus.Metric {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	var out []prometheus.Metric
	add := func(d *prometheus.Desc, v float64, labels ...string) {
		out = append(out, prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...))
	}
	q := func(sql string, scan func(rows interface{ Scan(...any) error })) {
		rows, err := c.st.Pool.Query(ctx, sql)
		if err != nil {
			c.errs.Inc()
			return
		}
		defer rows.Close()
		for rows.Next() {
			scan(rows)
		}
	}

	q(`SELECT state, count(*) FROM vms WHERE deleted_at IS NULL GROUP BY state`, func(r interface{ Scan(...any) error }) {
		var s string
		var n int64
		if r.Scan(&s, &n) == nil {
			add(c.vms, float64(n), s)
		}
	})
	q(`SELECT kind, status, count(*) FROM jobs WHERE status IN ('queued', 'running', 'failed') GROUP BY kind, status`, func(r interface{ Scan(...any) error }) {
		var k, s string
		var n int64
		if r.Scan(&k, &s, &n) == nil {
			add(c.jobs, float64(n), k, s)
		}
	})
	q(`SELECT status, count(*), COALESCE(sum(amount_uusdt), 0) FROM usage_charges WHERE status IN ('pending', 'unpaid') GROUP BY status`, func(r interface{ Scan(...any) error }) {
		var s string
		var n, amt int64
		if r.Scan(&s, &n, &amt) == nil {
			add(c.charges, float64(n), s)
			add(c.chargesAmt, float64(amt), s)
		}
	})
	q(`SELECT status, count(*) FROM conversions WHERE status IN ('pending', 'failed') GROUP BY status`, func(r interface{ Scan(...any) error }) {
		var s string
		var n int64
		if r.Scan(&s, &n) == nil {
			add(c.convs, float64(n), s)
		}
	})
	q(`SELECT COALESCE(EXTRACT(EPOCH FROM now() - min(hour)), 0)::float8,
	          (SELECT COALESCE(sum(amount_uusdt), 0) FROM usage_charges WHERE status = 'unpaid')::float8
	   FROM usage_charges WHERE status = 'pending'`, func(r interface{ Scan(...any) error }) {
		var age, unpaid float64
		if r.Scan(&age, &unpaid) == nil {
			add(c.oldest, age)
			add(c.unpaid, unpaid)
		}
	})
	q(`SELECT count(*) FILTER (WHERE vm_id IS NULL), count(*) FROM ip_addresses`, func(r interface{ Scan(...any) error }) {
		var free, total int64
		if r.Scan(&free, &total) == nil {
			add(c.ipsFree, float64(free))
			add(c.ipsTotal, float64(total))
		}
	})
	q(`SELECT COALESCE((SELECT EXTRACT(EPOCH FROM now() - at) FROM heartbeats WHERE name = 'worker'), -1)::float8`, func(r interface{ Scan(...any) error }) {
		var age float64
		if r.Scan(&age) == nil {
			add(c.heartbeat, age)
		}
	})
	q(`SELECT count(*) FROM webhook_failures WHERE at >= now() - interval '1 hour'`, func(r interface{ Scan(...any) error }) {
		var n int64
		if r.Scan(&n) == nil {
			add(c.webhookFail, float64(n))
		}
	})
	return out
}
