package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/israel-duff/xenos/internal/proxmox"
)

func makeAdmin(t *testing.T, env *testEnv, email string) *client {
	t.Helper()
	c := signupClient(t, env.ts.URL, email)
	if _, err := env.st.Pool.Exec(context.Background(), `UPDATE users SET is_admin = TRUE WHERE email = $1`, email); err != nil {
		t.Fatal(err)
	}
	return c
}

func userID(t *testing.T, env *testEnv, email string) int64 {
	var id int64
	if err := env.st.Pool.QueryRow(context.Background(), `SELECT id FROM users WHERE email=$1`, email).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// Every route under /v1/admin must refuse non-admins. The routes are enumerated
// from the router itself, so a new admin route cannot be added without this check.
func TestEveryAdminRouteRejectsNonAdmins(t *testing.T) {
	env := newTestEnv(t)
	user := signupClient(t, env.ts.URL, "user@x.co")
	anon := newClient(t, env.ts.URL)

	routes, ok := env.srv.Router().(chi.Routes)
	if !ok {
		t.Fatal("router is not a chi.Routes")
	}
	var seen int
	err := chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(route, "/v1/admin/") {
			return nil
		}
		seen++
		path := strings.ReplaceAll(route, "{id}", "1")
		if code, _ := user.do(method, path, map[string]any{}, user.csrfHdr()); code != 403 {
			t.Errorf("non-admin %s %s = %d, want 403", method, path, code)
		}
		if code, _ := anon.do(method, path, map[string]any{}, nil); code != 401 {
			t.Errorf("anonymous %s %s = %d, want 401", method, path, code)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen < 10 {
		t.Fatalf("only found %d admin routes; route enumeration is broken", seen)
	}
}

func TestAdminUsersListSearchAndDetail(t *testing.T) {
	env := newTestEnv(t)
	admin := makeAdmin(t, env, "admin@x.co")
	newVMUser(t, env, "alice@x.co", 5_000_000)
	newVMUser(t, env, "bob@x.co", 0)

	code, _ := admin.do("GET", "/v1/admin/users?q=ali", nil, nil)
	if code != 200 {
		t.Fatalf("list = %d", code)
	}
	resp, err := admin.http.Get(env.ts.URL + "/v1/admin/users?q=ali")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var rows []map[string]any
	_ = decodeBody(resp, &rows)
	if len(rows) != 1 || rows[0]["email"] != "alice@x.co" || int64(rows[0]["usdt_uusdt"].(float64)) != 5_000_000 {
		t.Fatalf("search result = %v", rows)
	}
	// LIKE wildcards in the search are literal.
	resp2, _ := admin.http.Get(env.ts.URL + "/v1/admin/users?q=%25")
	var all []map[string]any
	_ = decodeBody(resp2, &all)
	resp2.Body.Close()
	if len(all) != 0 {
		t.Fatalf("a bare %% must not match every user: %v", all)
	}

	code, detail := admin.do("GET", fmt.Sprintf("/v1/admin/users/%d", userID(t, env, "alice@x.co")), nil, nil)
	if code != 200 || detail["user"].(map[string]any)["email"] != "alice@x.co" {
		t.Fatalf("detail = %d %v", code, detail)
	}
	if code, _ := admin.do("GET", "/v1/admin/users/99999", nil, nil); code != 404 {
		t.Fatalf("unknown user = %d", code)
	}
}

func TestAdminSuspendBanAndLimit(t *testing.T) {
	env := newTestEnv(t)
	addIPs(t, env, 3)
	admin := makeAdmin(t, env, "admin@x.co")
	victim := newVMUser(t, env, "v@x.co", 10*nanoDay)
	vid := userID(t, env, "v@x.co")
	path := fmt.Sprintf("/v1/admin/users/%d", vid)

	// vm_limit
	if code, out := admin.do("PATCH", path, map[string]any{"vm_limit": 5}, admin.csrfHdr()); code != 200 || out["vm_limit"] != float64(5) {
		t.Fatalf("raise limit = %d %v", code, out)
	}
	if code, _ := admin.do("PATCH", path, map[string]any{"vm_limit": 1000}, admin.csrfHdr()); code != 400 {
		t.Fatalf("absurd limit = %d, want 400", code)
	}
	if code, _ := admin.do("PATCH", path, map[string]any{"status": "weird"}, admin.csrfHdr()); code != 400 {
		t.Fatalf("bad status = %d", code)
	}
	if code, _ := admin.do("PATCH", path, map[string]any{}, admin.csrfHdr()); code != 400 {
		t.Fatalf("empty patch = %d", code)
	}

	// suspended accounts cannot create VMs; existing sessions keep working
	if code, _ := admin.do("PATCH", path, map[string]any{"status": "suspended"}, admin.csrfHdr()); code != 200 {
		t.Fatalf("suspend = %d", code)
	}
	if code, _ := victim.create(t, nil); code != 403 {
		t.Fatalf("suspended create = %d, want 403", code)
	}
	if code, _ := victim.c.do("GET", "/v1/wallet", nil, nil); code != 200 {
		t.Fatalf("suspended users can still see their wallet to top up, got %d", code)
	}
	if code, _ := admin.do("PATCH", path, map[string]any{"status": "active"}, admin.csrfHdr()); code != 200 {
		t.Fatal("reactivate")
	}

	// ban: sessions revoked, login refused, running VMs queued for suspension
	_, out := victim.create(t, nil)
	env.st.Pool.Exec(context.Background(), `UPDATE vms SET state='running' WHERE id=$1`, int64(out["id"].(float64)))
	if code, out := admin.do("PATCH", path, map[string]any{"status": "banned"}, admin.csrfHdr()); code != 200 || out["vms_suspending"] != float64(1) {
		t.Fatalf("ban = %d %v", code, out)
	}
	if code, _ := victim.c.do("GET", "/v1/auth/me", nil, nil); code != 401 {
		t.Fatalf("banned session = %d, want 401", code)
	}
	fresh := newClient(t, env.ts.URL)
	if code, _ := fresh.do("POST", "/v1/auth/login", map[string]any{"email": "v@x.co", "password": "long-enough-pw"}, nil); code != 403 {
		t.Fatalf("banned login = %d, want 403", code)
	}
	if n := count(t, env, `SELECT count(*) FROM jobs WHERE kind='vm.suspend'`); n != 1 {
		t.Fatalf("suspend jobs = %d", n)
	}

	// admins are protected from web-based status changes
	other := makeAdmin(t, env, "other@x.co")
	_ = other
	if code, _ := admin.do("PATCH", fmt.Sprintf("/v1/admin/users/%d", userID(t, env, "other@x.co")), map[string]any{"status": "banned"}, admin.csrfHdr()); code != 403 {
		t.Fatalf("banning an admin = %d, want 403", code)
	}
	if n := count(t, env, `SELECT count(*) FROM admin_audit`); n < 4 {
		t.Fatalf("admin actions must be audited, rows=%d", n)
	}
}

func TestAdminBalanceAdjustment(t *testing.T) {
	env := newTestEnv(t)
	admin := makeAdmin(t, env, "admin@x.co")
	newVMUser(t, env, "c@x.co", 1_000_000)
	cust := customerOf(t, env, "c@x.co")
	path := fmt.Sprintf("/v1/admin/users/%d/adjustments", userID(t, env, "c@x.co"))
	seq := 0
	post := func(in map[string]any) (int, map[string]any) {
		seq++
		in["request_id"] = fmt.Sprintf("req-test-%04d", seq)
		return admin.do("POST", path, in, admin.csrfHdr())
	}
	// A credit is paid out of the Xenos merchant wallet; a debit is collected into it.
	if code, out := post(map[string]any{"amount_uusdt": 100, "note": "merchant wallet is empty"}); code != 409 ||
		!strings.Contains(out["error"].(string), "merchant wallet") {
		t.Fatalf("credit from an empty merchant wallet = %d %v", code, out)
	}
	env.ispend.FundMerchant(10_000_000)

	bal := func() int64 { b, _ := env.ispend.Balances(context.Background(), cust); return b.USDTMicro }

	code, out := post(map[string]any{"amount_uusdt": 2_500_000, "note": "goodwill credit, outage 3 Oct"})
	if code != 201 || out["status"] != "complete" || bal() != 3_500_000 {
		t.Fatalf("credit = %d %v balance=%d", code, out, bal())
	}
	if code, _ := post(map[string]any{"amount_uusdt": -500_000, "note": "refund reversal"}); code != 201 || bal() != 3_000_000 {
		t.Fatalf("debit: balance=%d", bal())
	}

	for name, body := range map[string]map[string]any{
		"zero":         {"amount_uusdt": 0, "note": "nothing"},
		"no note":      {"amount_uusdt": 100, "note": " "},
		"short note":   {"amount_uusdt": 100, "note": "hi"},
		"over the cap": {"amount_uusdt": 2_000_000_000, "note": "typo with too many zeros"},
	} {
		if code, _ := post(body); code != 400 {
			t.Errorf("%s = %d, want 400", name, code)
		}
	}
	if code, _ := post(map[string]any{"amount_uusdt": -900_000_000, "note": "more than they have"}); code != 409 {
		t.Fatalf("overdraw = %d, want 409", code)
	}
	if bal() != 3_000_000 {
		t.Fatalf("balance changed by a rejected adjustment: %d", bal())
	}

	// Recorded with the note, the admin, and an audit entry; failures are kept too.
	if n := count(t, env, `SELECT count(*) FROM adjustments WHERE status='complete' AND note LIKE 'goodwill%'`); n != 1 {
		t.Fatal("adjustment not recorded with its note")
	}
	if n := count(t, env, `SELECT count(*) FROM adjustments WHERE status='failed'`); n != 2 {
		t.Fatalf("the two rejected adjustments should be recorded as failed, got %d", n)
	}
	if n := count(t, env, `SELECT count(*) FROM admin_audit WHERE action='balance.adjust'`); n != 2 {
		t.Fatalf("audit rows = %d", n)
	}
	_, detail := admin.do("GET", fmt.Sprintf("/v1/admin/users/%d", userID(t, env, "c@x.co")), nil, nil)
	if adj := detail["adjustments"].([]any); len(adj) != 4 {
		t.Fatalf("detail should list adjustments, got %v", adj)
	}
}

func TestAdminVMActionsAndPort25(t *testing.T) {
	env := newTestEnv(t)
	addIPs(t, env, 3)
	admin := makeAdmin(t, env, "admin@x.co")
	u := newVMUser(t, env, "u@x.co", 10*nanoDay)
	_, out := u.create(t, nil)
	id := int64(out["id"].(float64))
	set := func(state string) {
		env.st.Pool.Exec(context.Background(), `UPDATE vms SET state=$2 WHERE id=$1`, id, state)
	}

	resp, _ := admin.http.Get(env.ts.URL + "/v1/admin/vms?q=u@x.co")
	var rows []map[string]any
	_ = decodeBody(resp, &rows)
	resp.Body.Close()
	if len(rows) != 1 || rows[0]["owner"] != "u@x.co" || rows[0]["ipv4"] != "203.0.113.10" {
		t.Fatalf("vm list = %v", rows)
	}

	vmPath := fmt.Sprintf("/v1/admin/vms/%d", id)
	if code, _ := admin.do("POST", vmPath+"/stop", nil, admin.csrfHdr()); code != 409 {
		t.Fatalf("force-stop of a pending VM = %d, want 409", code)
	}
	set("running")
	if code, _ := admin.do("POST", vmPath+"/stop", nil, admin.csrfHdr()); code != 202 {
		t.Fatalf("force stop = %d", code)
	}
	code, p25 := admin.do("POST", vmPath+"/port25", map[string]any{"allow": true}, admin.csrfHdr())
	if code != 200 || p25["firewall_reload_required"] != true {
		t.Fatalf("port25 = %d %v", code, p25)
	}
	if n := count(t, env, `SELECT count(*) FROM vms WHERE port25_unblocked`); n != 1 {
		t.Fatal("port 25 flag not stored")
	}
	if code, _ := admin.do("POST", vmPath+"/port25", map[string]any{}, admin.csrfHdr()); code != 400 {
		t.Fatalf("port25 without allow = %d", code)
	}
	if code, _ := admin.do("DELETE", vmPath, nil, admin.csrfHdr()); code != 202 {
		t.Fatalf("force delete = %d", code)
	}
	if code, _ := admin.do("DELETE", "/v1/admin/vms/99999", nil, admin.csrfHdr()); code != 404 {
		t.Fatalf("unknown vm = %d", code)
	}
	if n := count(t, env, `SELECT count(*) FROM admin_audit WHERE action LIKE 'vm.%'`); n != 3 {
		t.Fatalf("vm audit rows = %d, want 3", n)
	}
}

func TestAdminCapacityJobsAndRevenue(t *testing.T) {
	env := newTestEnv(t)
	addIPs(t, env, 4)
	admin := makeAdmin(t, env, "admin@x.co")
	pve := proxmox.NewFake()
	pve.CPUs, pve.MemTotal, pve.Pool = 8, 8<<30, proxmox.Usage{Used: 30 << 30, Total: 100 << 30}
	env.srv.PVE = pve

	u := newVMUser(t, env, "u@x.co", 10*nanoDay)
	u.create(t, nil)
	_, cap := admin.do("GET", "/v1/admin/capacity", nil, nil)
	if cap["host_reachable"] != true || cap["vcpu"].(map[string]any)["committed"] != float64(1) ||
		cap["vcpu"].(map[string]any)["physical"] != float64(8) || cap["ram_mb"].(map[string]any)["physical"] != float64(8192) ||
		cap["ips"].(map[string]any)["free"] != float64(3) || cap["ips"].(map[string]any)["total"] != float64(4) ||
		cap["pool"].(map[string]any)["fraction"] != 0.3 {
		t.Fatalf("capacity = %v", cap)
	}
	env.srv.PVE = nil
	if _, cap = admin.do("GET", "/v1/admin/capacity", nil, nil); cap["host_reachable"] != false || cap["vcpu"].(map[string]any)["physical"] != nil {
		t.Fatalf("capacity without a host should say so: %v", cap)
	}

	// failed jobs can be retried
	env.st.Pool.Exec(context.Background(), `INSERT INTO jobs (kind, status, attempts, last_error) VALUES ('vm.provision','failed',3,'boom')`)
	resp, _ := admin.http.Get(env.ts.URL + "/v1/admin/jobs")
	var jobs []map[string]any
	_ = decodeBody(resp, &jobs)
	resp.Body.Close()
	if len(jobs) != 1 || jobs[0]["last_error"] != "boom" {
		t.Fatalf("jobs = %v", jobs)
	}
	jid := int64(jobs[0]["id"].(float64))
	if code, _ := admin.do("POST", fmt.Sprintf("/v1/admin/jobs/%d/retry", jid), nil, admin.csrfHdr()); code != 202 {
		t.Fatalf("retry = %d", code)
	}
	if code, _ := admin.do("POST", fmt.Sprintf("/v1/admin/jobs/%d/retry", jid), nil, admin.csrfHdr()); code != 404 {
		t.Fatalf("retrying a non-failed job = %d, want 404", code)
	}

	// revenue reflects conversions and paid usage
	uid := userID(t, env, "u@x.co")
	var vmID int64
	_ = env.st.Pool.QueryRow(context.Background(), `SELECT id FROM vms LIMIT 1`).Scan(&vmID)
	env.st.Pool.Exec(context.Background(), `INSERT INTO conversions (user_id, amount_ngn_kobo, amount_uusdt, status) VALUES ($1, 500000, 3333333, 'complete')`, uid)
	env.st.Pool.Exec(context.Background(), `INSERT INTO usage_charges (user_id, vm_id, hour, amount_uusdt, status) VALUES ($1,$2,date_trunc('hour', now()),6000,'paid')`, uid, vmID)
	code, rev := admin.do("GET", "/v1/admin/revenue?days=7", nil, nil)
	totals := rev["totals"].(map[string]any)
	if code != 200 || totals["converted_ngn_kobo"] != float64(500000) || totals["converted_uusdt"] != float64(3333333) || totals["usage_uusdt"] != float64(6000) {
		t.Fatalf("revenue = %d %v", code, rev)
	}
	if rev["merchant_uusdt"] == nil {
		t.Fatalf("revenue should show the operating wallet balance: %v", rev)
	}
	if len(rev["days"].([]any)) != 7 || rev["fx"].(map[string]any)["rate_kobo_per_usdt"] != float64(150000) {
		t.Fatalf("revenue days/fx = %v", rev)
	}
}

func TestVMMonthCost(t *testing.T) {
	env := newTestEnv(t)
	addIPs(t, env, 1)
	u := newVMUser(t, env, "u@x.co", 10*nanoDay)

	_, vmOut := u.create(t, nil)
	id := int64(vmOut["id"].(float64))
	env.st.Pool.Exec(context.Background(), `INSERT INTO usage_charges (user_id, vm_id, hour, amount_uusdt, status) VALUES ($1,$2,date_trunc('month', now()),6000,'paid'),($1,$2,date_trunc('month', now()) + interval '1 hour',6000,'paid')`, userID(t, env, "u@x.co"), id)
	_, detail := u.c.do("GET", fmt.Sprintf("/v1/vms/%d", id), nil, nil)
	if detail["month_cost_uusdt"] != float64(12000) {
		t.Fatalf("detail = %v", detail)
	}
	resp, _ := u.c.http.Get(env.ts.URL + "/v1/vms")
	var list []map[string]any
	_ = decodeBody(resp, &list)
	resp.Body.Close()
	if _, present := list[0]["month_cost_uusdt"]; present {
		t.Fatal("the list view should not carry per-VM monthly cost")
	}
}

func decodeBody(resp *http.Response, v any) error {
	return jsonDecode(resp, v)
}

func TestAdminAdjustmentRetriesAreIdempotent(t *testing.T) {
	env := newTestEnv(t)
	admin := makeAdmin(t, env, "admin@x.co")
	newVMUser(t, env, "c@x.co", 1_000_000)
	cust := customerOf(t, env, "c@x.co")
	env.ispend.FundMerchant(10_000_000)
	path := fmt.Sprintf("/v1/admin/users/%d/adjustments", userID(t, env, "c@x.co"))
	bal := func() int64 { b, _ := env.ispend.Balances(context.Background(), cust); return b.USDTMicro }
	body := map[string]any{"amount_uusdt": 2_000_000, "note": "goodwill", "request_id": "req-retry-0001"}

	if code, _ := admin.do("POST", path, body, admin.csrfHdr()); code != 201 || bal() != 3_000_000 {
		t.Fatalf("first = balance %d", bal())
	}
	if code, out := admin.do("POST", path, body, admin.csrfHdr()); code != 200 || out["status"] != "complete" || bal() != 3_000_000 {
		t.Fatalf("a retry must not pay again: %d %v balance=%d", code, out, bal())
	}
	if code, _ := admin.do("POST", path, map[string]any{"amount_uusdt": 3_000_000, "note": "goodwill", "request_id": "req-retry-0001"}, admin.csrfHdr()); code != 409 {
		t.Fatalf("the same request_id for a different amount = %d, want 409", code)
	}
	if code, _ := admin.do("POST", path, map[string]any{"amount_uusdt": 1, "note": "goodwill"}, admin.csrfHdr()); code != 400 {
		t.Fatalf("a missing request_id = %d, want 400", code)
	}
	if n := count(t, env, `SELECT count(*) FROM adjustments`); n != 1 {
		t.Fatalf("adjustments recorded: %d, want 1", n)
	}
}

func TestAdminAdjustmentDailyLimit(t *testing.T) {
	env := newTestEnv(t)
	admin := makeAdmin(t, env, "admin@x.co")
	newVMUser(t, env, "c@x.co", 1_000_000)
	env.ispend.FundMerchant(100_000_000_000)
	path := fmt.Sprintf("/v1/admin/users/%d/adjustments", userID(t, env, "c@x.co"))
	for i := 0; i < 5; i++ { // 5 x 1,000 USDT is exactly the daily limit
		if code, out := admin.do("POST", path, map[string]any{"amount_uusdt": 1_000_000_000, "note": "bulk", "request_id": fmt.Sprintf("req-day-%04d", i)}, admin.csrfHdr()); code != 201 {
			t.Fatalf("adjustment %d = %d %v", i, code, out)
		}
	}
	if code, _ := admin.do("POST", path, map[string]any{"amount_uusdt": 1_000_000, "note": "one more", "request_id": "req-day-9999"}, admin.csrfHdr()); code != 429 {
		t.Fatalf("over the daily limit = %d, want 429", code)
	}
}

func TestSuspendedAccountCannotStartThings(t *testing.T) {
	env := newTestEnv(t)
	admin := makeAdmin(t, env, "admin@x.co")
	user := newVMUser(t, env, "c@x.co", 1_000_000).c
	uid := userID(t, env, "c@x.co")
	if code, _ := admin.do("PATCH", fmt.Sprintf("/v1/admin/users/%d", uid), map[string]any{"status": "suspended"}, admin.csrfHdr()); code != 200 {
		t.Fatalf("suspend = %d", code)
	}
	for _, p := range []string{"/v1/vms/1/start", "/v1/vms/1/reboot"} {
		if code, _ := user.do("POST", p, map[string]any{}, user.csrfHdr()); code != 403 {
			t.Errorf("%s as a suspended user = %d, want 403", p, code)
		}
	}
	if code, _ := user.do("GET", "/v1/vms", nil, nil); code != 200 {
		t.Errorf("a suspended user can still read: %d", code)
	}
}

func TestAdminCatalogue(t *testing.T) {
	env := newTestEnv(t)
	admin := makeAdmin(t, env, "admin@x.co")
	user := signupClient(t, env.ts.URL, "u@x.co")
	pve := proxmox.NewFake()
	env.srv.PVE = pve

	// Only admins reach it.
	if code, _ := user.do("GET", "/v1/admin/plans", nil, nil); code != 403 {
		t.Fatalf("a customer on the catalogue = %d, want 403", code)
	}
	if code, _ := user.do("POST", "/v1/admin/plans", map[string]any{"slug": "x"}, user.csrfHdr()); code != 403 {
		t.Fatalf("a customer adding a plan = %d, want 403", code)
	}

	code, out := admin.do("POST", "/v1/admin/plans", map[string]any{"slug": "d-small", "vcpu": 2, "ram_mb": 4096, "disk_gb": 40, "price_uusdt_hourly": 48_000}, admin.csrfHdr())
	if code != 201 {
		t.Fatalf("add plan = %d %v", code, out)
	}
	if code, out := admin.do("POST", "/v1/admin/plans", map[string]any{"slug": "d-small", "vcpu": 2, "ram_mb": 4096, "disk_gb": 40, "price_uusdt_hourly": 48_000}, admin.csrfHdr()); code != 400 || !strings.Contains(out["error"].(string), "already exists") {
		t.Fatalf("duplicate plan = %d %v", code, out)
	}
	// Customers now see it.
	resp, _ := http.Get(env.ts.URL + "/v1/plans")
	var plans []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&plans)
	resp.Body.Close()
	seen := false
	for _, p := range plans {
		seen = seen || p["slug"] == "d-small"
	}
	if !seen {
		t.Fatal("the new plan must be offered")
	}

	// A price change reports its impact and applies only when confirmed.
	code, out = admin.do("POST", "/v1/admin/plans/d-small/price", map[string]any{"price_uusdt_hourly": 50_000}, admin.csrfHdr())
	if code != 200 || out["applied"] != false || out["new_hourly_uusdt"] != float64(50_000) {
		t.Fatalf("price preview = %d %v", code, out)
	}
	code, out = admin.do("POST", "/v1/admin/plans/d-small/price", map[string]any{"price_uusdt_hourly": 50_000, "confirm": true}, admin.csrfHdr())
	if code != 200 || out["applied"] != true {
		t.Fatalf("price applied = %d %v", code, out)
	}
	if code, _ := admin.do("POST", "/v1/admin/plans/d-small/price", map[string]any{"price_uusdt_hourly": 0, "confirm": true}, admin.csrfHdr()); code != 400 {
		t.Fatalf("a zero price = %d, want 400", code)
	}
	if code, _ := admin.do("POST", "/v1/admin/plans/ghost/price", map[string]any{"price_uusdt_hourly": 1, "confirm": true}, admin.csrfHdr()); code != 404 {
		t.Fatalf("an unknown plan = %d, want 404", code)
	}
	if code, _ := admin.do("POST", "/v1/admin/plans/d-small/active", map[string]any{"active": false}, admin.csrfHdr()); code != 200 {
		t.Fatalf("disable = %d", code)
	}
	if n := count(t, env, `SELECT count(*) FROM plans WHERE slug='d-small' AND active`); n != 0 {
		t.Fatal("the plan should be disabled")
	}

	// Templates: the VMID must exist on the host.
	body := map[string]any{"slug": "alma-9", "name": "AlmaLinux 9", "proxmox_template_id": 9002, "ci_user": "almalinux"}
	if code, _ := admin.do("POST", "/v1/admin/templates", body, admin.csrfHdr()); code != 400 {
		t.Fatalf("a VMID missing on the host = %d, want 400", code)
	}
	pve.VMs[9002] = &proxmox.FakeVM{ID: 9002}
	if code, out := admin.do("POST", "/v1/admin/templates", body, admin.csrfHdr()); code != 201 {
		t.Fatalf("add template = %d %v", code, out)
	}
	if n := count(t, env, `SELECT count(*) FROM admin_audit WHERE action IN ('plan.add','plan.price','plan.active','template.add') AND admin_id IS NOT NULL`); n != 4 {
		t.Fatalf("audit rows for the web actions: %d, want 4 (a preview is not audited)", n)
	}
}
