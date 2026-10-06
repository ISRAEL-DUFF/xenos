// Package vm holds the worker-side VM lifecycle: provisioning, power actions
// and deletion. Only this package moves a VM between states; the HTTP API
// validates a request and enqueues a job.
package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/israel-duff/xenos/internal/jobs"
	"github.com/israel-duff/xenos/internal/proxmox"
	"github.com/israel-duff/xenos/internal/store"
	"github.com/israel-duff/xenos/internal/store/db"
)

// Job kinds.
const (
	JobProvision = "vm.provision"
	JobPower     = "vm.power"
	JobDelete    = "vm.delete"
	JobSuspend   = "vm.suspend" // out of funds: stop the guest, keep its disk
	JobResume    = "vm.resume"  // funded again: back to stopped; the customer starts it

	JobResize         = "vm.resize"          // move to a larger plan (stop, resize, start)
	JobSnapshot       = "vm.snapshot"        // take a customer snapshot
	JobSnapshotDelete = "vm.snapshot_delete" // remove a customer snapshot
	JobRestore        = "vm.restore"         // roll the disk back to a snapshot
)

// Power actions accepted by JobPower.
const (
	ActionStart  = "start"
	ActionStop   = "stop"
	ActionReboot = "reboot"
)

type Payload struct {
	VMID   int64  `json:"vm_id"`
	Action string `json:"action,omitempty"`
	// SnapshotID is the snapshots row a snapshot, delete or restore job works on.
	SnapshotID int64 `json:"snapshot_id,omitempty"`
}

type Config struct {
	Storage       string
	Disk          string
	DisableKVM    bool
	AgentTimeout  time.Duration // how long to wait for the guest agent after start
	PollInterval  time.Duration
	IPv4PrefixLen int
	IPv6Prefix    netip.Prefix // zero value disables IPv6
	IPv6Gateway   string
	Nameservers   string
}

type Provisioner struct {
	Store *store.Store
	PVE   proxmox.API
	Cfg   Config
	Log   *slog.Logger
	Now   func() time.Time // defaults to time.Now; tests inject a clock

	mu    sync.Mutex
	locks map[int64]*vmLock
}

type vmLock struct {
	mu   sync.Mutex
	refs int
}

// lock serialises jobs for one VM (a delete must not race its own provisioning).
func (p *Provisioner) lock(id int64) func() {
	p.mu.Lock()
	if p.locks == nil {
		p.locks = map[int64]*vmLock{}
	}
	l := p.locks[id]
	if l == nil {
		l = &vmLock{}
		p.locks[id] = l
	}
	l.refs++
	p.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		p.mu.Lock()
		if l.refs--; l.refs == 0 {
			delete(p.locks, id)
		}
		p.mu.Unlock()
	}
}

func (p *Provisioner) Handlers() map[string]jobs.Handler {
	return map[string]jobs.Handler{
		JobProvision: p.handle(p.provision),
		JobPower:     p.handle(p.power),
		JobDelete:    p.handle(p.delete),
		JobSuspend:   p.handle(p.suspend),
		JobResume:    p.handle(p.resume),

		JobResize:         p.handle(p.resize),
		JobSnapshot:       p.handle(p.snapshotCreate),
		JobSnapshotDelete: p.handle(p.snapshotDelete),
		JobRestore:        p.handle(p.restore),
	}
}

func (p *Provisioner) now() time.Time {
	if p.Now != nil {
		return p.Now().UTC()
	}
	return time.Now().UTC()
}

func tsOf(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

type step func(ctx context.Context, j *jobs.Job, in Payload) error

func (p *Provisioner) handle(fn step) jobs.Handler {
	return func(ctx context.Context, j *jobs.Job) error {
		var in Payload
		if err := json.Unmarshal(j.Payload, &in); err != nil {
			return fmt.Errorf("bad payload: %w", err)
		}
		defer p.lock(in.VMID)()
		return fn(ctx, j, in)
	}
}

func (p *Provisioner) work(ctx context.Context, id int64) (db.GetVMForWorkRow, error) {
	return p.Store.Q.GetVMForWork(ctx, id)
}

func (p *Provisioner) transition(ctx context.Context, id int64, from []string, to string) (bool, error) {
	n, err := p.Store.Q.TransitionVM(ctx, db.TransitionVMParams{ID: id, Column2: from, State: to})
	return n > 0, err
}

// ---- provisioning ----

func (p *Provisioner) provision(ctx context.Context, j *jobs.Job, in Payload) error {
	w, err := p.work(ctx, in.VMID)
	if err != nil {
		return err
	}
	if w.State != "pending" && w.State != "provisioning" {
		return nil // deleted, deleting or already running: nothing to do
	}
	err = p.build(ctx, w)
	if err == nil {
		return nil
	}
	if ctx.Err() == nil && j.Attempts >= jobs.MaxAttempts {
		// Out of retries: destroy the half-built VM, free the IP, mark error.
		p.abort(context.WithoutCancel(ctx), w, err)
	}
	return err
}

func (p *Provisioner) build(ctx context.Context, w db.GetVMForWorkRow) error {
	vmid := int(w.ProxmoxVmid.Int32)
	if _, err := p.transition(ctx, w.ID, []string{"pending", "provisioning"}, "provisioning"); err != nil {
		return err
	}

	// A previous attempt (or a crashed worker) may have left a half-built guest
	// under this VMID. We own the VMID, so start from a clean slate.
	if err := p.destroyIfPresent(ctx, vmid); err != nil {
		return fmt.Errorf("clear leftover vm: %w", err)
	}

	ipv6 := ""
	if p.Cfg.IPv6Prefix.IsValid() {
		ipv6 = IPv6For(p.Cfg.IPv6Prefix, w.ID).String()
		if err := p.Store.Q.SetVMIPv6(ctx, db.SetVMIPv6Params{ID: w.ID, Ipv6: textOf(ipv6)}); err != nil {
			return err
		}
	}

	upid, err := p.PVE.Clone(ctx, proxmox.CloneParams{
		TemplateID: int(w.ProxmoxTemplateID), NewID: vmid, Name: w.Hostname, Storage: p.Cfg.Storage})
	if err != nil {
		return fmt.Errorf("clone: %w", err)
	}
	if err := p.PVE.WaitTask(ctx, upid); err != nil {
		return fmt.Errorf("clone task: %w", err)
	}
	if err := p.PVE.Configure(ctx, vmid, proxmox.ConfigParams{
		Cores: int(w.Vcpu), MemoryMB: int(w.RamMb), CIUser: w.CiUser,
		SSHKeys:    w.AuthorizedKeys,
		IPConfig0:  ipConfig(w.Ipv4, p.Cfg.IPv4PrefixLen, w.Gateway, ipv6, p.Cfg.IPv6Gateway),
		Nameserver: p.Cfg.Nameservers,
		DisableKVM: p.Cfg.DisableKVM,
	}); err != nil {
		return fmt.Errorf("configure: %w", err)
	}
	if err := p.PVE.ResizeDisk(ctx, vmid, p.Cfg.Disk, int(w.DiskGb)); err != nil {
		return fmt.Errorf("resize: %w", err)
	}
	if upid, err = p.PVE.Power(ctx, vmid, "start"); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	if err := p.PVE.WaitTask(ctx, upid); err != nil {
		return fmt.Errorf("start task: %w", err)
	}
	if err := p.waitAgent(ctx, vmid); err != nil {
		return err
	}
	// Billing starts now, at the top of the current hour (hours are charged in advance).
	// A VM that never reaches running is never charged, so a failed build needs no refund.
	if _, err := p.Store.Q.MarkVMRunning(ctx, db.MarkVMRunningParams{ID: w.ID, BillingFrom: tsOf(p.now().Truncate(time.Hour))}); err != nil {
		return err
	}
	p.Log.Info("vm running", "vm_id", w.ID, "vmid", vmid)
	return nil
}

func (p *Provisioner) waitAgent(ctx context.Context, vmid int) error {
	ctx, cancel := context.WithTimeout(ctx, p.Cfg.AgentTimeout)
	defer cancel()
	var last error
	for {
		if last = p.PVE.AgentPing(ctx, vmid); last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("guest agent did not respond within %s: %w", p.Cfg.AgentTimeout, last)
		case <-time.After(p.Cfg.PollInterval):
		}
	}
}

// abort cleans up after the final failed attempt. The IP is only released once
// the guest is confirmed gone, so a guest we failed to destroy can never share
// its address with a new VM.
func (p *Provisioner) abort(ctx context.Context, w db.GetVMForWorkRow, cause error) {
	log := p.Log.With("vm_id", w.ID, "vmid", w.ProxmoxVmid.Int32)
	log.Error("provisioning failed permanently", "err", cause)
	cleaned := true
	if err := p.destroyIfPresent(ctx, int(w.ProxmoxVmid.Int32)); err != nil {
		cleaned = false
		log.Error("cleanup failed; keeping IP reserved for manual review", "err", err)
	}
	if cleaned {
		if err := p.Store.Q.ReleaseIPForVM(ctx, pgtype.Int8{Int64: w.ID, Valid: true}); err != nil {
			log.Error("release ip", "err", err)
		}
	}
	if _, err := p.transition(ctx, w.ID, []string{"pending", "provisioning"}, "error"); err != nil {
		log.Error("mark error", "err", err)
	}
}

// destroyIfPresent stops and removes the guest if it exists. A failed stop is
// ignored (it may already be stopped); the destroy result is what counts.
func (p *Provisioner) destroyIfPresent(ctx context.Context, vmid int) error {
	st, err := p.PVE.Status(ctx, vmid)
	if err != nil || !st.Exists {
		return err
	}
	if st.Running {
		if upid, err := p.PVE.Power(ctx, vmid, "stop"); err == nil {
			_ = p.PVE.WaitTask(ctx, upid)
		}
	}
	upid, err := p.PVE.Destroy(ctx, vmid)
	if err != nil {
		return err
	}
	return p.PVE.WaitTask(ctx, upid)
}

// ---- power actions ----

func (p *Provisioner) power(ctx context.Context, _ *jobs.Job, in Payload) error {
	w, err := p.work(ctx, in.VMID)
	if err != nil {
		return err
	}
	vmid := int(w.ProxmoxVmid.Int32)
	st, err := p.PVE.Status(ctx, vmid)
	if err != nil {
		return err
	}
	if !st.Exists {
		return fmt.Errorf("vm %d (vmid %d) is missing from proxmox", w.ID, vmid)
	}

	switch in.Action {
	case ActionStart:
		if w.State != "stopped" && w.State != "running" {
			return nil
		}
		if !st.Running {
			if err := p.run(ctx, vmid, "start"); err != nil {
				return err
			}
		}
		_, err = p.transition(ctx, w.ID, []string{"stopped"}, "running")
	case ActionStop:
		if w.State != "running" && w.State != "stopped" {
			return nil
		}
		if st.Running {
			if err := p.run(ctx, vmid, "shutdown"); err != nil {
				return err
			}
		}
		_, err = p.transition(ctx, w.ID, []string{"running"}, "stopped")
	case ActionReboot:
		if w.State != "running" {
			return nil
		}
		err = p.run(ctx, vmid, "reboot")
	default:
		return fmt.Errorf("unknown power action %q", in.Action)
	}
	return err
}

func (p *Provisioner) run(ctx context.Context, vmid int, action string) error {
	upid, err := p.PVE.Power(ctx, vmid, action)
	if err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	if err := p.PVE.WaitTask(ctx, upid); err != nil {
		return fmt.Errorf("%s task: %w", action, err)
	}
	return nil
}

// ---- deletion ----

func (p *Provisioner) delete(ctx context.Context, _ *jobs.Job, in Payload) error {
	w, err := p.work(ctx, in.VMID)
	if err != nil {
		return err
	}
	if w.State == "deleted" {
		return nil
	}
	if _, err := p.transition(ctx, w.ID,
		[]string{"pending", "provisioning", "running", "stopped", "suspended", "error", "deleting"}, "deleting"); err != nil {
		return err
	}
	// Billing ends when deletion is requested; the hour in progress was already charged.
	if err := p.Store.Q.StopBilling(ctx, db.StopBillingParams{ID: w.ID, BillingUntil: tsOf(p.now())}); err != nil {
		return err
	}
	if err := p.destroyIfPresent(ctx, int(w.ProxmoxVmid.Int32)); err != nil {
		return fmt.Errorf("destroy: %w", err)
	}
	if err := p.Store.Q.ReleaseIPForVM(ctx, pgtype.Int8{Int64: w.ID, Valid: true}); err != nil {
		return err
	}
	return p.Store.Q.MarkVMDeleted(ctx, w.ID)
}

// ---- suspension ----

// suspend stops a guest whose owner ran out of funds and ends its billing. The disk is kept.
func (p *Provisioner) suspend(ctx context.Context, _ *jobs.Job, in Payload) error {
	w, err := p.work(ctx, in.VMID)
	if err != nil {
		return err
	}
	if w.State != "running" && w.State != "stopped" {
		return nil
	}
	if st, err := p.PVE.Status(ctx, int(w.ProxmoxVmid.Int32)); err != nil {
		return err
	} else if st.Running {
		if err := p.run(ctx, int(w.ProxmoxVmid.Int32), "shutdown"); err != nil {
			return err
		}
	}
	_, err = p.Store.Q.SuspendVM(ctx, db.SuspendVMParams{ID: w.ID, SuspendedAt: tsOf(p.now())})
	return err
}

// resume returns a suspended VM to stopped and restarts its billing from the current hour.
func (p *Provisioner) resume(ctx context.Context, _ *jobs.Job, in Payload) error {
	_, err := p.Store.Q.ResumeVM(ctx, db.ResumeVMParams{ID: in.VMID, BillingFrom: tsOf(p.now().Truncate(time.Hour))})
	return err
}

// ---- helpers ----

// ipConfig builds the Proxmox ipconfig0 string.
func ipConfig(v4 string, prefixLen int, gw4, v6, gw6 string) string {
	parts := []string{fmt.Sprintf("ip=%s/%d", v4, prefixLen), "gw=" + gw4}
	if v6 != "" {
		parts = append(parts, "ip6="+v6+"/64")
		if gw6 != "" {
			parts = append(parts, "gw6="+gw6)
		}
	}
	return strings.Join(parts, ",")
}

// IPv6For derives a VM's address from the /64: the low 64 bits are 0x100 + the
// VM's row id. Deterministic, so a retry always yields the same address.
func IPv6For(prefix netip.Prefix, vmID int64) netip.Addr {
	b := prefix.Masked().Addr().As16()
	n := uint64(vmID) + 0x100
	for i := 15; i >= 8; i-- {
		b[i] = byte(n)
		n >>= 8
	}
	return netip.AddrFrom16(b)
}

func textOf(v string) pgtype.Text { return pgtype.Text{String: v, Valid: true} }
