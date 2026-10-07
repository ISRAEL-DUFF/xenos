// Package worker assembles the background processing: the job queue handlers
// (VM lifecycle, conversions) and the hourly metering loop.
//
// Run exactly one worker process: crash recovery requeues every job left
// 'running', which is only correct when no other worker is active.
package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/israel-duff/xenos/internal/alert"
	"github.com/israel-duff/xenos/internal/billing"
	"github.com/israel-duff/xenos/internal/config"
	"github.com/israel-duff/xenos/internal/hosts"
	"github.com/israel-duff/xenos/internal/jobs"
	"github.com/israel-duff/xenos/internal/mail"
	"github.com/israel-duff/xenos/internal/metering"
	"github.com/israel-duff/xenos/internal/metrics"
	"github.com/israel-duff/xenos/internal/monitor"
	"github.com/israel-duff/xenos/internal/store"
	"github.com/israel-duff/xenos/internal/vm"
	"github.com/israel-duff/xenos/internal/wallet"
)

const (
	meterInterval = time.Minute
	minRunway     = 24 // hours of usage a wallet must cover
)

// Run blocks until ctx is cancelled.
func Run(ctx context.Context, cfg config.Config, st *store.Store, is billing.ISpend, mailer mail.Mailer, log *slog.Logger) error {
	hs, err := NewHosts(ctx, cfg, st, log)
	if err != nil {
		return err
	}
	return RunWith(ctx, cfg, st, is, mailer, log, hs, nil)
}

// RunWith is Run with the Proxmox client supplied, so a process that also serves the API (development, with
// the in-memory fake) can share one host between the worker and the console.
//
// m, if given, is the metrics registry of a process that also serves the API: the worker records into it and does
// not open a second listener. With nil, the worker makes its own and serves it on XENOS_WORKER_METRICS_ADDR.
func RunWith(ctx context.Context, cfg config.Config, st *store.Store, is billing.ISpend, mailer mail.Mailer, log *slog.Logger, hs *hosts.Set, m *metrics.Metrics) error {
	if m == nil {
		m = metrics.New()
		is = metrics.WrapISpend(is, m)
		m.RegisterDB(st)
		go func() {
			if err := metrics.Serve(ctx, cfg.WorkerMetricsAddr, m, log); err != nil && ctx.Err() == nil {
				log.Error("worker metrics listener", "err", err)
			}
		}()
	} else {
		m.RegisterDB(st)
	}
	shared := vm.Config{AgentTimeout: cfg.ProvisionTimeout, PollInterval: 3 * time.Second, IPv4PrefixLen: cfg.IPv4PrefixLen}
	prov := &vm.Provisioner{Store: st, PVE: hs, Log: log, Cfg: shared, HostCfg: func(name string) vm.Config {
		c := shared
		if h, ok := hs.Get(name); ok {
			c.Storage, c.Disk, c.DisableKVM, c.IPv6Prefix, c.IPv6Gateway, c.Nameservers, c.PrivateBridge = h.Storage, h.Disk, h.DisableKVM, h.IPv6Prefix, h.IPv6Gateway, h.Nameservers, h.PrivateBridge
		}
		return c
	}}

	cache := billing.NewBalanceCache(is, 60*time.Second)
	q := jobs.New(st.Pool)
	q.Observe = m.JobDone
	notifier := &alert.Notifier{Store: st, Log: log, TelegramToken: cfg.TelegramBotToken, TelegramChat: cfg.TelegramChatID,
		Mailer: mailer, ToEmail: cfg.AlertEmail}
	wal := &wallet.Service{Store: st, ISpend: is, Cache: cache, Log: log, Alerter: notifier}
	meter := &metering.Meter{Store: st, ISpend: is, Cache: cache, Jobs: q, Mailer: mailer, Log: log,
		Grace: cfg.Grace(), MinRunwayHours: minRunway, SpreadMinutes: cfg.MeterSpreadMinutes, FloatingIPPriceUUSDT: cfg.FloatingIPPriceUUSDT, Alerts: notifier, Metrics: m}

	handlers := prov.Handlers()
	for k, h := range wal.Handlers() {
		handlers[k] = h
	}

	if err := q.RecoverStale(ctx); err != nil {
		return err
	}
	log.Info("worker started", "concurrency", cfg.WorkerConcurrency)
	go meter.Run(ctx, meterInterval)
	if !notifier.Configured() {
		log.Warn("no alert channel configured (XENOS_TELEGRAM_* or XENOS_ALERT_EMAIL): alerts will only appear in the log")
	}
	mon := &monitor.Monitor{Store: st, PVE: hs, Hosts: hs, Notify: notifier, Log: log, Cfg: monitor.DefaultConfig(cfg.PVEStorage), Metrics: m}
	go mon.Run(ctx, meterInterval)
	go func() {
		t := time.NewTicker(meterInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := wal.Sweep(ctx, q); err != nil && ctx.Err() == nil {
					log.Error("conversion sweep", "err", err)
				}
			}
		}
	}()
	q.Run(ctx, cfg.WorkerConcurrency, handlers, func(err error) { log.Error("job", "err", err) })
	return nil
}

// NewHosts loads the hosts (XENOS_HOSTS_FILE, or the single XENOS_PVE_* host) and connects them to the database.
func NewHosts(ctx context.Context, cfg config.Config, st *store.Store, log *slog.Logger) (*hosts.Set, error) {
	hs, err := hosts.Load(cfg, log)
	if err != nil {
		return nil, err
	}
	return hs, hs.Attach(ctx, st)
}
