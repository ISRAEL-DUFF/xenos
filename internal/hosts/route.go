package hosts

import (
	"bytes"
	"context"
	"io"

	"github.com/israel-duff/xenos/internal/proxmox"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// The proxmox.API methods below route by VMID. The four host-wide calls at the end have no VMID and answer for
// the primary host only: callers that care about every host (monitor, preflight, admin) iterate All().

func (s *Set) Clone(ctx context.Context, p proxmox.CloneParams) (string, error) {
	a, err := s.For(ctx, p.NewID)
	if err != nil {
		return "", err
	}
	return a.Clone(ctx, p)
}

func (s *Set) Configure(ctx context.Context, vmid int, p proxmox.ConfigParams) error {
	a, err := s.For(ctx, vmid)
	if err != nil {
		return err
	}
	return a.Configure(ctx, vmid, p)
}

func (s *Set) ResizeDisk(ctx context.Context, vmid int, disk string, sizeGB int) error {
	a, err := s.For(ctx, vmid)
	if err != nil {
		return err
	}
	return a.ResizeDisk(ctx, vmid, disk, sizeGB)
}

func (s *Set) Power(ctx context.Context, vmid int, action string) (string, error) {
	a, err := s.For(ctx, vmid)
	if err != nil {
		return "", err
	}
	return a.Power(ctx, vmid, action)
}

func (s *Set) Destroy(ctx context.Context, vmid int) (string, error) {
	a, err := s.For(ctx, vmid)
	if err != nil {
		return "", err
	}
	return a.Destroy(ctx, vmid)
}

func (s *Set) AgentPing(ctx context.Context, vmid int) error {
	a, err := s.For(ctx, vmid)
	if err != nil {
		return err
	}
	return a.AgentPing(ctx, vmid)
}

// WaitTask routes by the node named inside the UPID ("UPID:<node>:..."): each host has its own task list.
func (s *Set) WaitTask(ctx context.Context, upid string) error {
	return s.byUPID(upid).WaitTask(ctx, upid)
}

func (s *Set) Status(ctx context.Context, vmid int) (proxmox.VMStatus, error) {
	a, err := s.For(ctx, vmid)
	if err != nil {
		return proxmox.VMStatus{}, err
	}
	return a.Status(ctx, vmid)
}

func (s *Set) SetResources(ctx context.Context, vmid int, cores, memoryMB int) error {
	a, err := s.For(ctx, vmid)
	if err != nil {
		return err
	}
	return a.SetResources(ctx, vmid, cores, memoryMB)
}

func (s *Set) DiskSizeGB(ctx context.Context, vmid int, disk string) (int, error) {
	a, err := s.For(ctx, vmid)
	if err != nil {
		return 0, err
	}
	return a.DiskSizeGB(ctx, vmid, disk)
}

func (s *Set) SnapshotCreate(ctx context.Context, vmid int, name string) (string, error) {
	a, err := s.For(ctx, vmid)
	if err != nil {
		return "", err
	}
	return a.SnapshotCreate(ctx, vmid, name)
}

func (s *Set) SnapshotList(ctx context.Context, vmid int) ([]string, error) {
	a, err := s.For(ctx, vmid)
	if err != nil {
		return nil, err
	}
	return a.SnapshotList(ctx, vmid)
}

func (s *Set) SnapshotRollback(ctx context.Context, vmid int, name string) (string, error) {
	a, err := s.For(ctx, vmid)
	if err != nil {
		return "", err
	}
	return a.SnapshotRollback(ctx, vmid, name)
}

func (s *Set) SnapshotDelete(ctx context.Context, vmid int, name string) (string, error) {
	a, err := s.For(ctx, vmid)
	if err != nil {
		return "", err
	}
	return a.SnapshotDelete(ctx, vmid, name)
}

func (s *Set) AgentExec(ctx context.Context, vmid int, command []string, stdin string) (int, error) {
	a, err := s.For(ctx, vmid)
	if err != nil {
		return 0, err
	}
	return a.AgentExec(ctx, vmid, command, stdin)
}

func (s *Set) AgentExecStatus(ctx context.Context, vmid int, pid int) (proxmox.ExecStatus, error) {
	a, err := s.For(ctx, vmid)
	if err != nil {
		return proxmox.ExecStatus{}, err
	}
	return a.AgentExecStatus(ctx, vmid, pid)
}

func (s *Set) Console(ctx context.Context, vmid int) (proxmox.ConsoleTicket, error) {
	a, err := s.For(ctx, vmid)
	if err != nil {
		return proxmox.ConsoleTicket{}, err
	}
	return a.Console(ctx, vmid)
}

func (s *Set) DialConsole(ctx context.Context, vmid int, t proxmox.ConsoleTicket) (proxmox.ConsoleConn, error) {
	a, err := s.For(ctx, vmid)
	if err != nil {
		return nil, err
	}
	return a.DialConsole(ctx, vmid, t)
}

func (s *Set) Isolate(ctx context.Context, vmid int, allowed []string) error {
	a, err := s.For(ctx, vmid)
	if err != nil {
		return err
	}
	return a.Isolate(ctx, vmid, allowed)
}

// ---- host-wide calls: the primary host only ----

func (s *Set) HostFirewall(ctx context.Context) (proxmox.HostFirewallState, error) {
	return s.Primary().API.HostFirewall(ctx)
}
func (s *Set) Guests(ctx context.Context) ([]proxmox.Guest, error) {
	return s.Primary().API.Guests(ctx)
}
func (s *Set) StoragePool(ctx context.Context, storage string) (proxmox.Usage, error) {
	return s.Primary().API.StoragePool(ctx, storage)
}
func (s *Set) NodeInfo(ctx context.Context) (proxmox.NodeInfo, error) {
	return s.Primary().API.NodeInfo(ctx)
}
