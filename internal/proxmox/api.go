package proxmox

import "context"

// API is what the worker needs from Proxmox. *Client implements it against a
// real host and *Fake in memory, so provisioning logic is testable without one.
type API interface {
	Clone(ctx context.Context, p CloneParams) (upid string, err error)
	Configure(ctx context.Context, vmid int, p ConfigParams) error
	ResizeDisk(ctx context.Context, vmid int, disk string, sizeGB int) error
	// Power is one of start, stop, shutdown (graceful, forced after a timeout) or reboot.
	Power(ctx context.Context, vmid int, action string) (upid string, err error)
	Destroy(ctx context.Context, vmid int) (upid string, err error)
	AgentPing(ctx context.Context, vmid int) error
	WaitTask(ctx context.Context, upid string) error
	Status(ctx context.Context, vmid int) (VMStatus, error)

	// Resizing: SetResources changes cores and memory (effective after a full stop and start);
	// DiskSizeGB reads a disk's current size so a disk grow can be retried safely.
	SetResources(ctx context.Context, vmid int, cores, memoryMB int) error
	DiskSizeGB(ctx context.Context, vmid int, disk string) (int, error)

	// Snapshots (disk only, no RAM state). Names are ours; "current" is Proxmox's own pseudo entry and is
	// never returned. Delete and rollback return the task UPID.
	SnapshotCreate(ctx context.Context, vmid int, name string) (string, error)
	SnapshotList(ctx context.Context, vmid int) ([]string, error)
	SnapshotRollback(ctx context.Context, vmid int, name string) (string, error)
	SnapshotDelete(ctx context.Context, vmid int, name string) (string, error)

	// Monitoring.
	Guests(ctx context.Context) ([]Guest, error)
	StoragePool(ctx context.Context, storage string) (Usage, error)
	NodeInfo(ctx context.Context) (NodeInfo, error)
}

// VMStatus is a guest's presence and power state on the node.
type VMStatus struct {
	Exists  bool
	Running bool
}

// Guest is one VM as the node reports it.
type Guest struct {
	VMID    int
	Running bool
	CPU     float64 // fraction of the guest's allotted CPU in use, 0..1
}

// NodeInfo is the host's physical capacity.
type NodeInfo struct {
	CPUs     int   // logical CPUs
	MemTotal int64 // bytes
}

// Usage is used and total bytes of a storage pool.
type Usage struct{ Used, Total int64 }

func (u Usage) Fraction() float64 {
	if u.Total <= 0 {
		return 0
	}
	return float64(u.Used) / float64(u.Total)
}

var _ API = (*Client)(nil)
