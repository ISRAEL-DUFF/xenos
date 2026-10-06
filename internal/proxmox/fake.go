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
