package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/israel-duff/xenos/internal/proxmox"
	"github.com/israel-duff/xenos/internal/testutil"
	"github.com/israel-duff/xenos/internal/vm"
)

func floatingEnv(t *testing.T) (*testEnv, *proxmox.Fake, *vm.Provisioner) {
	env := newTestEnv(t)
	env.srv.Cfg.FloatingIPLimit = 2
	env.srv.Cfg.FloatingIPPriceUUSDT = 2000
	addIPs(t, env, 4)
	for _, a := range []string{"198.51.100.10", "198.51.100.11", "198.51.100.12"} {
		if _, err := env.st.Pool.Exec(context.Background(), `INSERT INTO floating_ips (address, region) VALUES ($1::inet, 'test-1')`, a); err != nil {
			t.Fatal(err)
		}
	}
	pve := proxmox.NewFake()
	prov := &vm.Provisioner{Store: env.st, PVE: pve, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Cfg: vm.Config{Storage: "vmdata", Disk: "scsi0", AgentTimeout: time.Second, PollInterval: 5 * time.Millisecond, IPv4PrefixLen: 32, Nameservers: "1.1.1.1"}}
	return env, pve, prov
}

func (e *testEnv) run(t *testing.T, prov *vm.Provisioner) {
	t.Helper()
	testutil.DrainJobs(t, e.st, prov.Handlers())
}

func guestFor(t *testing.T, pve *proxmox.Fake, env *testEnv, vmID int64) *proxmox.FakeVM {
	var vmid int
	if err := env.st.Pool.QueryRow(context.Background(), `SELECT proxmox_vmid FROM vms WHERE id=$1`, vmID).Scan(&vmid); err != nil {
		t.Fatal(err)
	}
	g := pve.VMs[vmid]
	if g == nil {
		t.Fatalf("vm %d has no guest", vmID)
	}
	return g
}

// ran lists the floating-address commands run on a guest, in order.
func ran(pve *proxmox.Fake, g *proxmox.FakeVM, what string) int {
	n := 0
	for _, e := range pve.Execs {
		if e.VMID == g.ID && strings.Contains(e.Stdin, what) {
			n++
		}
	}
	return n
}

func TestFloatingIPMovesBetweenVMsWithFirewallAndGuestCommands(t *testing.T) {
	env, pve, prov := floatingEnv(t)
	a := newVMUser(t, env, "a@x.co", 100*nanoDay)
	_, o1 := a.create(t, map[string]any{"hostname": "one"})
	_, o2 := a.create(t, map[string]any{"hostname": "two"})
	vm1, vm2 := int64(o1["id"].(float64)), int64(o2["id"].(float64))
	env.run(t, prov)
	g1, g2 := guestFor(t, pve, env, vm1), guestFor(t, pve, env, vm2)

	code, f := a.c.do("POST", "/v1/floating-ips", map[string]any{"label": "vip"}, a.c.csrfHdr())
	if code != 201 || f["address"] != "198.51.100.10" || f["vm_id"] != nil {
		t.Fatalf("allocate = %d %v", code, f)
	}
	id := int64(f["id"].(float64))
	path := "/v1/floating-ips/" + itoa(id)

	// Attach to VM 1: its firewall set gains the address and the guest configures it.
	if code, _ := a.c.do("POST", path+"/attach", map[string]any{"vm_id": vm1}, a.c.csrfHdr()); code != 202 {
		t.Fatalf("attach = %d", code)
	}
	env.run(t, prov)
	if !contains(g1.Allowed, "198.51.100.10") || !contains(g1.Allowed, "203.0.113.10") && !anyPrefix(g1.Allowed, "203.0.113.") {
		t.Fatalf("vm1 set = %v", g1.Allowed)
	}
	if n := ran(pve, g1, "addr replace 198.51.100.10/32"); n != 1 {
		t.Fatalf("guest 1 configured %d times", n)
	}
	if code, out := a.c.do("GET", path, nil, nil); code != 200 || out["applied"] != true || int64(out["vm_id"].(float64)) != vm1 {
		t.Fatalf("get = %d %v", code, out)
	}
	// Attaching to where it already is does nothing.
	if code, _ := a.c.do("POST", path+"/attach", map[string]any{"vm_id": vm1}, a.c.csrfHdr()); code != 200 {
		t.Fatalf("repeat attach = %d, want 200", code)
	}
	if n := env.countJobs(t, vm.JobFloating); n != 0 {
		t.Fatalf("a repeat attach queued %d jobs", n)
	}

	// Move to VM 2: taken off VM 1 (set and interface), put on VM 2.
	if code, _ := a.c.do("POST", path+"/attach", map[string]any{"vm_id": vm2}, a.c.csrfHdr()); code != 202 {
		t.Fatalf("move = %d", code)
	}
	env.run(t, prov)
	if contains(g1.Allowed, "198.51.100.10") || !contains(g2.Allowed, "198.51.100.10") {
		t.Fatalf("after the move: vm1 %v vm2 %v", g1.Allowed, g2.Allowed)
	}
	if ran(pve, g1, "addr del 198.51.100.10/32") != 1 || ran(pve, g2, "addr replace 198.51.100.10/32") != 1 {
		t.Fatal("the guests were not reconfigured")
	}

	// A restart re-applies it (the address lives only in the running guest).
	if code, _ := a.c.do("POST", "/v1/vms/"+itoa(vm2)+"/stop", nil, a.c.csrfHdr()); code != 202 {
		t.Fatalf("stop = %d", code)
	}
	env.run(t, prov)
	if code, _ := a.c.do("POST", "/v1/vms/"+itoa(vm2)+"/start", nil, a.c.csrfHdr()); code != 202 {
		t.Fatalf("start = %d", code)
	}
	env.run(t, prov)
	if n := ran(pve, g2, "addr replace 198.51.100.10/32"); n != 2 {
		t.Fatalf("after a restart the guest was configured %d times, want 2", n)
	}

	// Detach, then release.
	if code, _ := a.c.do("POST", path+"/detach", nil, a.c.csrfHdr()); code != 202 {
		t.Fatalf("detach = %d", code)
	}
	env.run(t, prov)
	if contains(g2.Allowed, "198.51.100.10") || ran(pve, g2, "addr del 198.51.100.10/32") != 1 {
		t.Fatalf("detach left it on vm2: %v", g2.Allowed)
	}
	if code, _ := a.c.do("DELETE", path, nil, a.c.csrfHdr()); code != 202 {
		t.Fatalf("release = %d", code)
	}
	if code, _ := a.c.do("GET", path, nil, nil); code != 404 {
		t.Fatalf("a released address is still visible: %d", code)
	}
}

func TestFloatingIPOwnershipLimitsAndDeadVM(t *testing.T) {
	env, pve, prov := floatingEnv(t)
	a := newVMUser(t, env, "a@x.co", 100*nanoDay)
	b := newVMUser(t, env, "b@x.co", 100*nanoDay)
	_, oa := a.create(t, map[string]any{"hostname": "one"})
	_, oa2 := a.create(t, map[string]any{"hostname": "two"})
	_, ob := b.create(t, map[string]any{"hostname": "bs"})
	vmA, vmA2, vmB := int64(oa["id"].(float64)), int64(oa2["id"].(float64)), int64(ob["id"].(float64))
	env.run(t, prov)

	_, f := a.c.do("POST", "/v1/floating-ips", map[string]any{"vm_id": vmA}, a.c.csrfHdr())
	id := int64(f["id"].(float64))
	path := "/v1/floating-ips/" + itoa(id)
	env.run(t, prov)

	// Another account sees nothing and can do nothing; and a VM of another account cannot be a target.
	if code, _ := b.c.do("GET", path, nil, nil); code != 404 {
		t.Fatalf("b reads a's address: %d", code)
	}
	if code, _ := b.c.do("POST", path+"/attach", map[string]any{"vm_id": vmB}, b.c.csrfHdr()); code != 404 {
		t.Fatalf("b attaches a's address: %d", code)
	}
	if code, _ := b.c.do("DELETE", path, nil, b.c.csrfHdr()); code != 404 {
		t.Fatalf("b releases a's address: %d", code)
	}
	if code, _ := a.c.do("POST", path+"/attach", map[string]any{"vm_id": vmB}, a.c.csrfHdr()); code != 404 {
		t.Fatalf("attach to someone else's vm: %d", code)
	}
	// The limit is two per account.
	if code, _ := a.c.do("POST", "/v1/floating-ips", map[string]any{}, a.c.csrfHdr()); code != 201 {
		t.Fatalf("second = %d", code)
	}
	if code, out := a.c.do("POST", "/v1/floating-ips", map[string]any{}, a.c.csrfHdr()); code != 409 {
		t.Fatalf("third = %d %v, want the limit", code, out)
	}
	// Closing is refused while an address is held.
	if resp := postJSON(t, env, a.c, "/v1/account/close", closeBody("a@x.co", true), nil); resp.StatusCode != 409 {
		t.Fatalf("close with floating IPs = %d, want 409", resp.StatusCode)
	}

	// Failover: the VM dies (its guest is gone), the address moves to the other VM anyway.
	g1 := guestFor(t, pve, env, vmA)
	delete(pve.VMs, g1.ID)
	if code, _ := a.c.do("POST", path+"/attach", map[string]any{"vm_id": vmA2}, a.c.csrfHdr()); code != 202 {
		t.Fatalf("move off a dead vm = %d", code)
	}
	env.run(t, prov)
	g2 := guestFor(t, pve, env, vmA2)
	if !contains(g2.Allowed, "198.51.100.10") || ran(pve, g2, "addr replace 198.51.100.10/32") != 1 {
		t.Fatalf("the new vm was not configured: %v", g2.Allowed)
	}

	// Deleting a VM leaves the address with the account, pointing nowhere.
	if code, _ := a.c.do("DELETE", "/v1/vms/"+itoa(vmA2), nil, a.c.csrfHdr()); code != 202 {
		t.Fatalf("delete = %d", code)
	}
	env.run(t, prov)
	if code, out := a.c.do("GET", path, nil, nil); code != 200 || out["vm_id"] != nil {
		t.Fatalf("after the VM was deleted: %d %v", code, out)
	}
}

func TestFloatingIPNeedsABalanceAndAnActiveAccount(t *testing.T) {
	env, _, _ := floatingEnv(t)
	poor := newVMUser(t, env, "p@x.co", 0)
	if code, _ := poor.c.do("POST", "/v1/floating-ips", map[string]any{}, poor.c.csrfHdr()); code != 402 {
		t.Fatalf("no balance = %d, want 402", code)
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func anyPrefix(l []string, p string) bool {
	for _, x := range l {
		if strings.HasPrefix(x, p) {
			return true
		}
	}
	return false
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func (e *testEnv) countJobs(t *testing.T, kind string) int {
	return count(t, e, `SELECT count(*) FROM jobs WHERE kind=$1 AND status='queued'`, kind)
}

func TestFailedCleanupKeepsTheAddressOutOfThePool(t *testing.T) {
	env, pve, prov := floatingEnv(t)
	a := newVMUser(t, env, "a@x.co", 100*nanoDay)
	_, o1 := a.create(t, map[string]any{"hostname": "one"})
	vm1 := int64(o1["id"].(float64))
	env.run(t, prov)
	g1 := guestFor(t, pve, env, vm1)
	_, f := a.c.do("POST", "/v1/floating-ips", map[string]any{"vm_id": vm1}, a.c.csrfHdr())
	id := int64(f["id"].(float64))
	env.run(t, prov)

	// Release while the host's firewall call fails: the address must not become allocatable.
	pve.Fail["isolate"] = errors.New("proxmox is down")
	if code, _ := a.c.do("DELETE", "/v1/floating-ips/"+itoa(id), nil, a.c.csrfHdr()); code != 202 {
		t.Fatalf("release = %d", code)
	}
	env.run(t, prov) // the job fails and is retried; still failing
	b := newVMUser(t, env, "b@x.co", 100*nanoDay)
	for i := 0; i < 3; i++ { // b can take the other two free addresses, never the stuck one
		if code, out := b.c.do("POST", "/v1/floating-ips", map[string]any{}, b.c.csrfHdr()); code == 201 && out["address"] == f["address"] {
			t.Fatalf("the address was handed out while the old guest still has it")
		}
	}
	if !contains(g1.Allowed, "198.51.100.10") {
		t.Fatal("test setup: the old guest should still hold it")
	}
	// The monitor's sweep finds it, and once the host answers the cleanup completes.
	if n, _ := env.st.Q.FloatingDueForCheck(context.Background(), pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true}); len(n) != 1 || n[0] != id {
		t.Fatalf("the sweep should list the stuck address: %v", n)
	}
	delete(pve.Fail, "isolate")
	if _, err := env.st.Pool.Exec(context.Background(), `UPDATE jobs SET run_after = now() WHERE status = 'queued'`); err != nil {
		t.Fatal(err)
	}
	if err := prov.ReconcileFloating(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if contains(g1.Allowed, "198.51.100.10") {
		t.Fatal("the old guest must lose the address")
	}
	if n := count(t, env, `SELECT count(*) FROM floating_ips WHERE id=$1 AND applied_vm_id IS NULL`, id); n != 1 {
		t.Fatal("the address should now be clean")
	}
}
