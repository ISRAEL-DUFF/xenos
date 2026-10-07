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

	// Guest agent commands: AgentExec starts a command inside the guest and returns its pid, AgentExecStatus
	// reports whether it has exited, with its exit code and the end of its output.
	AgentExec(ctx context.Context, vmid int, command []string, stdin string) (int, error)
	AgentExecStatus(ctx context.Context, vmid int, pid int) (ExecStatus, error)

	// Console: Console asks the host for a one-time VNC session on a running guest; DialConsole opens its
	// websocket. The ticket is the VNC password and never leaves the control plane except to the guest's owner.
	Console(ctx context.Context, vmid int) (ConsoleTicket, error)
	DialConsole(ctx context.Context, vmid int, t ConsoleTicket) (ConsoleConn, error)

	// Isolate turns on the guest's firewall with MAC and IP filtering, so it can only send from its own MAC
	// and from the addresses in allowed (plain IPv4/IPv6 addresses). It is idempotent: calling it again with a
	// different list replaces the set, which is how floating addresses are added and removed.
	Isolate(ctx context.Context, vmid int, allowed []string) error
	// Private networks: SetNIC adds or replaces a second NIC on a VLAN of a private bridge (slot 1 or 2, with
	// its cloud-init address), RemoveNIC takes it away, IsolateNIC fixes the addresses that NIC may send from.
	// Changes apply when the guest next starts; Bridges lists the host's bridges (for preflight).
	SetNIC(ctx context.Context, vmid int, p NICParams) error
	RemoveNIC(ctx context.Context, vmid int, slot int) error
	IsolateNIC(ctx context.Context, vmid int, slot int, allowed []string) error
	Bridges(ctx context.Context) ([]string, error)
	// HostFirewall reports whether the firewall is enabled at datacenter and node level; without both, the
	// per-guest filters do nothing.
	HostFirewall(ctx context.Context) (HostFirewallState, error)

	// Monitoring.
	Guests(ctx context.Context) ([]Guest, error)
	StoragePool(ctx context.Context, storage string) (Usage, error)
	NodeInfo(ctx context.Context) (NodeInfo, error)
}

// NICParams describes a private-network NIC.
type NICParams struct {
	Slot     int    // 1 or 2: net1/ipconfig1 or net2/ipconfig2
	Bridge   string // the private bridge, e.g. vmbr1
	VLAN     int
	IPConfig string // e.g. "ip=10.64.0.5/24" (no gateway: private networks are not routed)
}

// HostFirewallState is whether the Proxmox firewall is switched on above the guests.
type HostFirewallState struct{ Cluster, Node bool }

// ConsoleTicket is a one-time VNC session on the host.
type ConsoleTicket struct {
	Port   int
	Ticket string
}

// ConsoleConn is a binary message stream to a guest's VNC server (RFB over a websocket).
type ConsoleConn interface {
	ReadMessage() ([]byte, error)
	WriteMessage([]byte) error
	Close() error
}

// ExecStatus is the state of a command started with AgentExec.
type ExecStatus struct {
	Exited   bool
	ExitCode int
	Output   string // stdout then stderr
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
