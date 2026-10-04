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
}

// VMStatus is a guest's presence and power state on the node.
type VMStatus struct {
	Exists  bool
	Running bool
}

var _ API = (*Client)(nil)
