// Package worker assembles the background processing: the job queue handlers
// (VM lifecycle, conversions) and the hourly metering loop.
//
// Run exactly one worker process: crash recovery requeues every job left
// 'running', which is only correct when no other worker is active.
package worker

import (
	"context"
	"log/slog"
	"net/netip"
	"time"

	"github.com/israel-duff/xenos/internal/alert"
	"github.com/israel-duff/xenos/internal/billing"
	"github.com/israel-duff/xenos/internal/config"
	"github.com/israel-duff/xenos/internal/jobs"
	"github.com/israel-duff/xenos/internal/mail"
	"github.com/israel-duff/xenos/internal/metering"
	"github.com/israel-duff/xenos/internal/monitor"
	"github.com/israel-duff/xenos/internal/proxmox"
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
	var pve proxmox.API
	if cfg.PVEURL == "" {
		log.Warn("XENOS_PVE_URL unset: using in-memory fake Proxmox, no real VMs will be created")
		pve = proxmox.NewFake()
	} else {
		pve = proxmox.New(cfg.PVEURL, cfg.PVENode, cfg.PVETokenID, cfg.PVETokenSecret, cfg.PVEInsecureTLS)
	}

	var v6 netip.Prefix
	if cfg.IPv6Prefix != "" {
		var err error
		if v6, err = netip.ParsePrefix(cfg.IPv6Prefix); err != nil {
			return err
		}
	}
	prov := &vm.Provisioner{Store: st, PVE: pve, Log: log, Cfg: vm.Config{
		Storage: cfg.PVEStorage, Disk: cfg.PVEDisk, DisableKVM: cfg.PVEDisableKVM,
		AgentTimeout: cfg.ProvisionTimeout, PollInterval: 3 * time.Second,
		IPv4PrefixLen: cfg.IPv4PrefixLen, IPv6Prefix: v6, IPv6Gateway: cfg.IPv6Gateway,
		Nameservers: cfg.Nameservers,
	}}

	cache := billing.NewBalanceCache(is, 60*time.Second)
	q := jobs.New(st.Pool)
	wal := &wallet.Service{Store: st, ISpend: is, Cache: cache, Log: log}
	meter := &metering.Meter{Store: st, ISpend: is, Cache: cache, Jobs: q, Mailer: mailer, Log: log,
		Grace: cfg.Grace(), MinRunwayHours: minRunway}

	handlers := prov.Handlers()
	for k, h := range wal.Handlers() {
		handlers[k] = h
	}

	if err := q.RecoverStale(ctx); err != nil {
		return err
	}
	log.Info("worker started", "concurrency", cfg.WorkerConcurrency)
	go meter.Run(ctx, meterInterval)
	notifier := &alert.Notifier{Store: st, Log: log, TelegramToken: cfg.TelegramBotToken, TelegramChat: cfg.TelegramChatID,
		Mailer: mailer, ToEmail: cfg.AlertEmail}
	if !notifier.Configured() {
		log.Warn("no alert channel configured (XENOS_TELEGRAM_* or XENOS_ALERT_EMAIL): alerts will only appear in the log")
	}
	mon := &monitor.Monitor{Store: st, PVE: pve, Notify: notifier, Log: log, Cfg: monitor.DefaultConfig(cfg.PVEStorage)}
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
