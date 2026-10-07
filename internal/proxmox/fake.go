package proxmox

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Fake is an in-memory Proxmox for tests and local development. Tasks finish
// instantly. Set Fail to inject an error for a named operation.
type Fake struct {
	mu   sync.Mutex
	VMs  map[int]*FakeVM
	Fail map[string]error // key: "clone", "configure", "resize", "start", "agent", "destroy", ...
	// Calls records operations in order, e.g. "clone:100", "start:100".
	Calls []string
	// Monitoring knobs.
	CPU      map[int]float64 // vmid -> CPU fraction reported by Guests
	Pool     Usage
	MemTotal int64
	CPUs     int
	// ExecFn, if set, decides what a guest command does (exit code and output); the default exits 0.
	ExecFn func(vmid int, command []string, stdin string) (int, string)
	// Execs records every guest command started: "vmid\x00command\x00stdin".
	Execs []ExecCall
	// Consoles lists the console connections opened so far.
	Consoles []*FakeConsole
	// Bridge names the fake host has (Bridges); default vmbr0 and vmbr1.
	BridgeNames []string
	// AfterRollback, if set, runs on a guest after a snapshot rollback (Proxmox restores the VM config of that moment).
	AfterRollback func(vm *FakeVM)
	// HostFirewallOff makes HostFirewall report the firewall as disabled.
	HostFirewallOff bool
	// BeforeOp, if set, runs before each operation (used to simulate a crash by panicking).
	BeforeOp func(op string, vmid int)
}

type FakeVM struct {
	ID       int
	Name     string
	Running  bool
	Config   ConfigParams
	DiskGB   int
	Template int
	// Snapshots lists snapshot names in creation order.
	Snapshots []string
	Cores     int
	MemoryMB  int
	// Isolated is true once the guest firewall (MAC and IP filtering) is on; Allowed is the ipfilter set.
	Isolated bool
	Allowed  []string
	// NICs holds the private NICs by slot; NICAllowed their ipfilter sets.
	NICs       map[int]NICParams
	NICAllowed map[int][]string
}

func NewFake() *Fake {
	return &Fake{VMs: map[int]*FakeVM{}, Fail: map[string]error{}, CPU: map[int]float64{}, MemTotal: 64 << 30, CPUs: 16, Pool: Usage{Total: 1 << 40}}
}

var ErrFakeNotFound = errors.New("proxmox fake: no such vm")

func (f *Fake) op(name string, vmid int) error {
	if f.BeforeOp != nil {
		f.mu.Unlock()
		func() {
			defer f.mu.Lock() // re-lock even if BeforeOp panics to simulate a crash
			f.BeforeOp(name, vmid)
		}()
	}
	f.Calls = append(f.Calls, fmt.Sprintf("%s:%d", name, vmid))
	return f.Fail[name]
}

func (f *Fake) Clone(_ context.Context, p CloneParams) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("clone", p.NewID); err != nil {
		return "", err
	}
	if _, ok := f.VMs[p.NewID]; ok {
		return "", fmt.Errorf("proxmox fake: vmid %d already exists", p.NewID)
	}
	f.VMs[p.NewID] = &FakeVM{ID: p.NewID, Name: p.Name, Template: p.TemplateID}
	return "UPID:clone", nil
}

func (f *Fake) Configure(_ context.Context, vmid int, p ConfigParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("configure", vmid); err != nil {
		return err
	}
	vm, ok := f.VMs[vmid]
	if !ok {
		return ErrFakeNotFound
	}
	vm.Config = p
	return nil
}

func (f *Fake) ResizeDisk(_ context.Context, vmid int, _ string, sizeGB int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("resize", vmid); err != nil {
		return err
	}
	vm, ok := f.VMs[vmid]
	if !ok {
		return ErrFakeNotFound
	}
	vm.DiskGB = sizeGB
	return nil
}

func (f *Fake) Power(_ context.Context, vmid int, action string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op(action, vmid); err != nil {
		return "", err
	}
	vm, ok := f.VMs[vmid]
	if !ok {
		return "", ErrFakeNotFound
	}
	switch action {
	case "start", "reboot":
		vm.Running = true
	case "stop", "shutdown":
		vm.Running = false
	default:
		return "", fmt.Errorf("proxmox fake: unknown action %q", action)
	}
	return "UPID:" + action, nil
}

func (f *Fake) Destroy(_ context.Context, vmid int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("destroy", vmid); err != nil {
		return "", err
	}
	vm, ok := f.VMs[vmid]
	if !ok {
		return "", ErrFakeNotFound
	}
	if vm.Running {
		return "", errors.New("proxmox fake: cannot destroy a running vm")
	}
	delete(f.VMs, vmid)
	return "UPID:destroy", nil
}

func (f *Fake) AgentPing(_ context.Context, vmid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("agent", vmid); err != nil {
		return err
	}
	if vm, ok := f.VMs[vmid]; !ok || !vm.Running {
		return errors.New("proxmox fake: guest agent not running")
	}
	return nil
}

func (f *Fake) WaitTask(context.Context, string) error { return nil }

func (f *Fake) Status(_ context.Context, vmid int) (VMStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	vm, ok := f.VMs[vmid]
	if !ok {
		return VMStatus{}, nil
	}
	return VMStatus{Exists: true, Running: vm.Running}, nil
}

// IDs returns the current VM IDs (test helper).
func (f *Fake) IDs() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []int{}
	for id := range f.VMs {
		out = append(out, id)
	}
	return out
}

func (f *Fake) Guests(context.Context) ([]Guest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.Fail["guests"]; err != nil {
		return nil, err
	}
	out := []Guest{}
	for id, vm := range f.VMs {
		out = append(out, Guest{VMID: id, Running: vm.Running, CPU: f.CPU[id]})
	}
	return out, nil
}

func (f *Fake) StoragePool(context.Context, string) (Usage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Pool, f.Fail["pool"]
}

func (f *Fake) NodeInfo(context.Context) (NodeInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return NodeInfo{CPUs: f.CPUs, MemTotal: f.MemTotal}, f.Fail["memory"]
}

func (f *Fake) SetResources(_ context.Context, vmid int, cores, memoryMB int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("setresources", vmid); err != nil {
		return err
	}
	vm, ok := f.VMs[vmid]
	if !ok {
		return ErrFakeNotFound
	}
	vm.Cores, vm.MemoryMB = cores, memoryMB
	return nil
}

func (f *Fake) DiskSizeGB(_ context.Context, vmid int, _ string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	vm, ok := f.VMs[vmid]
	if !ok {
		return 0, ErrFakeNotFound
	}
	return vm.DiskGB, nil
}

func (f *Fake) SnapshotCreate(_ context.Context, vmid int, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("snapshot", vmid); err != nil {
		return "", err
	}
	vm, ok := f.VMs[vmid]
	if !ok {
		return "", ErrFakeNotFound
	}
	for _, s := range vm.Snapshots {
		if s == name {
			return "", fmt.Errorf("proxmox fake: snapshot %q already exists", name)
		}
	}
	vm.Snapshots = append(vm.Snapshots, name)
	return "UPID:snapshot", nil
}

func (f *Fake) SnapshotList(_ context.Context, vmid int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	vm, ok := f.VMs[vmid]
	if !ok {
		return nil, ErrFakeNotFound
	}
	return append([]string(nil), vm.Snapshots...), nil
}

func (f *Fake) SnapshotRollback(_ context.Context, vmid int, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("rollback", vmid); err != nil {
		return "", err
	}
	vm, ok := f.VMs[vmid]
	if !ok {
		return "", ErrFakeNotFound
	}
	if vm.Running {
		return "", errors.New("proxmox fake: rollback needs a stopped vm")
	}
	for _, s := range vm.Snapshots {
		if s == name {
			if f.AfterRollback != nil {
				f.AfterRollback(vm)
			}
			return "UPID:rollback", nil
		}
	}
	return "", fmt.Errorf("proxmox fake: no snapshot %q", name)
}

func (f *Fake) SnapshotDelete(_ context.Context, vmid int, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("snapshot-delete", vmid); err != nil {
		return "", err
	}
	vm, ok := f.VMs[vmid]
	if !ok {
		return "", ErrFakeNotFound
	}
	for i, s := range vm.Snapshots {
		if s == name {
			vm.Snapshots = append(vm.Snapshots[:i], vm.Snapshots[i+1:]...)
			return "UPID:snapshot-delete", nil
		}
	}
	return "", fmt.Errorf("proxmox fake: no snapshot %q", name)
}

// ConsolePeer is what a fake console connection talks to: it records what the browser sent and replies with
// an RFB banner, enough to prove the bridge moves bytes both ways.
func (f *Fake) Console(_ context.Context, vmid int) (ConsoleTicket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("console", vmid); err != nil {
		return ConsoleTicket{}, err
	}
	vm, ok := f.VMs[vmid]
	if !ok {
		return ConsoleTicket{}, ErrFakeNotFound
	}
	if !vm.Running {
		return ConsoleTicket{}, errors.New("proxmox fake: vm is not running")
	}
	return ConsoleTicket{Port: 5900 + vmid%100, Ticket: fmt.Sprintf("PVEVNC:fake-%d", vmid)}, nil
}

func (f *Fake) DialConsole(_ context.Context, vmid int, t ConsoleTicket) (ConsoleConn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("dialconsole", vmid); err != nil {
		return nil, err
	}
	if t.Ticket != fmt.Sprintf("PVEVNC:fake-%d", vmid) {
		return nil, errors.New("proxmox fake: bad console ticket")
	}
	c := &FakeConsole{in: make(chan []byte, 16), Sent: nil}
	c.in <- []byte("RFB 003.008\n")
	f.Consoles = append(f.Consoles, c)
	return c, nil
}

// FakeConsole is the in-memory guest end of a console. Everything written to it is kept in Sent and echoed back.
type FakeConsole struct {
	mu     sync.Mutex
	in     chan []byte
	Sent   [][]byte
	Closed bool
}

func (c *FakeConsole) ReadMessage() ([]byte, error) {
	b, ok := <-c.in
	if !ok {
		return nil, errors.New("closed")
	}
	return b, nil
}

func (c *FakeConsole) WriteMessage(b []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Closed {
		return errors.New("closed")
	}
	c.Sent = append(c.Sent, append([]byte(nil), b...))
	select {
	case c.in <- append([]byte("echo:"), b...):
	default:
	}
	return nil
}

func (c *FakeConsole) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.Closed {
		c.Closed = true
		close(c.in)
	}
	return nil
}

// ExecCall is one command the fake guest ran.
type ExecCall struct {
	VMID    int
	Command []string
	Stdin   string
	Exit    int
	Output  string
}

func (f *Fake) AgentExec(_ context.Context, vmid int, command []string, stdin string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("exec", vmid); err != nil {
		return 0, err
	}
	vm, ok := f.VMs[vmid]
	if !ok {
		return 0, ErrFakeNotFound
	}
	if !vm.Running {
		return 0, errors.New("proxmox fake: guest agent is not running")
	}
	call := ExecCall{VMID: vmid, Command: command, Stdin: stdin}
	if f.ExecFn != nil {
		call.Exit, call.Output = f.ExecFn(vmid, command, stdin)
	}
	f.Execs = append(f.Execs, call)
	return len(f.Execs), nil
}

func (f *Fake) AgentExecStatus(_ context.Context, _ int, pid int) (ExecStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if pid < 1 || pid > len(f.Execs) {
		return ExecStatus{}, errors.New("proxmox fake: no such pid")
	}
	c := f.Execs[pid-1]
	return ExecStatus{Exited: true, ExitCode: c.Exit, Output: c.Output}, nil
}

func (f *Fake) Isolate(_ context.Context, vmid int, allowed []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("isolate", vmid); err != nil {
		return err
	}
	vm, ok := f.VMs[vmid]
	if !ok {
		return ErrFakeNotFound
	}
	vm.Isolated = true
	vm.Allowed = append([]string(nil), allowed...)
	return nil
}

func (f *Fake) HostFirewall(context.Context) (HostFirewallState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	on := !f.HostFirewallOff
	return HostFirewallState{Cluster: on, Node: on}, nil
}

func (f *Fake) SetNIC(_ context.Context, vmid int, p NICParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("setnic", vmid); err != nil {
		return err
	}
	vm, ok := f.VMs[vmid]
	if !ok {
		return ErrFakeNotFound
	}
	if vm.NICs == nil {
		vm.NICs = map[int]NICParams{}
	}
	vm.NICs[p.Slot] = p
	return nil
}

func (f *Fake) RemoveNIC(_ context.Context, vmid int, slot int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("removenic", vmid); err != nil {
		return err
	}
	vm, ok := f.VMs[vmid]
	if !ok {
		return ErrFakeNotFound
	}
	delete(vm.NICs, slot)
	delete(vm.NICAllowed, slot)
	return nil
}

func (f *Fake) IsolateNIC(_ context.Context, vmid int, slot int, allowed []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("isolatenic", vmid); err != nil {
		return err
	}
	vm, ok := f.VMs[vmid]
	if !ok {
		return ErrFakeNotFound
	}
	if vm.NICAllowed == nil {
		vm.NICAllowed = map[int][]string{}
	}
	vm.NICAllowed[slot] = append([]string(nil), allowed...)
	return nil
}

func (f *Fake) Bridges(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.BridgeNames != nil {
		return f.BridgeNames, nil
	}
	return []string{"vmbr0", "vmbr1"}, nil
}
