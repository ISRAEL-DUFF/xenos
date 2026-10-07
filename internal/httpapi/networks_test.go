package httpapi

import (
	"context"
	"fmt"
	"testing"
)

func netPath(id any) string { return fmt.Sprintf("/v1/networks/%v", id) }

func newNet(t *testing.T, u *vmFixture, name string) map[string]any {
	t.Helper()
	code, out := u.c.do("POST", "/v1/networks", map[string]any{"name": name}, u.c.csrfHdr())
	if code != 201 {
		t.Fatalf("create network %s = %d %v", name, code, out)
	}
	return out
}

func TestNetworksGetTheirOwnSubnetAndVLAN(t *testing.T) {
	env, _, _ := floatingEnv(t)
	env.srv.Cfg.PrivateNetworkLimit, env.srv.Cfg.PrivateVLANMin, env.srv.Cfg.PrivateVLANMax = 2, 1000, 1010
	a := newVMUser(t, env, "a@x.co", 100*nanoDay)
	b := newVMUser(t, env, "b@x.co", 100*nanoDay)
	n1, n2 := newNet(t, a, "db"), newNet(t, b, "db") // the same name in another account is fine
	if n1["cidr"] != "10.64.0.0/24" || n2["cidr"] != "10.64.1.0/24" || n1["vlan_id"] == n2["vlan_id"] {
		t.Fatalf("networks must not share a subnet or VLAN: %v %v", n1, n2)
	}
	if code, _ := a.c.do("POST", "/v1/networks", map[string]any{"name": "db"}, a.c.csrfHdr()); code != 409 {
		t.Fatalf("duplicate name = %d", code)
	}
	newNet(t, a, "web")
	if code, out := a.c.do("POST", "/v1/networks", map[string]any{"name": "third"}, a.c.csrfHdr()); code != 409 {
		t.Fatalf("over the limit = %d %v", code, out)
	}
	if code, _ := a.c.do("POST", "/v1/networks", map[string]any{"name": "bad/name"}, a.c.csrfHdr()); code != 400 {
		t.Fatalf("bad name = %d", code)
	}
	// Another account cannot see or delete it.
	id := n1["id"].(float64)
	if code, _ := b.c.do("GET", netPath(int64(id)), nil, nil); code != 404 {
		t.Fatalf("b reads a's network: %d", code)
	}
	if code, _ := b.c.do("DELETE", netPath(int64(id)), nil, b.c.csrfHdr()); code != 404 {
		t.Fatalf("b deletes a's network: %d", code)
	}
	if code, out := a.c.do("GET", "/v1/networks", nil, nil); code != 200 || len(out["networks"].([]any)) != 2 {
		t.Fatalf("list = %d %v", code, out)
	}
}

func TestVLANExhaustionIsReported(t *testing.T) {
	env, _, _ := floatingEnv(t)
	env.srv.Cfg.PrivateNetworkLimit, env.srv.Cfg.PrivateVLANMin, env.srv.Cfg.PrivateVLANMax = 5, 1000, 1000
	a := newVMUser(t, env, "a@x.co", 0)
	newNet(t, a, "one")
	if code, out := a.c.do("POST", "/v1/networks", map[string]any{"name": "two"}, a.c.csrfHdr()); code != 503 {
		t.Fatalf("exhausted = %d %v, want 503", code, out)
	}
}

func TestVMsJoinANetworkAtCreationAndLater(t *testing.T) {
	env, pve, prov := floatingEnv(t)
	env.srv.Cfg.PrivateNetworkLimit, env.srv.Cfg.PrivateVLANMin, env.srv.Cfg.PrivateVLANMax = 5, 1000, 1010
	a := newVMUser(t, env, "a@x.co", 100*nanoDay)
	env.st.Pool.Exec(context.Background(), `UPDATE users SET vm_limit = 20`)
	net := newNet(t, a, "db")
	nid := int64(net["id"].(float64))

	// One VM joins at creation, the other afterwards.
	_, o1 := a.create(t, map[string]any{"hostname": "one", "networks": []int64{nid}})
	_, o2 := a.create(t, map[string]any{"hostname": "two"})
	vm1, vm2 := int64(o1["id"].(float64)), int64(o2["id"].(float64))
	env.run(t, prov)
	g1, g2 := guestFor(t, pve, env, vm1), guestFor(t, pve, env, vm2)
	nic := g1.NICs[1]
	if nic.VLAN != int(net["vlan_id"].(float64)) || nic.Bridge != "vmbr1" || nic.IPConfig != "ip=10.64.0.2/24" || !contains(g1.NICAllowed[1], "10.64.0.2") {
		t.Fatalf("vm1 nic = %+v allowed %v", nic, g1.NICAllowed)
	}
	if len(g2.NICs) != 0 {
		t.Fatal("vm2 must have no private nic yet")
	}

	code, out := a.c.do("POST", fmt.Sprintf("/v1/vms/%d/networks/%d", vm2, nid), nil, a.c.csrfHdr())
	if code != 202 || out["busy"] != "networking" {
		t.Fatalf("attach = %d %v", code, out)
	}
	if code, _ := a.c.do("POST", fmt.Sprintf("/v1/vms/%d/stop", vm2), nil, a.c.csrfHdr()); code != 409 {
		t.Fatalf("a busy VM must refuse other operations: %d", code)
	}
	env.run(t, prov)
	nic = g2.NICs[1]
	if nic.IPConfig != "ip=10.64.0.3/24" || nic.VLAN != int(net["vlan_id"].(float64)) || !g2.Running {
		t.Fatalf("vm2 nic = %+v running=%v", nic, g2.Running)
	}
	if code, vmOut := a.c.do("GET", fmt.Sprintf("/v1/vms/%d", vm2), nil, nil); code != 200 || vmOut["busy"] != nil || len(vmOut["private_ips"].([]any)) != 1 {
		t.Fatalf("vm2 = %d %v", code, vmOut)
	}
	// Joining twice, and deleting a network with members, are refused.
	if code, _ := a.c.do("POST", fmt.Sprintf("/v1/vms/%d/networks/%d", vm2, nid), nil, a.c.csrfHdr()); code != 409 {
		t.Fatalf("second attach = %d", code)
	}
	if code, _ := a.c.do("DELETE", netPath(nid), nil, a.c.csrfHdr()); code != 409 {
		t.Fatalf("delete with members = %d, want 409", code)
	}
	if code, out := a.c.do("GET", netPath(nid), nil, nil); code != 200 || len(out["members"].([]any)) != 2 {
		t.Fatalf("members = %v", out)
	}

	// Detach vm2: its NIC and filter go, the address frees up and is reused.
	if code, _ := a.c.do("DELETE", fmt.Sprintf("/v1/vms/%d/networks/%d", vm2, nid), nil, a.c.csrfHdr()); code != 202 {
		t.Fatalf("detach = %d", code)
	}
	env.run(t, prov)
	if len(g2.NICs) != 0 || len(g2.NICAllowed) != 0 {
		t.Fatalf("vm2 nic after detach: %v %v", g2.NICs, g2.NICAllowed)
	}
	_, o3 := a.create(t, map[string]any{"hostname": "three", "networks": []int64{nid}})
	env.run(t, prov)
	if got := guestFor(t, pve, env, int64(o3["id"].(float64))).NICs[1].IPConfig; got != "ip=10.64.0.3/24" {
		t.Fatalf("the freed address should be reused: %s", got)
	}
	// Deleting a VM frees its address and, once empty, the network can go.
	for _, id := range []int64{vm1, int64(o3["id"].(float64))} {
		a.c.do("DELETE", fmt.Sprintf("/v1/vms/%d", id), nil, a.c.csrfHdr())
	}
	env.run(t, prov)
	if code, _ := a.c.do("DELETE", netPath(nid), nil, a.c.csrfHdr()); code != 204 {
		t.Fatalf("delete empty network = %d", code)
	}
}

func TestNetworkIsolationBetweenAccountsAndLimits(t *testing.T) {
	env, _, prov := floatingEnv(t)
	env.srv.Cfg.PrivateNetworkLimit, env.srv.Cfg.PrivateVLANMin, env.srv.Cfg.PrivateVLANMax = 5, 1000, 1010
	a := newVMUser(t, env, "a@x.co", 100*nanoDay)
	b := newVMUser(t, env, "b@x.co", 100*nanoDay)
	netA := newNet(t, a, "db")
	nidA := int64(netA["id"].(float64))
	_, ob := b.create(t, map[string]any{"hostname": "bs"})
	vmB := int64(ob["id"].(float64))
	env.run(t, prov)
	// b cannot join a's network, neither at creation nor by attaching; a cannot put b's VM on it.
	if code, _ := b.c.do("POST", fmt.Sprintf("/v1/vms/%d/networks/%d", vmB, nidA), nil, b.c.csrfHdr()); code != 404 {
		t.Fatalf("b attaches to a's network: %d", code)
	}
	if code, _ := b.c.do("POST", "/v1/vms", map[string]any{"plan": "nano", "template": "ubuntu-24.04", "hostname": "x", "ssh_key_ids": []int64{b.key}, "networks": []int64{nidA}}, b.c.csrfHdr()); code != 400 {
		t.Fatalf("b creates onto a's network: %d", code)
	}
	if code, _ := a.c.do("POST", fmt.Sprintf("/v1/vms/%d/networks/%d", vmB, nidA), nil, a.c.csrfHdr()); code != 404 {
		t.Fatalf("a attaches b's vm: %d", code)
	}
	// A VM holds at most two networks.
	n2, n3 := newNet(t, a, "n2"), newNet(t, a, "n3")
	_, oa := a.create(t, map[string]any{"hostname": "mine", "networks": []int64{nidA, int64(n2["id"].(float64))}})
	env.run(t, prov)
	if code, _ := a.c.do("POST", fmt.Sprintf("/v1/vms/%d/networks/%d", int64(oa["id"].(float64)), int64(n3["id"].(float64))), nil, a.c.csrfHdr()); code != 409 {
		t.Fatalf("a third network on one VM = %d, want 409", code)
	}
	if resp := postJSON(t, env, a.c, "/v1/account/close", closeBody("a@x.co", true), nil); resp.StatusCode != 409 {
		t.Fatalf("close with networks = %d", resp.StatusCode)
	}
}

func TestNetworksArePinnedToTheirHost(t *testing.T) {
	f := newFleet(t, 3, "a", "b")
	f.env.srv.Cfg.PrivateNetworkLimit, f.env.srv.Cfg.PrivateVLANMin, f.env.srv.Cfg.PrivateVLANMax = 5, 1000, 1010
	u := newVMUser(t, f.env, "u@x.co", 1000*nanoDay)
	f.env.st.Pool.Exec(context.Background(), `UPDATE users SET vm_limit = 20`)
	net := newNet(t, u, "db")
	nid := int64(net["id"].(float64))
	// Several VMs on the network: all must land on the first one's host even though placement would spread them.
	hostsSeen := map[string]bool{}
	for i := 0; i < 3; i++ {
		code, out := u.create(t, map[string]any{"hostname": fmt.Sprintf("n%d", i), "networks": []int64{nid}})
		if code != 202 {
			t.Fatalf("create %d = %d %v", i, code, out)
		}
		hostsSeen[f.hostOf(t, out)] = true
	}
	if len(hostsSeen) != 1 {
		t.Fatalf("VMs of one network spread over hosts %v", hostsSeen)
	}
	// A VM on the other host cannot join, with or without the network's first VM still there.
	var other string
	for h := range f.fakes {
		if !hostsSeen[h] {
			other = h
		}
	}
	if _, err := f.env.st.Pool.Exec(context.Background(), `UPDATE vms SET host=$1 WHERE hostname='n0'`, other); err != nil {
		t.Fatal(err) // move one VM's record to the other host to stand in for a VM that was placed there
	}
	var strayID int64
	_ = f.env.st.Pool.QueryRow(context.Background(), `SELECT id FROM vms WHERE hostname='n0'`).Scan(&strayID)
	if _, err := f.env.st.Pool.Exec(context.Background(), `UPDATE vms SET state='running' WHERE id=$1`, strayID); err != nil {
		t.Fatal(err)
	}
	f.env.st.Pool.Exec(context.Background(), `DELETE FROM vm_private_ips WHERE vm_id=$1`, strayID)
	if code, out := u.c.do("POST", fmt.Sprintf("/v1/vms/%d/networks/%d", strayID, nid), nil, u.c.csrfHdr()); code != 409 {
		t.Fatalf("cross-host join = %d %v, want 409", code, out)
	}
	// With a tunnel configured the operator accepts that networks span hosts.
	f.env.srv.Cfg.PrivateNetworkTunnel = true
	if code, out := u.c.do("POST", fmt.Sprintf("/v1/vms/%d/networks/%d", strayID, nid), nil, u.c.csrfHdr()); code != 202 {
		t.Fatalf("join with a tunnel = %d %v", code, out)
	}
}
