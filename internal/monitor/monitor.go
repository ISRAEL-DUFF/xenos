// Package monitor watches the platform and the host for the problems an
// operator must hear about: failed jobs, a filling disk pool, overcommitted
// RAM, exhausted IPs, stuck billing or conversions, webhook probing, and VMs
// pinned at full CPU (usually crypto mining).
package monitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/israel-duff/xenos/internal/alert"
	"github.com/israel-duff/xenos/internal/proxmox"
	"github.com/israel-duff/xenos/internal/store"
	"github.com/israel-duff/xenos/internal/store/db"
)

// Thresholds. The plan specifies the pool at 80%, RAM at 90% and CPU at 90% for 6 hours.
type Config struct {
	Storage           string
	PoolWarn          float64
	RAMWarn           float64
	MinFreeIPs        int64
	CPUHigh           float64       // start the clock at or above this
	CPULow            float64       // reset it below this (hysteresis, so a brief dip does not hide a miner)
	CPUWindow         time.Duration // sustained this long => flag
	StaleChargeAfter  time.Duration // charges pending this long mean iSpend is failing
	WebhookFailures   int64         // rejected webhooks in WebhookWindow
	WebhookWindow     time.Duration
	ConversionStuckAt time.Duration
}

func DefaultConfig(storage string) Config {
	return Config{Storage: storage, PoolWarn: 0.80, RAMWarn: 0.90, MinFreeIPs: 2,
		CPUHigh: 0.90, CPULow: 0.50, CPUWindow: 6 * time.Hour,
		StaleChargeAfter: 30 * time.Minute, WebhookFailures: 5, WebhookWindow: 10 * time.Minute,
		ConversionStuckAt: 15 * time.Minute}
}

type Monitor struct {
	Store  *store.Store
	PVE    proxmox.API
	Notify *alert.Notifier
	Log    *slog.Logger
	Now    func() time.Time
	Cfg    Config
}

func (m *Monitor) now() time.Time {
	if m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}

// Run ticks until ctx ends. Each tick also records the worker heartbeat.
func (m *Monitor) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := m.Tick(ctx); err != nil && ctx.Err() == nil {
			m.Log.Error("monitor tick", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick runs every check; one failing check never stops the others.
func (m *Monitor) Tick(ctx context.Context) error {
	var errs []error
	if err := m.Store.Q.Heartbeat(ctx, db.HeartbeatParams{Name: "worker", At: m.now()}); err != nil {
		errs = append(errs, err)
	}
	for _, check := range []func(context.Context) error{
		m.failedJobs, m.hostCapacity, m.freeIPs, m.billing, m.webhooks, m.cpuWatch,
	} {
		if err := check(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *Monitor) failedJobs(ctx context.Context) error {
	jobs, err := m.Store.Q.ListUnalertedFailedJobs(ctx)
	if err != nil || len(jobs) == 0 {
		return err
	}
	ids := make([]int64, 0, len(jobs))
	var b strings.Builder
	fmt.Fprintf(&b, "%d job(s) failed after all retries:", len(jobs))
	for _, j := range jobs {
		ids = append(ids, j.ID)
		fmt.Fprintf(&b, "\n#%d %s: %s", j.ID, j.Kind, truncate(j.LastError, 200))
	}
	// Alert first, mark second: a crash in between repeats the alert rather than losing it.
	m.Notify.Notify(ctx, fmt.Sprintf("jobs-failed-%d", ids[0]), 0, b.String())
	return m.Store.Q.MarkJobsAlerted(ctx, db.MarkJobsAlertedParams{Column1: ids, AlertedAt: pgtype.Timestamptz{Time: m.now(), Valid: true}})
}

// hostCapacity checks the disk pool and committed RAM on the Proxmox host. If
// the API cannot be reached at all that is itself the alert.
func (m *Monitor) hostCapacity(ctx context.Context) error {
	pool, err := m.PVE.StoragePool(ctx, m.Cfg.Storage)
	if err != nil {
		m.Notify.Notify(ctx, "proxmox-unreachable", 30*time.Minute, "Proxmox API is not answering: "+truncate(err.Error(), 200))
		return nil
	}
	if f := pool.Fraction(); f >= m.Cfg.PoolWarn {
		m.Notify.Notify(ctx, "pool-full", 6*time.Hour,
			fmt.Sprintf("Disk pool %q is %.0f%% full (%d of %d GiB). Thin provisioning means VMs can fail when it fills; stop selling or add disk.",
				m.Cfg.Storage, f*100, pool.Used>>30, pool.Total>>30))
	}

	node, err := m.PVE.NodeInfo(ctx)
	total := node.MemTotal
	if err != nil {
		m.Notify.Notify(ctx, "proxmox-unreachable", 30*time.Minute, "Proxmox API is not answering: "+truncate(err.Error(), 200))
		return nil
	}
	committedMB, err := m.Store.Q.CommittedRAMMB(ctx)
	if err != nil {
		return err
	}
	if total > 0 {
		if f := float64(committedMB<<20) / float64(total); f >= m.Cfg.RAMWarn {
			m.Notify.Notify(ctx, "ram-committed", 6*time.Hour,
				fmt.Sprintf("Host RAM is %.0f%% committed (%d MiB promised to VMs of %d MiB physical).", f*100, committedMB, total>>20))
		}
	}
	return nil
}

func (m *Monitor) freeIPs(ctx context.Context) error {
	n, err := m.Store.Q.CountFreeIPs(ctx)
	if err != nil {
		return err
	}
	if n < m.Cfg.MinFreeIPs {
		m.Notify.Notify(ctx, "ips-low", 6*time.Hour, fmt.Sprintf("Only %d free IPv4 address(es) left; new VMs will be refused at 0.", n))
	}
	return nil
}

// billing alerts when iSpend looks broken: charges that will not complete, or conversions that keep failing.
func (m *Monitor) billing(ctx context.Context) error {
	now := m.now()
	stale, err := m.Store.Q.CountStalePendingCharges(ctx, now.Add(-m.Cfg.StaleChargeAfter))
	if err != nil {
		return err
	}
	if stale > 0 {
		m.Notify.Notify(ctx, "charges-stuck", time.Hour,
			fmt.Sprintf("%d usage charge(s) have been pending for over %s: iSpend may be unreachable. Hours will be collected late, not lost.", stale, m.Cfg.StaleChargeAfter))
	}
	stuck, err := m.Store.Q.CountStuckConversions(ctx, db.CountStuckConversionsParams{
		CreatedAt: now.Add(-24 * time.Hour), CreatedAt_2: now.Add(-m.Cfg.ConversionStuckAt)})
	if err != nil {
		return err
	}
	if stuck > 0 {
		m.Notify.Notify(ctx, "conversions-stuck", time.Hour,
			fmt.Sprintf("%d naira conversion(s) are failing or stuck. Customer naira is safe in their NGN wallets; check iSpend quoting.", stuck))
	}
	return nil
}

func (m *Monitor) webhooks(ctx context.Context) error {
	now := m.now()
	n, err := m.Store.Q.CountWebhookFailuresSince(ctx, now.Add(-m.Cfg.WebhookWindow))
	if err != nil {
		return err
	}
	if n >= m.Cfg.WebhookFailures {
		m.Notify.Notify(ctx, "webhook-signature", time.Hour,
			fmt.Sprintf("%d iSpend webhooks were rejected for a bad signature in the last %s: wrong secret, or someone is probing.", n, m.Cfg.WebhookWindow))
	}
	return m.Store.Q.PruneWebhookFailures(ctx, now.Add(-24*time.Hour))
}

// cpuWatch flags VMs that stay pinned near 100% CPU, the signature of crypto mining.
// The clock starts when a sample reaches CPUHigh and resets only when CPU falls
// under CPULow, so a miner throttling briefly does not escape.
func (m *Monitor) cpuWatch(ctx context.Context) error {
	guests, err := m.PVE.Guests(ctx)
	if err != nil {
		return nil // reported by hostCapacity
	}
	cpu := make(map[int]float64, len(guests))
	for _, g := range guests {
		if g.Running {
			cpu[g.VMID] = g.CPU
		}
	}
	vms, err := m.Store.Q.ListRunningVMsForCPU(ctx)
	if err != nil {
		return err
	}
	now := m.now()
	ts := func(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: !t.IsZero()} }
	for _, v := range vms {
		use, ok := cpu[int(v.ProxmoxVmid.Int32)]
		if !ok {
			continue
		}
		switch {
		case use >= m.Cfg.CPUHigh && !v.CpuHighSince.Valid:
			err = m.Store.Q.SetCPUHighSince(ctx, db.SetCPUHighSinceParams{ID: v.ID, CpuHighSince: ts(now)})
		case use < m.Cfg.CPULow && v.CpuHighSince.Valid:
			err = m.Store.Q.SetCPUHighSince(ctx, db.SetCPUHighSinceParams{ID: v.ID})
		case v.CpuHighSince.Valid && !v.FlaggedAt.Valid && now.Sub(v.CpuHighSince.Time) >= m.Cfg.CPUWindow:
			reason := fmt.Sprintf("CPU at or above %.0f%% since %s", m.Cfg.CPUHigh*100, v.CpuHighSince.Time.Format(time.RFC3339))
			if err = m.Store.Q.FlagVM(ctx, db.FlagVMParams{ID: v.ID, FlaggedAt: ts(now), FlagReason: pgtype.Text{String: reason, Valid: true}}); err == nil {
				m.Notify.Notify(ctx, fmt.Sprintf("cpu-%d", v.ID), 0,
					fmt.Sprintf("VM %d (%s) flagged for review: %s. Possible crypto mining.", v.ID, v.Hostname, reason))
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
