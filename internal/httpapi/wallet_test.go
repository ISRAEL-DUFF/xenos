package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/israel-duff/xenos/internal/billing"
	"github.com/israel-duff/xenos/internal/testutil"
)

const testWebhookSecret = "whsec_test"

func customerOf(t *testing.T, env *testEnv, email string) string {
	var cust string
	if err := env.st.Pool.QueryRow(context.Background(), `SELECT ispend_customer_id FROM users WHERE email=$1`, email).Scan(&cust); err != nil {
		t.Fatal(err)
	}
	return cust
}

// webhook posts a signed event the way iSpend would.
func (e *testEnv) webhook(t *testing.T, ev map[string]any, signedAt time.Time, secret string) int {
	t.Helper()
	body, _ := json.Marshal(ev)
	req, _ := http.NewRequest("POST", e.ts.URL+"/v1/webhooks/ispend", bytes.NewReader(body))
	if secret != "" {
		req.Header.Set(billing.SignatureHeader, billing.SignWebhook(secret, signedAt, body))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func depositEvent(id, cust string, kobo int64) map[string]any {
	return map[string]any{"id": id, "type": billing.EventDeposit, "customer_id": cust, "amount_kobo": kobo}
}

func (e *testEnv) drainWallet(t *testing.T) int {
	return testutil.DrainJobs(t, e.st, e.srv.Wallet.Handlers())
}

func verifyEmail(t *testing.T, env *testEnv, c *client) {
	t.Helper()
	if code, _ := c.do("POST", "/v1/auth/verify", map[string]any{"token": env.mailer.token(t)}, nil); code != 200 {
		t.Fatalf("verify = %d", code)
	}
}

func TestDepositAutoConvertsOnceEvenIfReplayed(t *testing.T) {
	env := newTestEnv(t)
	c := signupClient(t, env.ts.URL, "a@x.co")
	cust := customerOf(t, env, "a@x.co")
	env.ispend.Deposit(cust, 500_000) // ₦5,000 arrives at iSpend

	ev := depositEvent("evt_1", cust, 500_000)
	for i := 0; i < 3; i++ { // iSpend retries and replays
		if code := env.webhook(t, ev, time.Now(), testWebhookSecret); code != 200 {
			t.Fatalf("webhook %d = %d", i, code)
		}
		env.drainWallet(t)
	}
	bal, _ := env.ispend.Balances(context.Background(), cust)
	if bal.NGNKobo != 0 || bal.USDTMicro != 3_333_333 { // ₦5,000 at ₦1,500/USDT
		t.Fatalf("balances after replays: %+v", bal)
	}
	if n := count(t, env, `SELECT count(*) FROM conversions WHERE status='complete'`); n != 1 {
		t.Fatalf("conversions = %d, want exactly 1", n)
	}

	// A rate move afterwards changes the naira display, never the USDT already bought.
	env.ispend.RateKobo = 300_000
	env.webhook(t, ev, time.Now(), testWebhookSecret)
	env.drainWallet(t)
	bal2, _ := env.ispend.Balances(context.Background(), cust)
	if bal2.USDTMicro != bal.USDTMicro {
		t.Fatalf("USDT changed with the rate: %d -> %d", bal.USDTMicro, bal2.USDTMicro)
	}
	env.srv.Cache.Invalidate(cust)
	code, w := c.do("GET", "/v1/wallet", nil, nil)
	if code != 200 || int64(w["usdt_uusdt"].(float64)) != 3_333_333 || int64(w["usdt_in_ngn_kobo"].(float64)) != 3_333_333*300_000/1_000_000 {
		t.Fatalf("wallet = %d %v", code, w)
	}
}

func TestWebhookValidation(t *testing.T) {
	env := newTestEnv(t)
	signupClient(t, env.ts.URL, "a@x.co")
	cust := customerOf(t, env, "a@x.co")
	ev := depositEvent("evt_x", cust, 100_000)

	for name, code := range map[string]int{
		"no signature":    env.webhook(t, ev, time.Now(), ""),
		"wrong secret":    env.webhook(t, ev, time.Now(), "other"),
		"stale signature": env.webhook(t, ev, time.Now().Add(-time.Hour), testWebhookSecret),
	} {
		if code != 401 {
			t.Errorf("%s = %d, want 401", name, code)
		}
	}
	if code := env.webhook(t, depositEvent("e2", cust, 0), time.Now(), testWebhookSecret); code != 400 {
		t.Errorf("zero amount = %d, want 400", code)
	}
	if code := env.webhook(t, map[string]any{"id": "e3", "type": "something.else"}, time.Now(), testWebhookSecret); code != 200 {
		t.Errorf("unknown type = %d, want 200", code)
	}
	if code := env.webhook(t, depositEvent("e4", "cus_nobody", 100_000), time.Now(), testWebhookSecret); code != 200 {
		t.Errorf("unknown customer = %d, want 200", code)
	}
	if n := count(t, env, `SELECT count(*) FROM conversions`); n != 0 {
		t.Fatalf("rejected events created %d conversions", n)
	}
	if n := count(t, env, `SELECT count(*) FROM jobs`); n != 0 {
		t.Fatalf("rejected events queued %d jobs", n)
	}
}

func TestAutoConvertOffAndManualConversion(t *testing.T) {
	env := newTestEnv(t)
	c := signupClient(t, env.ts.URL, "a@x.co")
	cust := customerOf(t, env, "a@x.co")

	if code, out := c.do("PATCH", "/v1/wallet/settings", map[string]any{"auto_convert": false}, c.csrfHdr()); code != 200 || out["auto_convert"] != false {
		t.Fatalf("settings = %d %v", code, out)
	}
	env.ispend.Deposit(cust, 500_000)
	env.webhook(t, depositEvent("evt_m", cust, 500_000), time.Now(), testWebhookSecret)
	if n := env.drainWallet(t); n != 0 {
		t.Fatalf("auto-convert off must not queue conversions (ran %d)", n)
	}

	// Conversion needs a verified email.
	if code, _ := c.do("POST", "/v1/wallet/convert", map[string]any{"amount_ngn_kobo": 200_000}, c.csrfHdr()); code != 403 {
		t.Fatalf("unverified convert = %d, want 403", code)
	}
	verifyEmail(t, env, c)

	for name, tc := range map[string]struct {
		kobo int64
		want int
	}{"below minimum": {5_000, 400}, "more than balance": {600_000, 409}} {
		if code, _ := c.do("POST", "/v1/wallet/convert", map[string]any{"amount_ngn_kobo": tc.kobo}, c.csrfHdr()); code != tc.want {
			t.Errorf("%s = %d, want %d", name, code, tc.want)
		}
	}

	code, q := c.do("POST", "/v1/wallet/convert", map[string]any{"amount_ngn_kobo": 300_000}, c.csrfHdr())
	if code != 200 || int64(q["amount_uusdt"].(float64)) != 2_000_000 || q["rate"] != "1500.00" {
		t.Fatalf("quote = %d %v", code, q)
	}
	quoteID := q["quote_id"].(string)

	// Someone else cannot confirm this quote.
	other := signupClient(t, env.ts.URL, "b@x.co")
	verifyEmail(t, env, other)
	if code, _ := other.do("POST", "/v1/wallet/convert", map[string]any{"quote_id": quoteID}, other.csrfHdr()); code != 400 {
		t.Fatalf("foreign quote = %d, want 400", code)
	}

	// Confirming twice (a double click) converts once.
	code1, c1 := c.do("POST", "/v1/wallet/convert", map[string]any{"quote_id": quoteID}, c.csrfHdr())
	code2, c2 := c.do("POST", "/v1/wallet/convert", map[string]any{"quote_id": quoteID}, c.csrfHdr())
	if code1 != 200 || code2 != 200 || c1["id"] != c2["id"] || c1["status"] != "complete" {
		t.Fatalf("confirm = %d %v / %d %v", code1, c1, code2, c2)
	}
	bal, _ := env.ispend.Balances(context.Background(), cust)
	if bal.NGNKobo != 200_000 || bal.USDTMicro != 2_000_000 {
		t.Fatalf("balances = %+v", bal)
	}
	if n := count(t, env, `SELECT count(*) FROM conversions`); n != 1 {
		t.Fatalf("conversions = %d", n)
	}
}

func TestExpiredQuoteFailsInsteadOfChangingTheRate(t *testing.T) {
	env := newTestEnv(t)
	c := signupClient(t, env.ts.URL, "a@x.co")
	cust := customerOf(t, env, "a@x.co")
	verifyEmail(t, env, c)
	env.ispend.Deposit(cust, 500_000)

	_, q := c.do("POST", "/v1/wallet/convert", map[string]any{"amount_ngn_kobo": 300_000}, c.csrfHdr())
	env.ispend.ExpireQuote(q["quote_id"].(string))
	if code, _ := c.do("POST", "/v1/wallet/convert", map[string]any{"quote_id": q["quote_id"]}, c.csrfHdr()); code != 409 {
		t.Fatalf("expired quote = %d, want 409", code)
	}
	bal, _ := env.ispend.Balances(context.Background(), cust)
	if bal.NGNKobo != 500_000 || bal.USDTMicro != 0 {
		t.Fatalf("nothing should have converted: %+v", bal)
	}
}

func TestAutoConversionRetriedAfterOutage(t *testing.T) {
	env := newTestEnv(t)
	signupClient(t, env.ts.URL, "a@x.co")
	cust := customerOf(t, env, "a@x.co")
	env.ispend.Deposit(cust, 500_000)

	env.ispend.Down = true
	if code := env.webhook(t, depositEvent("evt_o", cust, 500_000), time.Now(), testWebhookSecret); code != 200 {
		t.Fatalf("webhook during outage = %d", code) // acknowledged: nothing needs iSpend yet
	}
	env.drainWallet(t) // job fails, exhausting its retries
	if n := count(t, env, `SELECT count(*) FROM conversions WHERE status='pending'`); n != 1 {
		t.Fatalf("conversion should still be pending, pending=%d", n)
	}
	env.ispend.Down = false
	if err := env.srv.Wallet.Sweep(context.Background(), env.srv.Jobs); err != nil {
		t.Fatal(err)
	}
	env.drainWallet(t)
	bal, _ := env.ispend.Balances(context.Background(), cust)
	if bal.NGNKobo != 0 || bal.USDTMicro != 3_333_333 {
		t.Fatalf("balances after recovery: %+v", bal)
	}
	if n := count(t, env, `SELECT count(*) FROM conversions WHERE status='complete'`); n != 1 {
		t.Fatalf("complete conversions = %d", n)
	}
	// Sweeping again finds nothing to do.
	_ = env.srv.Wallet.Sweep(context.Background(), env.srv.Jobs)
	if n := env.drainWallet(t); n != 0 {
		t.Fatalf("sweep re-queued a finished conversion (%d jobs)", n)
	}
}

func TestWalletOverview(t *testing.T) {
	env := newTestEnv(t)
	c := signupClient(t, env.ts.URL, "a@x.co")

	code, w := c.do("GET", "/v1/wallet", nil, nil)
	if code != 200 || w["virtual_account"] != nil || w["runway_hours"] != nil || w["email_verified"] != false {
		t.Fatalf("unverified wallet = %d %v", code, w)
	}
	verifyEmail(t, env, c)
	_, w = c.do("GET", "/v1/wallet", nil, nil)
	va, ok := w["virtual_account"].(map[string]any)
	if !ok || va["account_number"] == "" {
		t.Fatalf("verified wallet should show the virtual account: %v", w)
	}

	env.ispend.Down = true
	env.srv.Cache.Invalidate(customerOf(t, env, "a@x.co"))
	if code, _ := c.do("GET", "/v1/wallet", nil, nil); code != 503 {
		t.Fatalf("wallet during outage = %d, want 503", code)
	}
	env.ispend.Down = false

	anon := newClient(t, env.ts.URL)
	if code, _ := anon.do("GET", "/v1/wallet", nil, nil); code != 401 {
		t.Fatalf("anonymous wallet = %d", code)
	}
}
