package httpapi

import (
	"context"
	"encoding/csv"
	"net/http"
	"strings"
	"testing"
	"time"
)

func charge(t *testing.T, env *testEnv, userID, vmID int64, hour time.Time, amount int64, status string) {
	t.Helper()
	if _, err := env.st.Pool.Exec(context.Background(),
		`INSERT INTO usage_charges (user_id, vm_id, hour, amount_uusdt, status) VALUES ($1, $2, $3, $4, $5)`, userID, vmID, hour, amount, status); err != nil {
		t.Fatal(err)
	}
}

func TestStatementTotalsMatchTheCharges(t *testing.T) {
	env := newTestEnv(t)
	addIPs(t, env, 4)
	a := newVMUser(t, env, "a@x.co", 100*nanoDay)
	b := newVMUser(t, env, "b@x.co", 100*nanoDay)
	_, outA := a.create(t, map[string]any{"hostname": "api", "labels": map[string]string{"pgdock.node": "n1"}})
	_, outA2 := a.create(t, map[string]any{"hostname": "web", "labels": map[string]string{"pgdock.node": "n2"}})
	_, outB := b.create(t, nil)
	ctx := context.Background()
	var uidA, uidB int64
	_ = env.st.Pool.QueryRow(ctx, `SELECT id FROM users WHERE email='a@x.co'`).Scan(&uidA)
	_ = env.st.Pool.QueryRow(ctx, `SELECT id FROM users WHERE email='b@x.co'`).Scan(&uidB)
	vm1, vm2, vmB := int64(outA["id"].(float64)), int64(outA2["id"].(float64)), int64(outB["id"].(float64))

	oct := func(day, hour int) time.Time { return time.Date(2026, 10, day, hour, 0, 0, 0, time.UTC) }
	// VM 1: 3 paid hours, one capped hour (0), one refunded; VM 2: 2 hours; a charge in September and one at the
	// very start of November must not appear in October. B's charge must not appear for A.
	charge(t, env, uidA, vm1, oct(1, 0), 6000, "paid")
	charge(t, env, uidA, vm1, oct(1, 1), 6000, "paid")
	charge(t, env, uidA, vm1, oct(31, 23), 6000, "unpaid")
	charge(t, env, uidA, vm1, oct(2, 0), 0, "paid")
	charge(t, env, uidA, vm1, oct(3, 0), 6000, "refunded")
	charge(t, env, uidA, vm2, oct(5, 5), 12000, "paid")
	charge(t, env, uidA, vm2, oct(5, 6), 12000, "paid")
	charge(t, env, uidA, vm1, time.Date(2026, 9, 30, 23, 0, 0, 0, time.UTC), 6000, "paid")
	charge(t, env, uidA, vm1, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), 6000, "paid")
	charge(t, env, uidB, vmB, oct(1, 0), 99999, "paid")
	if _, err := env.st.Pool.Exec(ctx, `INSERT INTO conversions (user_id, amount_ngn_kobo, amount_uusdt, rate, status, created_at) VALUES ($1, 500000, 3000000, 1375.5, 'complete', '2026-10-04T10:00:00Z')`, uidA); err != nil {
		t.Fatal(err)
	}
	if _, err := env.st.Pool.Exec(ctx, `INSERT INTO adjustments (admin_id, user_id, amount_uusdt, note, status, created_at) VALUES ($1, $1, 1000000, 'goodwill', 'complete', '2026-10-10T10:00:00Z')`, uidA); err != nil {
		t.Fatal(err)
	}

	code, out := a.c.do("GET", "/v1/statements/2026-10?group_by=label:pgdock.node", nil, nil)
	if code != 200 {
		t.Fatalf("statement = %d %v", code, out)
	}
	tot := out["totals"].(map[string]any)
	// charged: 6000*2 + 6000 (the unpaid one is still a charge) + 12000*2 = 42000; refunded 6000; capped 1.
	if tot["charged_uusdt"] != float64(42000) || tot["refunded_uusdt"] != float64(6000) || tot["charged_hours"] != float64(5) || tot["capped_hours"] != float64(1) || tot["unpaid_uusdt"] != float64(6000) {
		t.Fatalf("totals: %v", tot)
	}
	vms := out["vms"].([]any)
	if len(vms) != 2 {
		t.Fatalf("vms: %v", vms)
	}
	if g := out["groups"].([]any); len(g) != 2 || g[0].(map[string]any)["value"] != "n1" || g[0].(map[string]any)["charged_uusdt"] != float64(18000) || g[1].(map[string]any)["charged_uusdt"] != float64(24000) {
		t.Fatalf("groups: %v", out["groups"])
	}
	if c := out["conversions"].([]any); len(c) != 1 || c[0].(map[string]any)["amount_uusdt"] != float64(3000000) {
		t.Fatalf("conversions: %v", out["conversions"])
	}
	if adj := out["adjustments"].([]any); len(adj) != 1 || adj[0].(map[string]any)["note"] != "goodwill" {
		t.Fatalf("adjustments: %v", out["adjustments"])
	}

	// September and November are separate months; B only ever sees B.
	if _, o := a.c.do("GET", "/v1/statements/2026-09", nil, nil); o["totals"].(map[string]any)["charged_uusdt"] != float64(6000) {
		t.Fatalf("september: %v", o["totals"])
	}
	if _, o := b.c.do("GET", "/v1/statements/2026-10", nil, nil); o["totals"].(map[string]any)["charged_uusdt"] != float64(99999) || len(o["vms"].([]any)) != 1 {
		t.Fatalf("b's statement: %v", o)
	}
	code, months := a.c.do("GET", "/v1/statements", nil, nil)
	if got := months["months"].([]any); code != 200 || len(got) != 3 || got[0] != "2026-11" || got[2] != "2026-09" {
		t.Fatalf("months: %v", months)
	}

	// Bad input.
	for _, p := range []string{"/v1/statements/2026-13", "/v1/statements/oct", "/v1/statements/1999-01", "/v1/statements/2026-10?group_by=vm", "/v1/statements/2026-10?group_by=label:Bad Key"} {
		if code, _ := a.c.do("GET", p, nil, nil); code != 400 {
			t.Errorf("%s = %d, want 400", p, code)
		}
	}
	if code, _ := newClient(t, env.ts.URL).do("GET", "/v1/statements/2026-10", nil, nil); code != 401 {
		t.Errorf("anonymous = %d", code)
	}
}

func TestStatementCSV(t *testing.T) {
	env := newTestEnv(t)
	addIPs(t, env, 2)
	a := newVMUser(t, env, "a@x.co", 100*nanoDay)
	_, out := a.create(t, map[string]any{"hostname": "api", "labels": map[string]string{"role": "shared", "pgdock.node": "n1"}})
	var uid int64
	_ = env.st.Pool.QueryRow(context.Background(), `SELECT id FROM users WHERE email='a@x.co'`).Scan(&uid)
	vm := int64(out["id"].(float64))
	charge(t, env, uid, vm, time.Date(2026, 10, 1, 7, 0, 0, 0, time.UTC), 6000, "paid")
	charge(t, env, uid, vm, time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC), 1234567, "unpaid")

	req, _ := http.NewRequest("GET", env.ts.URL+"/v1/statements/2026-10?format=csv", nil)
	for _, c := range a.c.http.Jar.Cookies(req.URL) {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/csv") || !strings.Contains(resp.Header.Get("Content-Disposition"), "xenos-statement-2026-10.csv") {
		t.Fatalf("csv response: %d %v", resp.StatusCode, resp.Header)
	}
	rows, err := csv.NewReader(resp.Body).ReadAll()
	if err != nil || len(rows) != 3 {
		t.Fatalf("rows: %v %v", rows, err)
	}
	if rows[0][0] != "vm_id" || rows[1][1] != "api" {
		t.Fatalf("header %v, first row %v", rows[0], rows[1])
	}
	if rows[1][3] != "2026-10-01T07:00:00Z" || rows[1][4] != "6000" || rows[1][5] != "0.006000" || rows[2][5] != "1.234567" || rows[2][6] != "unpaid" {
		t.Fatalf("amounts: %v %v", rows[1], rows[2])
	}
	if rows[1][7] != "pgdock.node=n1;role=shared" {
		t.Fatalf("labels %q", rows[1][7])
	}
}

// An API token may read statements (PGDock attributes cost per node from them) but it is the owner's data only.
func TestStatementsAreReadableWithAnAPIToken(t *testing.T) {
	env := newTestEnv(t)
	a := newVMUser(t, env, "a@x.co", nanoDay)
	verify(t, env, "a@x.co")
	_, tok := mintToken(t, a, "pgdock")
	prog := newClient(t, env.ts.URL)
	if code, _ := prog.do("GET", "/v1/statements", nil, bearer(tok)); code != 200 {
		t.Fatalf("token list = %d", code)
	}
	if code, out := prog.do("GET", "/v1/statements/2026-10?group_by=label:pgdock.node", nil, bearer(tok)); code != 200 || out["month"] != "2026-10" {
		t.Fatalf("token statement = %d %v", code, out)
	}
}
