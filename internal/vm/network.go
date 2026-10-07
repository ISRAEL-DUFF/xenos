package vm

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenos/internal/jobs"
	"github.com/israel-duff/xenos/internal/store/db"
)

// privateNICs lists the private interfaces a VM has settled (not those still being attached or detached).
func (p *Provisioner) privateNICs(ctx context.Context, vmID int64) ([]PrivateNIC, error) {
	rows, err := p.Store.Q.ListVMPrivateIPs(ctx, vmID)
	if err != nil {
		return nil, err
	}
	var out []PrivateNIC
	for _, r := range rows {
		if r.State != "attached" {
			continue
		}
		pre, err := netip.ParsePrefix(r.Cidr)
		if err != nil {
			return nil, err
		}
		out = append(out, PrivateNIC{Slot: int(r.Slot), VLAN: int(r.VlanID), Address: r.Address, PrefixLen: pre.Bits()})
	}
	return out, nil
}

// Actions of a JobNetwork.
const (
	NetAttach = "attach"
	NetDetach = "detach"
)

// network attaches or detaches one private network: the guest is stopped, its second NIC is changed, and it is
// started again so cloud-init applies the new address. Every step checks the host first, so a retry from any point
// is safe. The API has already recorded the intent (a vm_private_ips row in state attaching or detaching) and
// claimed the VM; this releases the claim when it ends.
func (p *Provisioner) network(ctx context.Context, j *jobs.Job, in Payload) error {
	w, err := p.Store.Q.GetResizeWork(ctx, in.VMID)
	if err != nil {
		return err
	}
	if !w.Busy.Valid || w.Busy.String != "networking" {
		return nil // nothing claimed: a stale job
	}
	attach := in.Action == NetAttach
	if !operable(w.State) {
		_ = p.abandonNetwork(ctx, w.ID, in.NetworkID, attach)
		p.release(ctx, w.ID)
		return nil
	}
	err = p.doNetwork(ctx, w.ID, w.Host, int(w.ProxmoxVmid.Int32), w.State == "running", in.NetworkID, attach)
	if err == nil {
		p.release(ctx, w.ID)
		return nil
	}
	if finalFailure(ctx, j) {
		p.Log.Error("private network change failed permanently; releasing the VM", "vm_id", w.ID, "err", err)
		clean := p.abandonNetwork(ctx, w.ID, in.NetworkID, attach)
		p.release(ctx, w.ID)
		if clean && w.State == "running" { // best effort: do not leave the customer's VM powered off
			vmid := int(w.ProxmoxVmid.Int32)
			if st, e := p.PVE.Status(context.WithoutCancel(ctx), vmid); e == nil && st.Exists && !st.Running {
				_ = p.run(context.WithoutCancel(ctx), vmid, "start")
			}
		}
	}
	return err
}

// abandonNetwork undoes the recorded intent when the change cannot be made. It reports whether the host is known
// to be clean. The record is only forgotten (freeing the address and VLAN for others) once the NIC is confirmed gone:
// otherwise the row stays, in state "stuck", so nothing else can be given that VLAN while a guest may still carry it.
func (p *Provisioner) abandonNetwork(ctx context.Context, vmID, networkID int64, attach bool) bool {
	ctx = context.WithoutCancel(ctx)
	q := p.Store.Q
	if !attach {
		_ = q.SetVMPrivateIPState(ctx, db.SetVMPrivateIPStateParams{VmID: vmID, NetworkID: networkID, State: "attached"})
		return true
	}
	rows, err := q.ListVMPrivateIPs(ctx, vmID)
	if err != nil {
		return false
	}
	vmid := p.vmidOf(ctx, vmID)
	for _, r := range rows {
		if r.NetworkID != networkID || r.State != "attaching" {
			continue
		}
		clean := vmid != 0 && p.PVE.IsolateNIC(ctx, vmid, int(r.Slot), nil) == nil && p.PVE.RemoveNIC(ctx, vmid, int(r.Slot)) == nil
		if !clean {
			p.Log.Error("could not take a half-added private NIC off the guest; keeping its VLAN reserved", "vm_id", vmID, "network_id", networkID)
			_ = q.SetVMPrivateIPState(ctx, db.SetVMPrivateIPStateParams{VmID: vmID, NetworkID: networkID, State: "stuck"})
			return false
		}
	}
	_ = q.DeleteVMPrivateIP(ctx, db.DeleteVMPrivateIPParams{VmID: vmID, NetworkID: networkID})
	_ = p.unpinNetwork(ctx, networkID)
	return true
}

// unpinNetwork clears a network's host once it has no members, under the network's row lock so a join that is
// committing at the same moment is not overwritten.
func (p *Provisioner) unpinNetwork(ctx context.Context, networkID int64) error {
	return p.Store.InTx(ctx, func(q *db.Queries, tx pgx.Tx) error {
		if _, err := q.LockNetworkByID(ctx, networkID); err != nil {
			return err
		}
		return q.UnpinEmptyNetwork(ctx, networkID)
	})
}

// reconcileNICs makes the guest's private NICs and their filters match the database: every attached membership is
// (re)built, every other slot is emptied and removed.
func (p *Provisioner) reconcileNICs(ctx context.Context, vmID int64, host string, vmid int) error {
	nics, err := p.privateNICs(ctx, vmID)
	if err != nil {
		return err
	}
	want := map[int]PrivateNIC{}
	for _, n := range nics {
		want[n.Slot] = n
	}
	for slot := 1; slot <= 2; slot++ {
		if n, ok := want[slot]; ok {
			if err := p.PVE.SetNIC(ctx, vmid, n.params(p.cfgFor(host).privateBridge())); err != nil {
				return err
			}
			if err := p.PVE.IsolateNIC(ctx, vmid, slot, []string{n.Address}); err != nil {
				return err
			}
			continue
		}
		if err := p.PVE.IsolateNIC(ctx, vmid, slot, nil); err != nil {
			return err
		}
		if err := p.PVE.RemoveNIC(ctx, vmid, slot); err != nil {
			return err
		}
	}
	return nil
}

func (p *Provisioner) vmidOf(ctx context.Context, vmID int64) int {
	w, err := p.work(ctx, vmID)
	if err != nil || !w.ProxmoxVmid.Valid {
		return 0
	}
	return int(w.ProxmoxVmid.Int32)
}

func (p *Provisioner) doNetwork(ctx context.Context, vmID int64, host string, vmid int, wasRunning bool, networkID int64, attach bool) error {
	rows, err := p.Store.Q.ListVMPrivateIPs(ctx, vmID)
	if err != nil {
		return err
	}
	var row *privateRow
	for i := range rows {
		if rows[i].NetworkID == networkID {
			row = &privateRow{Slot: int(rows[i].Slot), VLAN: int(rows[i].VlanID), Address: rows[i].Address, Cidr: rows[i].Cidr, State: rows[i].State}
		}
	}
	if row == nil {
		return nil // already finished (a retry after the last step)
	}
	if attach && row.State == "attached" {
		return nil // done on an earlier attempt
	}
	st, err := p.PVE.Status(ctx, vmid)
	if err != nil {
		return err
	}
	if !st.Exists {
		return fmt.Errorf("vm %d (vmid %d) is missing from proxmox", vmID, vmid)
	}
	if st.Running { // NIC changes apply with the next start, and the guest re-reads its network config then
		if err := p.run(ctx, vmid, "shutdown"); err != nil {
			return err
		}
	}
	if attach {
		pre, err := netip.ParsePrefix(row.Cidr)
		if err != nil {
			return err
		}
		nic := PrivateNIC{Slot: row.Slot, VLAN: row.VLAN, Address: row.Address, PrefixLen: pre.Bits()}
		if err := p.PVE.SetNIC(ctx, vmid, nic.params(p.cfgFor(host).privateBridge())); err != nil {
			return fmt.Errorf("set nic: %w", err)
		}
		if err := p.PVE.IsolateNIC(ctx, vmid, row.Slot, []string{row.Address}); err != nil {
			return fmt.Errorf("nic firewall: %w", err)
		}
	} else {
		if err := p.PVE.IsolateNIC(ctx, vmid, row.Slot, nil); err != nil {
			return fmt.Errorf("nic firewall: %w", err)
		}
		if err := p.PVE.RemoveNIC(ctx, vmid, row.Slot); err != nil {
			return fmt.Errorf("remove nic: %w", err)
		}
	}
	if wasRunning {
		if err := p.run(ctx, vmid, "start"); err != nil {
			return err
		}
		if err := p.waitAgent(ctx, vmid); err != nil {
			return err
		}
	}
	if attach {
		return p.Store.Q.SetVMPrivateIPState(ctx, db.SetVMPrivateIPStateParams{VmID: vmID, NetworkID: networkID, State: "attached"})
	}
	if err := p.Store.Q.DeleteVMPrivateIP(ctx, db.DeleteVMPrivateIPParams{VmID: vmID, NetworkID: networkID}); err != nil {
		return err
	}
	return p.unpinNetwork(ctx, networkID)
}

type privateRow struct {
	Slot, VLAN           int
	Address, Cidr, State string
}
