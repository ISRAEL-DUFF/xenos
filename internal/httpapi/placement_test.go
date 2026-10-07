package httpapi

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/israel-duff/xenos/internal/hosts"
	"github.com/israel-duff/xenos/internal/proxmox"
	"github.com/israel-duff/xenos/internal/vm"
)

type fleet struct {
	env   *testEnv
	fakes map[string]*proxmox.Fake
}

// newFleet gives the server the named hosts (each with its own fake, one template and ips free addresses).
func newFleet(t *testing.T, ips int, names ...string) *fleet {
	env := newTestEnv(t)
	f := &fleet{env: env, fakes: map[string]*proxmox.Fake{}}
	var hs []*hosts.Host
	for _, n := range names {
		fk := proxmox.NewFake()
		f.fakes[n] = fk
		hs = append(hs, &hosts.Host{Name: n, Region: "test-1", API: fk, Node: n})
	}
	set := hosts.NewSet(hs...)
	if err := set.Attach(context.Background(), env.st); err != nil {
		t.Fatal(err)
	}
	env.srv.Hosts = set
	ctx := context.Background()
	for i, n := range names {
		if _, err := env.st.Pool.Exec(ctx, `INSERT INTO host_templates (host, template_id, proxmox_template_id) SELECT $1, id, 9000 FROM templates ON CONFLICT DO NOTHING`, n); err != nil {
			t.Fatal(err)
		}
		for k := 0; k < ips; k++ {
			if _, err := env.st.Pool.Exec(ctx, `INSERT INTO ip_addresses (address, gateway, region, host) VALUES ($1::inet, '203.0.113.1', 'test-1', $2)`,
				fmt.Sprintf("203.0.%d.%d", 100+i, 10+k), n); err != nil {
				t.Fatal(err)
			}
		}
	}
	return f
}

func (f *fleet) hostOf(t *testing.T, out map[string]any) string {
	var h string
	if err := f.env.st.Pool.QueryRow(context.Background(), `SELECT host FROM vms WHERE id=$1`, int64(out["id"].(float64))).Scan(&h); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestPlacementPicksTheLeastCommittedHost(t *testing.T) {
	f := newFleet(t, 5, "a", "b", "c")
	u := newVMUser(t, f.env, "u@x.co", 1000*nanoDay)
	// Put load on a and b so c is emptiest, then b is next.
	f.env.st.Pool.Exec(context.Background(), `UPDATE users SET vm_limit = 20`)
	var seen []string
	for i := 0; i < 4; i++ {
		code, out := u.create(t, map[string]any{"hostname": fmt.Sprintf("n%d", i)})
		if code != 202 {
			t.Fatalf("create %d = %d %v", i, code, out)
		}
		seen = append(seen, f.hostOf(t, out))
	}
	// Equal hosts: ties go by name, then the committed fraction rotates the choice.
	if seen[0] != "a" || seen[1] != "b" || seen[2] != "c" || seen[3] != "a" {
		t.Fatalf("placement order = %v", seen)
	}
}

func TestPlacementSkipsHostsThatCannotTakeTheVM(t *testing.T) {
	f := newFleet(t, 3, "a", "b", "c", "d")
	u := newVMUser(t, f.env, "u@x.co", 1000*nanoDay)
	ctx := context.Background()
	must := func(sql string, args ...any) {
		if _, err := f.env.st.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	must(`UPDATE hosts SET status='draining' WHERE name='a'`)
	must(`DELETE FROM host_templates WHERE host='b'`)
	f.fakes["c"].MemTotal = 512 << 20 // a nano VM wants 1024 MB
	code, out := u.create(t, nil)
	if code != 202 || f.hostOf(t, out) != "d" {
		t.Fatalf("create = %d %v host %s: only d can take it (a drains, b lacks the template, c is full)", code, out, f.hostOf(t, out))
	}
	// No free IP on d: nothing can take it.
	must(`UPDATE ip_addresses SET vm_id = (SELECT id FROM vms LIMIT 1) WHERE host='d' AND vm_id IS NULL`)
	if code, out := u.create(t, map[string]any{"hostname": "two"}); code != 503 {
		t.Fatalf("with no capacity = %d %v, want 503", code, out)
	}
}

func TestSpreadGroupsPlaceApartAndRefuseWhenHostsRunOut(t *testing.T) {
	f := newFleet(t, 5, "a", "b")
	u := newVMUser(t, f.env, "u@x.co", 1000*nanoDay)
	f.env.st.Pool.Exec(context.Background(), `UPDATE users SET vm_limit = 20`)
	code, o1 := u.create(t, map[string]any{"hostname": "n1", "spread_group": "pg-1"})
	code2, o2 := u.create(t, map[string]any{"hostname": "n2", "spread_group": "pg-1"})
	if code != 202 || code2 != 202 || f.hostOf(t, o1) == f.hostOf(t, o2) {
		t.Fatalf("a spread pair must land apart: %d %d %s %s", code, code2, f.hostOf(t, o1), f.hostOf(t, o2))
	}
	if code, out := u.create(t, map[string]any{"hostname": "n3", "spread_group": "pg-1"}); code != 409 {
		t.Fatalf("a third in a two-host fleet = %d %v, want 409", code, out)
	}
	// prefer: share a host instead of failing. The group can also come from the label.
	code, o3 := u.create(t, map[string]any{"hostname": "n3", "labels": map[string]string{"spread.group": "pg-1"}, "spread": "prefer"})
	if code != 202 {
		t.Fatalf("prefer = %d %v", code, o3)
	}
	// Another group is independent, and a deleted VM frees its host for the group.
	if code, _ := u.create(t, map[string]any{"hostname": "other", "spread_group": "pg-2"}); code != 202 {
		t.Fatalf("other group = %d", code)
	}
	if code, out := u.create(t, map[string]any{"hostname": "bad", "spread_group": "no spaces!"}); code != 400 {
		t.Fatalf("bad group = %d %v", code, out)
	}
}

func TestPlacementSkipsAnUnreachableHostAndCachesItsHealth(t *testing.T) {
	f := newFleet(t, 3, "a", "b")
	u := newVMUser(t, f.env, "u@x.co", 1000*nanoDay)
	f.env.st.Pool.Exec(context.Background(), `UPDATE users SET vm_limit = 20`)
	f.fakes["a"].Fail["memory"] = fmt.Errorf("connection refused")
	for i := 0; i < 2; i++ {
		code, out := u.create(t, map[string]any{"hostname": fmt.Sprintf("n%d", i)})
		if code != 202 || f.hostOf(t, out) != "b" {
			t.Fatalf("create %d = %d %v: a is down", i, code, out)
		}
	}
	// Recovered, but the verdict is cached for 30 seconds: still not placed there until it expires.
	delete(f.fakes["a"].Fail, "memory")
	f.env.srv.health.mu.Lock()
	h := f.env.srv.health.m["a"]
	h.at = h.at.Add(-time.Minute)
	f.env.srv.health.m["a"] = h
	f.env.srv.health.mu.Unlock()
	code, out := u.create(t, map[string]any{"hostname": "back"})
	if code != 202 || f.hostOf(t, out) != "a" {
		t.Fatalf("after recovery = %d %v host %s", code, out, f.hostOf(t, out))
	}
}

func TestOperationsReachTheHostHoldingTheVM(t *testing.T) {
	f := newFleet(t, 3, "a", "b")
	u := newVMUser(t, f.env, "u@x.co", 1000*nanoDay)
	f.env.st.Pool.Exec(context.Background(), `UPDATE users SET vm_limit = 20`)
	_, o1 := u.create(t, map[string]any{"hostname": "one", "spread_group": "g"})
	_, o2 := u.create(t, map[string]any{"hostname": "two", "spread_group": "g"})
	prov := &vm.Provisioner{Store: f.env.st, PVE: f.env.srv.Hosts, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Cfg: vm.Config{Storage: "vmdata", Disk: "scsi0", AgentTimeout: time.Second, PollInterval: 5 * time.Millisecond, IPv4PrefixLen: 32}}
	f.env.run(t, prov)
	for _, out := range []map[string]any{o1, o2} {
		var vmid int
		_ = f.env.st.Pool.QueryRow(context.Background(), `SELECT proxmox_vmid FROM vms WHERE id=$1`, int64(out["id"].(float64))).Scan(&vmid)
		host := f.hostOf(t, out)
		other := "a"
		if host == "a" {
			other = "b"
		}
		if g := f.fakes[host].VMs[vmid]; g == nil || !g.Running || !g.Isolated {
			t.Fatalf("vm %v should run isolated on host %s", out["id"], host)
		}
		if f.fakes[other].VMs[vmid] != nil {
			t.Fatalf("vm %v leaked onto host %s", out["id"], other)
		}
	}
	// Stop, then delete: each reaches the same host.
	id := int64(o1["id"].(float64))
	u.c.do("POST", "/v1/vms/"+itoa(id)+"/stop", nil, u.c.csrfHdr())
	u.c.do("DELETE", "/v1/vms/"+itoa(id), nil, u.c.csrfHdr())
	f.env.run(t, prov)
	var vmid int
	_ = f.env.st.Pool.QueryRow(context.Background(), `SELECT proxmox_vmid FROM vms WHERE id=$1`, id).Scan(&vmid)
	if f.fakes[f.hostOf(t, o1)].VMs[vmid] != nil {
		t.Fatal("the guest should be gone from its host")
	}
}

func TestFloatingIPsStayOnTheirHost(t *testing.T) {
	f := newFleet(t, 3, "a", "b")
	f.env.srv.Cfg.FloatingIPLimit = 3
	for host, addr := range map[string]string{"a": "198.51.100.10", "b": "198.51.100.20"} {
		if _, err := f.env.st.Pool.Exec(context.Background(), `INSERT INTO floating_ips (address, region, host) VALUES ($1::inet, 'test-1', $2)`, addr, host); err != nil {
			t.Fatal(err)
		}
	}
	u := newVMUser(t, f.env, "u@x.co", 1000*nanoDay)
	f.env.st.Pool.Exec(context.Background(), `UPDATE users SET vm_limit = 20`)
	_, o1 := u.create(t, map[string]any{"hostname": "one", "spread_group": "g"})
	_, o2 := u.create(t, map[string]any{"hostname": "two", "spread_group": "g"})
	vm1, vm2 := int64(o1["id"].(float64)), int64(o2["id"].(float64))
	prov := &vm.Provisioner{Store: f.env.st, PVE: f.env.srv.Hosts, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Cfg: vm.Config{Storage: "vmdata", Disk: "scsi0", AgentTimeout: time.Second, PollInterval: 5 * time.Millisecond, IPv4PrefixLen: 32}}
	f.env.run(t, prov)
	// Allocating for a VM takes an address from that VM's host.
	code, fip := u.c.do("POST", "/v1/floating-ips", map[string]any{"vm_id": vm2}, u.c.csrfHdr())
	if code != 201 || fip["host"] != f.hostOf(t, o2) {
		t.Fatalf("allocate for vm2 = %d %v (vm2 is on %s)", code, fip, f.hostOf(t, o2))
	}
	// It cannot move to a VM on the other host.
	path := "/v1/floating-ips/" + itoa(int64(fip["id"].(float64)))
	if code, out := u.c.do("POST", path+"/attach", map[string]any{"vm_id": vm1}, u.c.csrfHdr()); code != 409 {
		t.Fatalf("cross-host attach = %d %v, want 409", code, out)
	}
}

func TestStartupRefusesWhenVMsLiveOnAHostThatIsNotConfigured(t *testing.T) {
	f := newFleet(t, 2, "a", "b")
	u := newVMUser(t, f.env, "u@x.co", 100*nanoDay)
	_, out := u.create(t, nil)
	h := f.hostOf(t, out)
	only := "a"
	if h == "a" {
		only = "b"
	}
	set := hosts.NewSet(&hosts.Host{Name: only, API: proxmox.NewFake()})
	if err := set.Attach(context.Background(), f.env.st); err == nil {
		t.Fatalf("a hosts file without %q (which holds a VM) must be refused", h)
	}
}
