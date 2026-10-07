package httpapi

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/israel-duff/xenos/internal/accounts"
)

func postJSON(t *testing.T, env *testEnv, c *client, path string, body any, extra map[string]string) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", env.ts.URL+path, bytes.NewReader(b))
	for _, ck := range c.http.Jar.Cookies(req.URL) {
		req.AddCookie(ck)
	}
	req.Header.Set("X-CSRF-Token", c.csrf)
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestExportHoldsOnlyTheCallersDataAndNoSecrets(t *testing.T) {
	env := newTestEnv(t)
	addIPs(t, env, 4)
	a := newVMUser(t, env, "a@x.co", 100*nanoDay)
	b := newVMUser(t, env, "b@x.co", 100*nanoDay)
	verify(t, env, "a@x.co")
	_, tok := mintToken(t, a, "pgdock")
	_, outA := a.create(t, map[string]any{"hostname": "alpha"})
	b.create(t, map[string]any{"hostname": "bravo-secret"})
	var uidA int64
	_ = env.st.Pool.QueryRow(context.Background(), `SELECT id FROM users WHERE email='a@x.co'`).Scan(&uidA)
	charge(t, env, uidA, int64(outA["id"].(float64)), time.Date(2026, 10, 1, 7, 0, 0, 0, time.UTC), 6000, "paid")

	// The password is asked again; a token cannot export.
	if resp := postJSON(t, env, a.c, "/v1/account/export", map[string]any{"password": "wrong-password-1"}, nil); resp.StatusCode != 401 {
		t.Fatalf("wrong password = %d, want 401", resp.StatusCode)
	}
	if code, _ := newClient(t, env.ts.URL).do("POST", "/v1/account/export", map[string]any{"password": "long-enough-pw"}, bearer(tok)); code != 403 {
		t.Fatalf("an API token exporting = %d, want 403", code)
	}

	resp := postJSON(t, env, a.c, "/v1/account/export", map[string]any{"password": "long-enough-pw"}, nil)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/zip" {
		t.Fatalf("export = %d %v", resp.StatusCode, resp.Header)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		files[f.Name] = string(b)
	}
	for _, name := range []string{"profile.json", "ssh_keys.csv", "vms.csv", "charges.csv", "conversions.csv", "adjustments.csv", "api_tokens.csv"} {
		if _, ok := files[name]; !ok {
			t.Errorf("the export lacks %s (has %v)", name, zr.File)
		}
	}
	all := strings.Join([]string{files["profile.json"], files["vms.csv"], files["charges.csv"], files["api_tokens.csv"], files["ssh_keys.csv"]}, "\n")
	if !strings.Contains(files["profile.json"], "a@x.co") || !strings.Contains(files["vms.csv"], "alpha") || !strings.Contains(files["charges.csv"], "6000") || !strings.Contains(files["api_tokens.csv"], "pgdock") {
		t.Fatalf("the caller's own data is missing:\n%s", all)
	}
	if strings.Contains(all, "bravo-secret") || strings.Contains(all, "b@x.co") {
		t.Fatal("another account's data leaked into the export")
	}
	if strings.Contains(all, "argon2") || strings.Contains(all, tok) || strings.Contains(strings.ToLower(files["profile.json"]), "password") {
		t.Fatal("the export must contain no password hash and no token")
	}
	// Three a day.
	for i := 0; i < 2; i++ {
		postJSON(t, env, a.c, "/v1/account/export", map[string]any{"password": "long-enough-pw"}, nil).Body.Close()
	}
	if resp := postJSON(t, env, a.c, "/v1/account/export", map[string]any{"password": "long-enough-pw"}, nil); resp.StatusCode != 429 {
		t.Fatalf("the fourth export = %d, want 429", resp.StatusCode)
	}
}

func closeBody(email string, deleteVMs bool) map[string]any {
	return map[string]any{"password": "long-enough-pw", "email": email, "delete_vms": deleteVMs}
}

func TestClosingNeedsEverythingSettled(t *testing.T) {
	env := newTestEnv(t)
	addIPs(t, env, 3)
	a := newVMUser(t, env, "a@x.co", 100*nanoDay)
	_, out := a.create(t, nil)
	vmID := int64(out["id"].(float64))
	var uid int64
	_ = env.st.Pool.QueryRow(context.Background(), `SELECT id FROM users WHERE email='a@x.co'`).Scan(&uid)

	blockers := func(body map[string]any) []string {
		resp := postJSON(t, env, a.c, "/v1/account/close", body, nil)
		defer resp.Body.Close()
		if resp.StatusCode != 409 {
			t.Fatalf("close = %d, want 409", resp.StatusCode)
		}
		var o struct {
			Blockers []accounts.Blocker `json:"blockers"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&o)
		var codes []string
		for _, b := range o.Blockers {
			codes = append(codes, b.Code)
		}
		return codes
	}
	// A live VM, unpaid charges and a funded wallet each block (and are all reported together).
	charge(t, env, uid, vmID, time.Date(2026, 10, 1, 7, 0, 0, 0, time.UTC), 6000, "unpaid")
	if got := strings.Join(blockers(closeBody("a@x.co", false)), ","); got != "vms,unpaid,balance" {
		t.Fatalf("blockers = %s", got)
	}
	// Deleting the VMs for the customer removes that one; the balance is settled by hand (support), the unpaid by topping up.
	if got := strings.Join(blockers(closeBody("a@x.co", true)), ","); got != "unpaid,balance" {
		t.Fatalf("blockers with delete_vms = %s", got)
	}
	if n := count(t, env, `SELECT count(*) FROM jobs WHERE kind='vm.delete'`); n != 0 {
		t.Fatal("nothing may be deleted while the closure is refused")
	}

	// Bad inputs.
	if resp := postJSON(t, env, a.c, "/v1/account/close", map[string]any{"password": "nope-nope-nope", "email": "a@x.co"}, nil); resp.StatusCode != 401 {
		t.Fatalf("wrong password = %d", resp.StatusCode)
	}
	if resp := postJSON(t, env, a.c, "/v1/account/close", closeBody("someone@else.co", true), nil); resp.StatusCode != 400 {
		t.Fatalf("wrong email = %d", resp.StatusCode)
	}
}

func TestClosingAnAccountEndsAccessAndLeavesAGrace(t *testing.T) {
	env := newTestEnv(t)
	addIPs(t, env, 3)
	a := newVMUser(t, env, "a@x.co", 100*nanoDay)
	verify(t, env, "a@x.co")
	_, tok := mintToken(t, a, "pgdock")
	a.create(t, nil)
	// Nothing left to settle.
	cust := customerOf(t, env, "a@x.co")
	if _, err := env.ispend.Charge(context.Background(), "drain", cust, 100*nanoDay, "drain"); err != nil {
		t.Fatal(err)
	}

	resp := postJSON(t, env, a.c, "/v1/account/close", closeBody("A@X.co", true), nil)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("close = %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()
	if n := count(t, env, `SELECT count(*) FROM jobs WHERE kind='vm.delete'`); n != 1 {
		t.Fatalf("the VM must be queued for deletion: %d jobs", n)
	}
	if n := count(t, env, `SELECT count(*) FROM users WHERE email='a@x.co' AND status='closing' AND purge_after > now() + interval '29 days' AND NOT auto_convert`); n != 1 {
		t.Fatal("the account should be closing with a 30-day grace and auto-convert off")
	}
	if got := env.mailer.count("Your Xenos account is closed"); got != 1 {
		t.Fatalf("a confirmation email was sent %d times", got)
	}

	// Everything stops: the old session, the API token, a new login.
	if code, _ := a.c.do("GET", "/v1/auth/me", nil, nil); code != 401 {
		t.Errorf("the old session = %d, want 401", code)
	}
	prog := newClient(t, env.ts.URL)
	if code, _ := prog.do("GET", "/v1/vms", nil, bearer(tok)); code != 401 {
		t.Errorf("the API token = %d, want 401", code)
	}
	if code, out := newClient(t, env.ts.URL).do("POST", "/v1/auth/login", map[string]any{"email": "a@x.co", "password": "long-enough-pw"}, nil); code != 403 {
		t.Errorf("login = %d %v, want 403", code, out)
	}
	// Support can reopen it during the grace period.
	var uid int64
	_ = env.st.Pool.QueryRow(context.Background(), `SELECT id FROM users WHERE email='a@x.co'`).Scan(&uid)
	if err := accounts.Reopen(context.Background(), env.st, uid); err != nil {
		t.Fatal(err)
	}
	if code, _ := newClient(t, env.ts.URL).do("POST", "/v1/auth/login", map[string]any{"email": "a@x.co", "password": "long-enough-pw"}, nil); code != 200 {
		t.Errorf("login after reopening = %d", code)
	}
}

func TestPurgeAnonymisesAndKeepsTheFinancialRecords(t *testing.T) {
	env := newTestEnv(t)
	addIPs(t, env, 3)
	a := newVMUser(t, env, "a@x.co", 100*nanoDay)
	code, out := a.create(t, map[string]any{"hostname": "private-name", "labels": map[string]string{"owner": "ada"}})
	if code != 201 && code != 202 {
		t.Fatalf("create = %d %v", code, out)
	}
	vmID := int64(out["id"].(float64))
	ctx := context.Background()
	var uid int64
	_ = env.st.Pool.QueryRow(ctx, `SELECT id FROM users WHERE email='a@x.co'`).Scan(&uid)
	charge(t, env, uid, vmID, time.Date(2026, 10, 1, 7, 0, 0, 0, time.UTC), 6000, "paid")
	if _, err := env.st.Pool.Exec(ctx, `INSERT INTO admin_audit (admin_id, action, target, detail) VALUES (NULL, 'balance.adjust', 'a@x.co', '{}')`); err != nil {
		t.Fatal(err)
	}
	// The VM is already gone; the account asked to close and its grace has ended.
	if _, err := env.st.Pool.Exec(ctx, `UPDATE vms SET state='deleted', deleted_at=now()`); err != nil {
		t.Fatal(err)
	}
	if _, err := env.st.Pool.Exec(ctx, `UPDATE users SET status='closing', closing_at=now() - interval '31 days', purge_after=now() - interval '1 day' WHERE id=$1`, uid); err != nil {
		t.Fatal(err)
	}

	n, err := accounts.PurgeDue(ctx, env.st, time.Now())
	if err != nil || n != 1 {
		t.Fatalf("purge = %d %v", n, err)
	}
	var email, status, phone, host, labels string
	var keys, charges int
	_ = env.st.Pool.QueryRow(ctx, `SELECT email, status, phone FROM users WHERE id=$1`, uid).Scan(&email, &status, &phone)
	_ = env.st.Pool.QueryRow(ctx, `SELECT hostname, labels::text FROM vms WHERE id=$1`, vmID).Scan(&host, &labels)
	_ = env.st.Pool.QueryRow(ctx, `SELECT count(*) FROM ssh_keys WHERE user_id=$1`, uid).Scan(&keys)
	_ = env.st.Pool.QueryRow(ctx, `SELECT count(*) FROM usage_charges WHERE user_id=$1`, uid).Scan(&charges)
	if status != "closed" || !strings.HasPrefix(email, "closed-") || !strings.HasSuffix(email, "@invalid") || phone != "" {
		t.Fatalf("user row: %s %s %q", email, status, phone)
	}
	if strings.Contains(host, "private") || labels != "{}" || keys != 0 {
		t.Fatalf("personal data survived: host=%q labels=%s keys=%d", host, labels, keys)
	}
	if charges != 1 {
		t.Fatal("the financial records must be kept")
	}
	if c := count(t, env, `SELECT count(*) FROM admin_audit WHERE target='a@x.co'`); c != 0 {
		t.Fatal("the audit log still names the closed account's email")
	}
	if c := count(t, env, `SELECT count(*) FROM admin_audit WHERE action='account.purged'`); c != 1 {
		t.Fatalf("the purge itself is audited: %d", c)
	}
	// The email is free again.
	if _, err := signupClientE(env.ts.URL, "a@x.co"); err != nil {
		t.Fatal(err)
	}
	// Running it again does nothing.
	if n, _ := accounts.PurgeDue(ctx, env.st, time.Now()); n != 0 {
		t.Fatalf("second purge = %d", n)
	}
}

func TestPurgeWaitsForLiveVMs(t *testing.T) {
	env := newTestEnv(t)
	addIPs(t, env, 2)
	a := newVMUser(t, env, "a@x.co", 100*nanoDay)
	a.create(t, nil)
	var uid int64
	_ = env.st.Pool.QueryRow(context.Background(), `SELECT id FROM users WHERE email='a@x.co'`).Scan(&uid)
	if _, err := env.st.Pool.Exec(context.Background(), `UPDATE users SET status='closing', purge_after=now() - interval '1 day' WHERE id=$1`, uid); err != nil {
		t.Fatal(err)
	}
	if n, err := accounts.PurgeDue(context.Background(), env.st, time.Now()); err != nil || n != 0 {
		t.Fatalf("purge with a live VM = %d %v, want it to wait", n, err)
	}
}

func TestDepositForAClosingAccountIsNotConverted(t *testing.T) {
	env := newTestEnv(t)
	verifyEmail(t, env, signupClient(t, env.ts.URL, "a@x.co"))
	cust := customerOf(t, env, "a@x.co")
	if _, err := env.st.Pool.Exec(context.Background(), `UPDATE users SET status='closing'`); err != nil {
		t.Fatal(err)
	}
	env.ispend.Deposit(cust, 500_000)
	if code := env.webhook(t, depositEvent("late-1", cust, 500_000), time.Now(), testWebhookSecret); code != 200 {
		t.Fatalf("webhook = %d", code)
	}
	env.drainWallet(t)
	if n := count(t, env, `SELECT count(*) FROM conversions`); n != 0 {
		t.Fatal("money for a closing account must not be converted")
	}
	if got := env.mailer.count("arrived for a closing account"); got != 1 {
		t.Fatalf("operators must be told: %d alerts", got)
	}
}
