// Command worker runs background jobs: provisioning, power actions, deletion
// (and, in later phases, metering and suspension).
//
// Run a single worker process: crash recovery requeues every job left
// 'running', which is only correct when no other worker is active.
package main

import (
	"context"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/israel-duff/xenos/internal/config"
	"github.com/israel-duff/xenos/internal/jobs"
	"github.com/israel-duff/xenos/internal/proxmox"
	"github.com/israel-duff/xenos/internal/store"
	"github.com/israel-duff/xenos/internal/vm"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	var pve proxmox.API
	if cfg.PVEURL == "" {
		log.Warn("XENOS_PVE_URL unset: using in-memory fake Proxmox, no real VMs will be created")
		pve = proxmox.NewFake()
	} else {
		pve = proxmox.New(cfg.PVEURL, cfg.PVENode, cfg.PVETokenID, cfg.PVETokenSecret, cfg.PVEInsecureTLS)
	}

	var v6 netip.Prefix
	if cfg.IPv6Prefix != "" {
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

	q := jobs.New(st.Pool)
	if err := q.RecoverStale(ctx); err != nil {
		return err
	}
	log.Info("worker started", "concurrency", cfg.WorkerConcurrency)
	q.Run(ctx, cfg.WorkerConcurrency, prov.Handlers(), func(err error) { log.Error("job", "err", err) })
	return nil
}
