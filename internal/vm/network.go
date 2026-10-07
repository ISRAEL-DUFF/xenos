package vm

import (
	"context"
	"fmt"
	"net/netip"

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
		p.abandonNetwork(ctx, w.ID, in.NetworkID, attach)
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
		p.abandonNetwork(ctx, w.ID, in.NetworkID, attach)
		p.release(ctx, w.ID)
		if w.State == "running" { // best effort: do not leave the customer's VM powered off
			vmid := int(w.ProxmoxVmid.Int32)
			if st, e := p.PVE.Status(context.WithoutCancel(ctx), vmid); e == nil && st.Exists && !st.Running {
				_ = p.run(context.WithoutCancel(ctx), vmid, "start")
			}
		}
	}
	return err
}

// abandonNetwork undoes the recorded intent when the change cannot be made.
func (p *Provisioner) abandonNetwork(ctx context.Context, vmID, networkID int64, attach bool) {
	ctx = context.WithoutCancel(ctx)
	q := p.Store.Q
	if attach {
		if rows, err := q.ListVMPrivateIPs(ctx, vmID); err == nil {
			for _, r := range rows {
				if r.NetworkID == networkID && r.State == "attaching" {
					_ = p.PVE.RemoveNIC(ctx, p.vmidOf(ctx, vmID), int(r.Slot)) // may not have been added yet
				}
			}
		}
		_ = q.DeleteVMPrivateIP(ctx, db.DeleteVMPrivateIPParams{VmID: vmID, NetworkID: networkID})
		_ = q.UnpinEmptyNetwork(ctx, networkID)
		return
	}
	_ = q.SetVMPrivateIPState(ctx, db.SetVMPrivateIPStateParams{VmID: vmID, NetworkID: networkID, State: "attached"})
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
	return p.Store.Q.UnpinEmptyNetwork(ctx, networkID)
}

type privateRow struct {
	Slot, VLAN           int
	Address, Cidr, State string
}
