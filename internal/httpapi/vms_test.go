package httpapi

import (
	"context"
	"fmt"
	"testing"
)

const nanoDay = 6000 * 24 // micro-USDT needed to cover 24h of a nano VM

type vmFixture struct {
	env *testEnv
	c   *client
	key int64
}

// newVMUser signs up a user with one SSH key and the given wallet balance.
func newVMUser(t *testing.T, env *testEnv, email string, balance int64) *vmFixture {
	t.Helper()
	c := signupClient(t, env.ts.URL, email)
	code, out := c.do("POST", "/v1/ssh-keys", map[string]any{"name": "k", "public_key": newPubKey(t)}, c.csrfHdr())
	if code != 201 {
		t.Fatalf("add key = %d %v", code, out)
	}
	var cust string
	if err := env.st.Pool.QueryRow(context.Background(), `SELECT ispend_customer_id FROM users WHERE email=$1`, email).Scan(&cust); err != nil {
		t.Fatal(err)
	}
	env.ispend.Credit(cust, balance)
	return &vmFixture{env: env, c: c, key: int64(out["id"].(float64))}
}

func (f *vmFixture) create(t *testing.T, extra map[string]any) (int, map[string]any) {
	body := map[string]any{"plan": "nano", "template": "ubuntu-24.04", "hostname": "web", "ssh_key_ids": []int64{f.key}}
	for k, v := range extra {
		body[k] = v
	}
	return f.c.do("POST", "/v1/vms", body, f.c.csrfHdr())
}

func addIPs(t *testing.T, env *testEnv, n int) {
	for i := 0; i < n; i++ {
		if _, err := env.st.Pool.Exec(context.Background(),
			`INSERT INTO ip_addresses (address, gateway, region) VALUES ($1::inet, '203.0.113.1', 'test-1')`,
			fmt.Sprintf("203.0.113.%d", 10+i)); err != nil {
			t.Fatal(err)
		}
	}
}

func count(t *testing.T, env *testEnv, sql string, args ...any) int {
	var n int
	if err := env.st.Pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCreateVM(t *testing.T) {
	env := newTestEnv(t)
	addIPs(t, env, 3)
	u := newVMUser(t, env, "a@x.co", 10*nanoDay)

	code, out := u.create(t, nil)
	if code != 202 {
		t.Fatalf("create = %d %v", code, out)
	}
	if out["state"] != "pending" || out["plan"] != "nano" || out["ipv4"] != "203.0.113.10" ||
		out["ssh_command"] != "ssh root@203.0.113.10" || out["region"] != "test-1" {
		t.Fatalf("unexpected vm %v", out)
	}
	if n := count(t, env, `SELECT count(*) FROM jobs WHERE kind='vm.provision' AND status='queued'`); n != 1 {
		t.Fatalf("queued provision jobs = %d", n)
	}
	if n := count(t, env, `SELECT count(*) FROM ip_addresses WHERE vm_id IS NOT NULL`); n != 1 {
		t.Fatalf("assigned IPs = %d", n)
	}
	var keys string
	_ = env.st.Pool.QueryRow(context.Background(), `SELECT authorized_keys FROM vms`).Scan(&keys)
	if keys == "" {
		t.Fatal("ssh keys must be snapshotted onto the VM")
	}

	// Listed and fetchable by the owner.
	if code, _ := u.c.do("GET", fmt.Sprintf("/v1/vms/%d", int64(out["id"].(float64))), nil, nil); code != 200 {
		t.Fatalf("get = %d", code)
	}
}

func TestCreateVMValidation(t *testing.T) {
	env := newTestEnv(t)
	addIPs(t, env, 3)
	a := newVMUser(t, env, "a@x.co", 10*nanoDay)
	b := newVMUser(t, env, "b@x.co", 10*nanoDay)

	for name, extra := range map[string]map[string]any{
		"no keys":          {"ssh_key_ids": []int64{}},
		"someone else key": {"ssh_key_ids": []int64{b.key}},
		"unknown plan":     {"plan": "gigantic"},
		"unknown template": {"template": "windows-95"},
		"bad hostname":     {"hostname": "-Bad_Name"},
		"long hostname":    {"hostname": "a123456789a123456789a123456789a123456789a123456789a123456789a1234"},
	} {
		if code, out := a.create(t, extra); code != 400 {
			t.Errorf("%s = %d %v, want 400", name, code, out)
		}
	}
	if n := count(t, env, `SELECT count(*) FROM vms`); n != 0 {
		t.Fatalf("rejected requests created %d VMs", n)
	}
	if code, _ := a.c.do("POST", "/v1/vms", map[string]any{"plan": "nano"}, nil); code != 403 {
		t.Fatalf("missing CSRF = %d", code)
	}
}

func TestCreateVMGenerateHostname(t *testing.T) {
	env := newTestEnv(t)
	addIPs(t, env, 1)
	u := newVMUser(t, env, "a@x.co", 10*nanoDay)
	code, out := u.create(t, map[string]any{"hostname": ""})
	if code != 202 || len(out["hostname"].(string)) != 9 {
		t.Fatalf("create = %d %v", code, out)
	}
}

func TestCreateVMLimitAndBalance(t *testing.T) {
	env := newTestEnv(t)
	addIPs(t, env, 5)

	// Wallet covers less than 24h.
	poor := newVMUser(t, env, "poor@x.co", nanoDay-1)
	if code, _ := poor.create(t, nil); code != 402 {
		t.Fatalf("underfunded create = %d, want 402", code)
	}

	// 24h of ONE nano VM only: a second VM would push total runway under 24h.
	tight := newVMUser(t, env, "tight@x.co", nanoDay+nanoDay/2)
	if code, out := tight.create(t, nil); code != 202 {
		t.Fatalf("first create = %d %v", code, out)
	}
	if code, _ := tight.create(t, nil); code != 402 {
		t.Fatalf("second create must account for the first VM, got %d", code)
	}

	// vm_limit (default 2).
	rich := newVMUser(t, env, "rich@x.co", 100*nanoDay)
	for i := 0; i < 2; i++ {
		if code, out := rich.create(t, nil); code != 202 {
			t.Fatalf("create %d = %d %v", i, code, out)
		}
	}
	if code, _ := rich.create(t, nil); code != 409 {
		t.Fatalf("over limit = %d, want 409", code)
	}
}

func TestCreateVMNoCapacityRollsBack(t *testing.T) {
	env := newTestEnv(t) // empty IP pool
	u := newVMUser(t, env, "a@x.co", 10*nanoDay)
	if code, _ := u.create(t, nil); code != 503 {
		t.Fatalf("no capacity = %d, want 503", code)
	}
	if n := count(t, env, `SELECT count(*) FROM vms`); n != 0 {
		t.Fatalf("VM rows left after rollback: %d", n)
	}
	if n := count(t, env, `SELECT count(*) FROM jobs`); n != 0 {
		t.Fatalf("jobs left after rollback: %d", n)
	}
}

func TestVMIsolationAndActions(t *testing.T) {
	env := newTestEnv(t)
	addIPs(t, env, 3)
	a := newVMUser(t, env, "a@x.co", 10*nanoDay)
	b := newVMUser(t, env, "b@x.co", 10*nanoDay)
	_, out := a.create(t, nil)
	id := int64(out["id"].(float64))
	path := fmt.Sprintf("/v1/vms/%d", id)

	if code, list := b.c.do("GET", "/v1/vms", nil, nil); code != 200 {
		t.Fatalf("list = %d %v", code, list)
	}
	for _, tc := range []struct{ method, path string }{
		{"GET", path}, {"DELETE", path}, {"POST", path + "/stop"}, {"POST", path + "/start"}, {"POST", path + "/reboot"},
	} {
		if code, _ := b.c.do(tc.method, tc.path, nil, b.c.csrfHdr()); code != 404 {
			t.Errorf("B %s %s = %d, want 404", tc.method, tc.path, code)
		}
	}
	if n := count(t, env, `SELECT count(*) FROM jobs WHERE kind <> 'vm.provision'`); n != 0 {
		t.Fatalf("another user's requests queued %d jobs", n)
	}

	// Power actions depend on state; the API only queues, the worker changes state.
	if code, _ := a.c.do("POST", path+"/stop", nil, a.c.csrfHdr()); code != 409 {
		t.Fatalf("stop while pending = %d, want 409", code)
	}
	set := func(state string) {
		if _, err := env.st.Pool.Exec(context.Background(), `UPDATE vms SET state=$2 WHERE id=$1`, id, state); err != nil {
			t.Fatal(err)
		}
	}
	set("running")
	for action, want := range map[string]int{"stop": 202, "reboot": 202, "start": 409} {
		if code, _ := a.c.do("POST", path+"/"+action, nil, a.c.csrfHdr()); code != want {
			t.Errorf("%s while running = %d, want %d", action, code, want)
		}
	}
	set("stopped")
	if code, _ := a.c.do("POST", path+"/start", nil, a.c.csrfHdr()); code != 202 {
		t.Errorf("start while stopped = %d", code)
	}
	set("suspended")
	if code, _ := a.c.do("POST", path+"/start", nil, a.c.csrfHdr()); code != 409 {
		t.Errorf("start while suspended = %d, want 409", code)
	}

	// Delete queues a job and is idempotent while deleting; deleted VMs vanish.
	if code, _ := a.c.do("DELETE", path, nil, a.c.csrfHdr()); code != 202 {
		t.Fatalf("delete = %d", code)
	}
	set("deleting")
	if code, _ := a.c.do("DELETE", path, nil, a.c.csrfHdr()); code != 202 {
		t.Fatalf("delete while deleting = %d", code)
	}
	if _, err := env.st.Pool.Exec(context.Background(), `UPDATE vms SET state='deleted', deleted_at=now() WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if code, _ := a.c.do("GET", path, nil, nil); code != 404 {
		t.Fatalf("get deleted = %d, want 404", code)
	}
}
