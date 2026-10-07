package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/israel-duff/xenos/internal/jobs"
	"github.com/israel-duff/xenos/internal/store/db"
)

const (
	floatingExecTimeout = 45 * time.Second
	floatingLockBase    = 0x464c4f4154 << 20 // "FLOAT"; the floating IP id is added
)

// floating makes the world match what the account asked for: the firewall set of the VM the address points at
// holds it, that guest has it on its interface, and the VM it used to point at has lost both. It is idempotent,
// so it also runs after a start or rebuild and on a schedule.
func (p *Provisioner) floating(ctx context.Context, j *jobs.Job) error {
	var in Payload
	if err := json.Unmarshal(j.Payload, &in); err != nil {
		return fmt.Errorf("bad payload: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute) // a hung host must not hold a worker for ever
	defer cancel()
	return p.ReconcileFloating(ctx, in.FloatingID)
}

// ReconcileFloating is the body of the floating job.
func (p *Provisioner) ReconcileFloating(ctx context.Context, id int64) error {
	// One reconcile per address at a time, across workers. The transaction exists only to hold the lock.
	return p.Store.InTx(ctx, func(q *db.Queries, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(floatingLockBase)+id); err != nil {
			return err
		}
		f, err := q.GetFloatingForWork(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		} else if err != nil {
			return err
		}
		addr, err := netip.ParseAddr(f.Address)
		if err != nil {
			return fmt.Errorf("floating ip %d: bad address %q", id, f.Address)
		}

		// Take it off the VM it was configured on, when that is no longer where it points.
		if f.AppliedVmID.Valid && (!f.VmID.Valid || f.AppliedVmID.Int64 != f.VmID.Int64) {
			// A failed cleanup fails the job (it retries, and the monitor re-queues it): the address must not be
			// handed to anyone while an old guest can still send from it.
			if err := p.unconfigure(ctx, q, f.AppliedVmID.Int64, addr); err != nil {
				return err
			}
			if err := q.MarkFloatingApplied(ctx, db.MarkFloatingAppliedParams{ID: id}); err != nil {
				return err
			}
		}
		if !f.VmID.Valid {
			return nil
		}

		w, err := q.GetVMForWork(ctx, f.VmID.Int64)
		if err != nil {
			return err
		}
		if w.State == "deleted" || w.State == "deleting" || !w.ProxmoxVmid.Valid {
			return nil
		}
		vmid := int(w.ProxmoxVmid.Int32)
		st, err := p.PVE.Status(ctx, vmid)
		if err != nil {
			return err
		}
		if !st.Exists {
			return fmt.Errorf("vm %d is missing from proxmox", w.ID)
		}
		allowed, err := p.allowedFor(ctx, q, w)
		if err != nil {
			return err
		}
		if err := p.PVE.Isolate(ctx, vmid, allowed); err != nil {
			return fmt.Errorf("firewall: %w", err)
		}
		if st.Running {
			if out, err := p.execShell(ctx, vmid, floatingScript(addr, true)); err != nil {
				return fmt.Errorf("configure %s on vm %d: %w: %s", addr, w.ID, err, out)
			}
		}
		// A stopped guest gets the address when it starts (the start hook queues this job again).
		return q.MarkFloatingApplied(ctx, db.MarkFloatingAppliedParams{ID: id, AppliedVmID: pgtype.Int8{Int64: w.ID, Valid: true}})
	})
}

// unconfigure removes the address from a VM it no longer points at. A VM that is gone (deleted, destroyed on the
// host) has nothing to clean. Taking the address out of the VM's firewall set is mandatory: once it is gone the
// address is filtered even if the guest still holds it, so a guest that cannot be reached is tolerated after that.
func (p *Provisioner) unconfigure(ctx context.Context, q *db.Queries, vmID int64, addr netip.Addr) error {
	w, err := q.GetVMForWork(ctx, vmID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (w.State == "deleted" || !w.ProxmoxVmid.Valid)) {
		return nil
	} else if err != nil {
		return err
	}
	vmid := int(w.ProxmoxVmid.Int32)
	st, err := p.PVE.Status(ctx, vmid)
	if err != nil {
		return fmt.Errorf("check vm %d before removing %s: %w", vmID, addr, err)
	}
	if !st.Exists {
		return nil
	}
	allowed, err := p.allowedFor(ctx, q, w)
	if err != nil {
		return err
	}
	if err := p.PVE.Isolate(ctx, vmid, allowed); err != nil {
		return fmt.Errorf("remove %s from vm %d's firewall set: %w", addr, vmID, err)
	}
	if st.Running {
		if out, err := p.execShell(ctx, vmid, floatingScript(addr, false)); err != nil {
			p.Log.Warn("floating ip: could not remove the address from the old vm's interface (it is filtered)", "vm_id", vmID, "err", err, "output", out)
		}
	}
	return nil
}

// allowedFor is every address the VM may send from: its own, plus the floating IPs that point at it.
func (p *Provisioner) allowedFor(ctx context.Context, q *db.Queries, w db.GetVMForWorkRow) ([]string, error) {
	g := guestSpec{IPv4: w.Ipv4, IPv6: w.Ipv6.String}
	rows, err := q.FloatingForVM(ctx, pgtype.Int8{Int64: w.ID, Valid: true})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		g.Extra = append(g.Extra, r.Address)
	}
	return g.allowedAddrs(), nil
}

// floatingAddrs lists the floating IPs pointing at a VM (best effort: a read error just means none are added now,
// and the reconcile that follows every rebuild puts them back).
func (p *Provisioner) floatingAddrs(ctx context.Context, vmID int64) []string {
	rows, err := p.Store.Q.FloatingForVM(ctx, pgtype.Int8{Int64: vmID, Valid: true})
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Address)
	}
	return out
}

// reapplyFloating queues a reconcile for every floating IP pointing at the VM.
func (p *Provisioner) reapplyFloating(ctx context.Context, vmID int64) error {
	rows, err := p.Store.Q.FloatingForVM(ctx, pgtype.Int8{Int64: vmID, Valid: true})
	if err != nil {
		return err
	}
	for _, r := range rows {
		if err := jobs.EnqueueTx(ctx, p.Store.Pool, JobFloating, Payload{FloatingID: r.ID}); err != nil {
			return err
		}
	}
	return nil
}

func reapplyFloatingTx(ctx context.Context, q *db.Queries, tx pgx.Tx, vmID int64) error {
	rows, err := q.FloatingForVM(ctx, pgtype.Int8{Int64: vmID, Valid: true})
	if err != nil {
		return err
	}
	for _, r := range rows {
		if err := jobs.EnqueueTx(ctx, tx, JobFloating, Payload{FloatingID: r.ID}); err != nil {
			return err
		}
	}
	return nil
}

// floatingScript adds or removes the address on the guest's default interface. The address has been parsed as an
// IP, so it contains nothing a shell would interpret. Adding announces the address (gratuitous ARP, or a ping
// to the gateway as a fallback) so the upstream switch learns where it lives now.
func floatingScript(addr netip.Addr, add bool) string {
	fam, bits := "-4", 32
	if addr.Is6() {
		fam, bits = "-6", 128
	}
	var b strings.Builder
	fmt.Fprintf(&b, "dev=$(ip %s -o route show default | awk '{for(i=1;i<=NF;i++) if($i==\"dev\"){print $(i+1); exit}}')\n", fam)
	b.WriteString("[ -n \"$dev\" ] || { echo 'no default interface' >&2; exit 1; }\n")
	if !add {
		fmt.Fprintf(&b, "ip %s addr del %s/%d dev \"$dev\" 2>/dev/null || true\n", fam, addr, bits)
		return b.String()
	}
	fmt.Fprintf(&b, "ip %s addr replace %s/%d dev \"$dev\"\n", fam, addr, bits)
	if addr.Is4() {
		fmt.Fprintf(&b, "gw=$(ip -4 route show default | awk '{print $3; exit}')\n")
		fmt.Fprintf(&b, "(arping -c 2 -U -I \"$dev\" %s || ping -c 1 -W 1 -I %s \"$gw\") >/dev/null 2>&1 || true\n", addr, addr)
	} else {
		fmt.Fprintf(&b, "(ndisc6 -q %s \"$dev\" || ping -6 -c 1 -W 1 -I %s ff02::2%%\"$dev\") >/dev/null 2>&1 || true\n", addr, addr)
	}
	return b.String()
}

// execShell runs a script in the guest and waits for it. It returns the output either way.
func (p *Provisioner) execShell(ctx context.Context, vmid int, script string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, floatingExecTimeout)
	defer cancel()
	pid, err := p.PVE.AgentExec(ctx, vmid, []string{"/bin/sh", "-s"}, script)
	if err != nil {
		return "", err
	}
	for {
		st, err := p.PVE.AgentExecStatus(ctx, vmid, pid)
		if err == nil && st.Exited {
			if st.ExitCode != 0 {
				return st.Output, fmt.Errorf("exit %d", st.ExitCode)
			}
			return st.Output, nil
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("the command did not finish: %w", ctx.Err())
		case <-time.After(p.Cfg.PollInterval):
		}
	}
}

// reisolate puts the guest's anti-spoof set back after a snapshot rollback (the rollback restores the VM's
// configuration from when the snapshot was taken).
func (p *Provisioner) reisolate(ctx context.Context, vmID int64) {
	w, err := p.work(ctx, vmID)
	if err != nil || !w.ProxmoxVmid.Valid {
		return
	}
	allowed, err := p.allowedFor(ctx, p.Store.Q, w)
	if err == nil {
		err = p.PVE.Isolate(ctx, int(w.ProxmoxVmid.Int32), allowed)
	}
	if err != nil {
		p.Log.Warn("could not re-apply the guest firewall after a restore", "vm_id", vmID, "err", err)
	}
}
