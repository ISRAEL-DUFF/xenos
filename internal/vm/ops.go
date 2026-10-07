package vm

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/israel-duff/xenos/internal/jobs"
	"github.com/israel-duff/xenos/internal/store/db"
)

// Customer-requested operations: resize, snapshots and restore. Each is claimed through vms.busy by the API
// (one at a time per VM) and released here when the job ends. After the last failed attempt the claim is
// released too, so a VM is never stuck "busy"; the failed job itself raises the operator alert.

// SnapshotName is the Proxmox name of a snapshot row: Proxmox needs a letter first and no spaces, so the
// customer's label is kept in our table only.
func SnapshotName(id int64) string { return fmt.Sprintf("xs%d", id) }

func (p *Provisioner) release(ctx context.Context, id int64) {
	if err := p.Store.Q.ReleaseVMBusy(context.WithoutCancel(ctx), id); err != nil {
		p.Log.Error("release busy", "vm_id", id, "err", err)
	}
}

// finalFailure reports whether this was the last attempt (not a shutdown in flight).
func finalFailure(ctx context.Context, j *jobs.Job) bool {
	return ctx.Err() == nil && j.Attempts >= jobs.MaxAttempts
}

func operable(state string) bool { return state == "running" || state == "stopped" }

// ---- resize ----

func (p *Provisioner) resize(ctx context.Context, j *jobs.Job, in Payload) error {
	w, err := p.Store.Q.GetResizeWork(ctx, in.VMID)
	if err != nil {
		return err
	}
	if !w.Busy.Valid || w.Busy.String != "resizing" || !w.ResizePlanID.Valid {
		return nil // nothing claimed: a stale job
	}
	if !operable(w.State) { // suspended or deleted while queued
		p.release(ctx, w.ID)
		return nil
	}
	err = p.doResize(ctx, w)
	if err == nil {
		return nil
	}
	if finalFailure(ctx, j) {
		p.Log.Error("resize failed permanently; releasing the VM, the host may hold a partial change", "vm_id", w.ID, "err", err)
		p.release(ctx, w.ID)
		if w.State == "running" { // best effort: do not leave the customer's VM powered off
			if st, e := p.PVE.Status(context.WithoutCancel(ctx), int(w.ProxmoxVmid.Int32)); e == nil && st.Exists && !st.Running {
				_ = p.run(context.WithoutCancel(ctx), int(w.ProxmoxVmid.Int32), "start")
			}
		}
	}
	return err
}

// doResize is safe to repeat from any point: every step checks the host first.
func (p *Provisioner) doResize(ctx context.Context, w db.GetResizeWorkRow) error {
	vmid := int(w.ProxmoxVmid.Int32)
	st, err := p.PVE.Status(ctx, vmid)
	if err != nil {
		return err
	}
	if !st.Exists {
		return fmt.Errorf("vm %d (vmid %d) is missing from proxmox", w.ID, vmid)
	}
	if st.Running { // cores and memory only apply after a full stop and start
		if err := p.run(ctx, vmid, "shutdown"); err != nil {
			return err
		}
	}
	if err := p.PVE.SetResources(ctx, vmid, int(w.NewVcpu.Int32), int(w.NewRamMb.Int32)); err != nil {
		return fmt.Errorf("set resources: %w", err)
	}
	size, err := p.PVE.DiskSizeGB(ctx, vmid, p.cfgFor(w.Host).Disk)
	if err != nil {
		return fmt.Errorf("read disk size: %w", err)
	}
	if size < int(w.NewDiskGb.Int32) { // a disk only grows, and only once
		if err := p.PVE.ResizeDisk(ctx, vmid, p.cfgFor(w.Host).Disk, int(w.NewDiskGb.Int32)); err != nil {
			return fmt.Errorf("resize disk: %w", err)
		}
	}
	if w.State == "running" {
		if err := p.run(ctx, vmid, "start"); err != nil {
			return err
		}
		if err := p.waitAgent(ctx, vmid); err != nil {
			return err
		}
	}
	// The new price applies from the next hour that is charged; the hour in progress was charged at the old one.
	if n, err := p.Store.Q.FinishResize(ctx, w.ID); err != nil {
		return err
	} else if n == 0 {
		return errors.New("resize claim was lost")
	}
	p.Log.Info("vm resized", "vm_id", w.ID, "plan_id", w.ResizePlanID.Int64)
	return p.reapplyFloating(ctx, w.ID) // the guest was restarted
}

// ---- snapshots ----

func (p *Provisioner) snapshotCreate(ctx context.Context, j *jobs.Job, in Payload) error {
	w, err := p.work(ctx, in.VMID)
	if err != nil {
		return err
	}
	snap, err := p.Store.Q.GetVMSnapshot(ctx, db.GetVMSnapshotParams{ID: in.SnapshotID, VmID: in.VMID})
	if errors.Is(err, pgx.ErrNoRows) {
		p.release(ctx, in.VMID)
		return nil
	} else if err != nil {
		return err
	}
	if snap.Status != "creating" {
		p.release(ctx, in.VMID)
		return nil
	}
	if !operable(w.State) {
		_ = p.Store.Q.SetSnapshotStatus(ctx, db.SetSnapshotStatusParams{ID: snap.ID, Status: "error", LastError: textOf("the VM was not running or stopped")})
		p.release(ctx, in.VMID)
		return nil
	}
	vmid := int(w.ProxmoxVmid.Int32)
	err = func() error {
		names, err := p.PVE.SnapshotList(ctx, vmid)
		if err != nil {
			return err
		}
		if !slices.Contains(names, snap.PveName) { // a retry after a lost reply must not create it twice
			upid, err := p.PVE.SnapshotCreate(ctx, vmid, snap.PveName)
			if err != nil {
				return fmt.Errorf("snapshot: %w", err)
			}
			if err := p.PVE.WaitTask(ctx, upid); err != nil {
				return fmt.Errorf("snapshot task: %w", err)
			}
		}
		return p.Store.Q.SetSnapshotStatus(ctx, db.SetSnapshotStatusParams{ID: snap.ID, Status: "ready"})
	}()
	switch {
	case err == nil:
		p.release(ctx, in.VMID)
	case finalFailure(ctx, j):
		_ = p.Store.Q.SetSnapshotStatus(context.WithoutCancel(ctx), db.SetSnapshotStatusParams{ID: snap.ID, Status: "error", LastError: textOf(err.Error())})
		p.release(ctx, in.VMID)
	}
	return err
}

func (p *Provisioner) snapshotDelete(ctx context.Context, j *jobs.Job, in Payload) error {
	w, err := p.work(ctx, in.VMID)
	if err != nil {
		return err
	}
	snap, err := p.Store.Q.GetVMSnapshot(ctx, db.GetVMSnapshotParams{ID: in.SnapshotID, VmID: in.VMID})
	if errors.Is(err, pgx.ErrNoRows) {
		p.release(ctx, in.VMID)
		return nil
	} else if err != nil {
		return err
	}
	vmid := int(w.ProxmoxVmid.Int32)
	err = func() error {
		names, err := p.PVE.SnapshotList(ctx, vmid)
		if err != nil {
			return err
		}
		if slices.Contains(names, snap.PveName) {
			upid, err := p.PVE.SnapshotDelete(ctx, vmid, snap.PveName)
			if err != nil {
				return fmt.Errorf("delete snapshot: %w", err)
			}
			if err := p.PVE.WaitTask(ctx, upid); err != nil {
				return fmt.Errorf("delete snapshot task: %w", err)
			}
		}
		return p.Store.Q.DeleteSnapshotRow(ctx, snap.ID)
	}()
	switch {
	case err == nil:
		p.release(ctx, in.VMID)
	case finalFailure(ctx, j):
		_ = p.Store.Q.SetSnapshotStatus(context.WithoutCancel(ctx), db.SetSnapshotStatusParams{ID: snap.ID, Status: "error", LastError: textOf(err.Error())})
		p.release(ctx, in.VMID)
	}
	return err
}

// ---- restore ----

// restore rolls the disk back to a snapshot. Everything written since is lost, which is why the API makes
// the customer confirm. A VM that was running is started again afterwards.
func (p *Provisioner) restore(ctx context.Context, j *jobs.Job, in Payload) error {
	w, err := p.work(ctx, in.VMID)
	if err != nil {
		return err
	}
	snap, err := p.Store.Q.GetVMSnapshot(ctx, db.GetVMSnapshotParams{ID: in.SnapshotID, VmID: in.VMID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && snap.Status != "ready") || !operable(w.State) {
		p.release(ctx, in.VMID)
		return nil
	} else if err != nil {
		return err
	}
	vmid := int(w.ProxmoxVmid.Int32)
	err = func() error {
		st, err := p.PVE.Status(ctx, vmid)
		if err != nil {
			return err
		}
		if !st.Exists {
			return fmt.Errorf("vm %d (vmid %d) is missing from proxmox", w.ID, vmid)
		}
		if st.Running {
			if err := p.run(ctx, vmid, "shutdown"); err != nil {
				return err
			}
		}
		upid, err := p.PVE.SnapshotRollback(ctx, vmid, snap.PveName)
		if err != nil {
			return fmt.Errorf("rollback: %w", err)
		}
		if err := p.PVE.WaitTask(ctx, upid); err != nil {
			return fmt.Errorf("rollback task: %w", err)
		}
		if w.State == "running" {
			if err := p.run(ctx, vmid, "start"); err != nil {
				return err
			}
			return p.waitAgent(ctx, vmid)
		}
		return nil
	}()
	switch {
	case err == nil:
		p.release(ctx, in.VMID)
		_ = p.reapplyFloating(ctx, in.VMID)
	case finalFailure(ctx, j):
		p.Log.Error("restore failed permanently; releasing the VM", "vm_id", w.ID, "err", err)
		p.release(ctx, in.VMID)
		if w.State == "running" {
			if st, e := p.PVE.Status(context.WithoutCancel(ctx), vmid); e == nil && st.Exists && !st.Running {
				_ = p.run(context.WithoutCancel(ctx), vmid, "start")
			}
		}
	}
	return err
}

// ---- rebuild ----

// rebuild reinstalls a VM from a template: the guest and its disk are destroyed and a fresh one is cloned
// into the same VMID with the same IP, plan and hostname. Everything on the old disk, snapshots included, is
// gone. Each attempt starts from a clean slate, so a retry or a crashed worker is safe.
func (p *Provisioner) rebuild(ctx context.Context, j *jobs.Job, in Payload) error {
	w, err := p.Store.Q.GetRebuildWork(ctx, in.VMID)
	if err != nil {
		return err
	}
	if !w.Busy.Valid || w.Busy.String != "rebuilding" || !w.RebuildTemplateID.Valid {
		return nil // nothing claimed: a stale job
	}
	if !operable(w.State) { // suspended or deleted while queued
		p.release(ctx, w.ID)
		return nil
	}
	err = p.doRebuild(ctx, w)
	if err == nil {
		return nil
	}
	if finalFailure(ctx, j) {
		// The old guest is already gone, so the VM cannot be put back: mark it errored (which stops its
		// billing), remove whatever half-built guest exists and leave it to the customer to delete or retry.
		bg := context.WithoutCancel(ctx)
		p.Log.Error("rebuild failed permanently; the VM is marked errored", "vm_id", w.ID, "err", err)
		if e := p.destroyIfPresent(bg, int(w.ProxmoxVmid.Int32)); e != nil {
			p.Log.Error("cleanup after failed rebuild", "vm_id", w.ID, "err", e)
		}
		if e := p.Store.Q.MarkVMError(bg, w.ID); e != nil {
			p.Log.Error("mark error", "vm_id", w.ID, "err", e)
		}
		if e := p.Store.Q.StopBilling(bg, db.StopBillingParams{ID: w.ID, BillingUntil: tsOf(p.now())}); e != nil {
			p.Log.Error("stop billing", "vm_id", w.ID, "err", e)
		}
	}
	return err
}

func (p *Provisioner) doRebuild(ctx context.Context, w db.GetRebuildWorkRow) error {
	vmid := int(w.ProxmoxVmid.Int32)
	if err := p.destroyIfPresent(ctx, vmid); err != nil {
		return fmt.Errorf("remove old guest: %w", err)
	}
	if err := p.Store.Q.DeleteVMSnapshots(ctx, w.ID); err != nil {
		return err
	}
	if err := p.createGuest(ctx, guestSpec{
		Host: w.Host, VMID: vmid, Name: w.Hostname, TemplateVMID: int(w.TemplateVmid), CIUser: w.CiUser, Keys: w.RebuildKeys.String,
		IPv4: w.Ipv4, Gateway: w.Gateway, IPv6: w.Ipv6.String, Extra: p.floatingAddrs(ctx, w.ID), Cores: int(w.Vcpu), MemoryMB: int(w.RamMb), DiskGB: int(w.DiskGb)}); err != nil {
		return err
	}
	err := p.Store.InTx(ctx, func(q *db.Queries, tx pgx.Tx) error {
		n, err := q.FinishRebuild(ctx, w.ID)
		if err != nil {
			return err
		}
		if n == 0 {
			return errors.New("rebuild claim was lost")
		}
		if err := reapplyFloatingTx(ctx, q, tx, w.ID); err != nil {
			return err
		}
		if w.RebuildBootScript.Valid {
			return jobs.EnqueueTx(ctx, tx, JobBootScript, Payload{VMID: w.ID})
		}
		return nil
	})
	if err != nil {
		return err
	}
	p.Log.Info("vm rebuilt", "vm_id", w.ID, "vmid", vmid)
	return nil
}

// ---- boot script ----

const (
	bootScriptTimeout = 10 * time.Minute
	bootScriptPoll    = 2 * time.Second
	bootOutputTail    = 4096
)

// bootScript runs the customer's boot script once, as root, through the guest agent: the script is fed to
// bash on stdin. It runs at most once: a start that never reached the guest is retried, but a script that
// began and was then interrupted is recorded as failed and not run again, because it may not be repeatable.
// The script text is erased afterwards (it can carry a one-time token); the exit code and the tail of its
// output stay. It is for work that finishes: background anything long-lived.
func (p *Provisioner) bootScript(ctx context.Context, j *jobs.Job, in Payload) error {
	w, err := p.Store.Q.GetBootScriptWork(ctx, in.VMID)
	if err != nil {
		return err
	}
	finish := func(status string, exit int, out string) {
		e := p.Store.Q.FinishBootScript(context.WithoutCancel(ctx), db.FinishBootScriptParams{ID: w.ID, BootScriptStatus: status,
			BootScriptExit: pgtype.Int4{Int32: int32(exit), Valid: true}, BootScriptOutput: textOf(tail(out, bootOutputTail))})
		if e != nil {
			p.Log.Error("record boot script outcome", "vm_id", w.ID, "err", e)
		}
	}
	switch w.BootScriptStatus {
	case "running": // an earlier attempt died mid-flight
		finish("failed", -1, "interrupted: the worker stopped while the script was running; it was not run again")
		return nil
	case "pending":
	default:
		return nil
	}
	if w.State != "running" {
		finish("failed", -1, "the VM was not running")
		return nil
	}
	if n, err := p.Store.Q.ClaimBootScript(ctx, w.ID); err != nil {
		return err
	} else if n == 0 {
		return nil
	}

	vmid := int(w.ProxmoxVmid.Int32)
	pid, err := p.PVE.AgentExec(ctx, vmid, []string{"/bin/bash", "-s"}, w.BootScript)
	if err != nil { // never started: safe to retry
		if finalFailure(ctx, j) {
			finish("failed", -1, "could not start the script through the guest agent: "+err.Error())
			return err
		}
		_ = p.Store.Q.RevertBootScript(context.WithoutCancel(ctx), w.ID)
		return fmt.Errorf("start boot script: %w", err)
	}
	deadline := time.Now().Add(bootScriptTimeout)
	for {
		st, err := p.PVE.AgentExecStatus(ctx, vmid, pid)
		switch {
		case err != nil && ctx.Err() == nil && time.Now().Before(deadline):
			// transient: keep asking
		case err != nil:
			finish("failed", -1, "lost contact with the script: "+err.Error())
			return nil
		case st.Exited:
			status := "ok"
			if st.ExitCode != 0 {
				status = "failed"
			}
			finish(status, st.ExitCode, st.Output)
			return nil
		case !time.Now().Before(deadline):
			finish("failed", -1, "the script did not finish within "+bootScriptTimeout.String()+"; scripts must finish, so background long-running work")
			return nil
		}
		select {
		case <-ctx.Done():
			finish("failed", -1, "interrupted while the script was running")
			return nil
		case <-time.After(bootScriptPoll):
		}
	}
}

// tail keeps the last n bytes of s, on a character boundary.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return "…" + s
}
