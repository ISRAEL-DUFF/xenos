//go:build sandbox

package billing

// Live checks against the iswallet SANDBOX. Not part of the normal test run:
//
//	set -a; . ./.env; set +a
//	go test -tags sandbox -run Sandbox -v -count=1 ./internal/billing
//
// Needs XENOS_ISPEND_URL and XENOS_ISPEND_API_KEY. Each run creates a fresh customer (the sandbox is
// never reset). Failures print iswallet's request id, which is what their support asks for.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"
)

func sandboxClient(t *testing.T) *ISWallet {
	url, key := os.Getenv("XENOS_ISPEND_URL"), os.Getenv("XENOS_ISPEND_API_KEY")
	if url == "" || key == "" {
		t.Skip("XENOS_ISPEND_URL / XENOS_ISPEND_API_KEY not set")
	}
	prefix := os.Getenv("XENOS_ISPEND_OWNER_PREFIX")
	if prefix == "" {
		prefix = "xenos-sbx"
	}
	c, err := NewISWallet(ISWalletConfig{BaseURL: url, APIKey: key, OwnerPrefix: prefix, USDTDecimals: 6})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func report(t *testing.T, what string, err error) {
	t.Helper()
	var api *APIError
	if errors.As(err, &api) {
		t.Logf("✗ %s: %v", what, api)
		return
	}
	t.Logf("✗ %s: %v", what, err)
}

func TestSandboxEndToEnd(t *testing.T) {
	c := sandboxClient(t)
	ctx := context.Background()
	stamp := strconv.FormatInt(time.Now().UnixNano(), 36)

	// 1. Customer and virtual account.
	cust, err := c.CreateCustomer(ctx, "sbx-signup:"+stamp, "user:"+stamp, "sbx-"+stamp+"@example.com", "+2348030000000")
	if err != nil {
		report(t, "create customer", err)
		t.FailNow()
	}
	t.Logf("✓ customer wallet=%s account=%s bank=%q name=%q", cust.ID, cust.VirtualAcct, cust.VirtualBank, cust.VirtualName)
	if cust.VirtualAcct == "" {
		t.Errorf("no virtual account was issued")
	}
	email := "sbx-" + stamp + "@example.com"
	if again, err := c.CreateCustomer(ctx, "sbx-signup:"+stamp, "user:"+stamp, email, "+2348030000000"); err != nil || again.ID != cust.ID {
		t.Errorf("re-creating with the same key and payload must return the same wallet: %+v %v", again, err)
	} else {
		t.Logf("✓ create customer is idempotent (same key, same payload)")
	}
	// A new key for the same owner_ref is WALLET_ALREADY_EXISTS. The guide says original_response
	// carries the existing wallet; the live sandbox returns an EMPTY one, so we cannot recover the id
	// and surface the 409. (Our stable per-user signup key avoids this path.)
	var api *APIError
	if again, err := c.CreateCustomer(ctx, "sbx-signup-b:"+stamp, "user:"+stamp, email, "+2348030000000"); !errors.As(err, &api) || api.Code != "WALLET_ALREADY_EXISTS" || again.ID != "" {
		t.Errorf("same owner_ref under a new key: got %+v %v, want the WALLET_ALREADY_EXISTS error", again, err)
	} else {
		t.Logf("✓ WALLET_ALREADY_EXISTS surfaced (its original_response is empty in the sandbox, contrary to the guide)")
	}
	// The same key with a different payload is refused.
	if _, err := c.CreateCustomer(ctx, "sbx-signup:"+stamp, "user:"+stamp, email, ""); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Errorf("same key, different payload: got %v, want IDEMPOTENCY_KEY_REUSED", err)
	} else {
		t.Logf("✓ IDEMPOTENCY_KEY_REUSED on a changed payload")
	}

	// 2. Balances and scale.
	b, err := c.Balances(ctx, cust.ID)
	if err != nil {
		report(t, "balances", err)
	} else {
		t.Logf("✓ balances of a new wallet: %+v (NGN scale and USDT scale were asserted)", b)
	}

	// 3. Rate.
	if k, err := c.Rate(ctx); err != nil {
		report(t, "rate", err)
	} else {
		t.Logf("✓ rate: %d kobo per USDT (₦%d.%02d)", k, k/100, k%100)
	}

	// 4. Operating wallet.
	if m, err := c.MerchantBalance(ctx); err != nil {
		report(t, "platform account", err)
	} else {
		w, _ := c.merchantWallet(ctx)
		t.Logf("✓ operating wallet %s holds %d micro-USDT", w, m)
	}

	// 5. Simulate a deposit.
	var dep map[string]any
	err = c.do(ctx, "POST", "/v1/sandbox/simulate/deposit", "sbx-dep:"+stamp, map[string]any{
		"account_number": cust.VirtualAcct, "amount": 5_000_000, "currency": "NGN", "sender_name": "Test Payer"}, &dep)
	if err != nil {
		report(t, "simulate deposit", err)
	} else {
		t.Logf("✓ simulated deposit: %v", dep)
	}
	var credited int64
	if b, err := c.Balances(ctx, cust.ID); err != nil {
		report(t, "balances after deposit", err)
	} else {
		credited = b.NGNKobo
		fee := int64(5_000_000) - credited
		t.Logf("✓ deposit is spendable: %+v. Sent 5,000,000 kobo, credited %d: a deposit fee of %d kobo (%.2f%%)", b, credited, fee, float64(fee)/50000)
	}

	// 6. Quote and convert (FX has been unavailable in the sandbox: retry briefly, then carry on).
	var q Quote
	for i := 0; i < 3; i++ {
		if q, err = c.Quote(ctx, cust.ID, credited); err == nil || !errors.Is(err, ErrQuoteUnavailable) {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		report(t, "quote", err)
		t.Logf("→ skipping conversion, charge and adjustment checks that need USDT")
		overcharge(t, c, ctx, stamp, cust.ID)
		return
	}
	t.Logf("✓ quote %s: ₦%.2f -> %.6f USDT at ₦%s, expires %s", q.ID, float64(q.AmountNGN)/100, float64(q.AmountUSDT)/1e6, q.Rate, q.ExpiresAt.Format(time.RFC3339))
	mv, err := c.Convert(ctx, "sbx-conv:"+stamp, cust.ID, q.ID)
	if err != nil {
		report(t, "convert", err)
		return
	}
	t.Logf("✓ convert %s: execute response credited %d micro-USDT; the quote promised %d", mv.ID, mv.CreditUUSDT, q.AmountUSDT)
	if mv.CreditUUSDT != 0 && mv.CreditUUSDT != q.AmountUSDT {
		t.Errorf("EXECUTE RESPONSE DISAGREES WITH THE QUOTE (%d vs %d)", mv.CreditUUSDT, q.AmountUSDT)
	}
	if again, err := c.Convert(ctx, "sbx-conv:"+stamp, cust.ID, q.ID); err != nil || again.ID != mv.ID {
		t.Errorf("replaying the convert must return the original: %+v %v", again, err)
	} else {
		t.Logf("✓ convert replay returns the original result")
	}
	b2, err := c.Balances(ctx, cust.ID)
	if err != nil {
		report(t, "balances after convert", err)
		return
	}
	t.Logf("✓ balances after convert: %+v", b2)
	if b2.USDTMicro != q.AmountUSDT || b2.NGNKobo != 0 {
		t.Errorf("balances do not match the quote: %+v", b2)
	}

	// 7. Used and expired quotes.
	if _, err := c.Convert(ctx, "sbx-conv2:"+stamp, cust.ID, q.ID); !errors.Is(err, ErrQuoteUsed) {
		t.Errorf("a used quote under a new key: got %v, want QUOTE_ALREADY_USED", err)
	} else {
		t.Logf("✓ QUOTE_ALREADY_USED")
	}

	// 8. Charge, replay, and insufficient funds.
	key := ChargeKey(7, time.Now())
	tr, err := c.Charge(ctx, key+stamp, cust.ID, 6000, "vm:7 sandbox")
	if err != nil {
		report(t, "charge", err)
	} else {
		t.Logf("✓ charged 0.006 USDT: transfer %s", tr.ID)
		if again, err := c.Charge(ctx, key+stamp, cust.ID, 6000, "vm:7 sandbox"); err != nil || again.ID != tr.ID {
			t.Errorf("charge replay: %+v %v", again, err)
		} else {
			t.Logf("✓ charge replay returns the original")
		}
		if _, err := c.Charge(ctx, key+stamp, cust.ID, 7000, "vm:7 sandbox"); !errors.Is(err, ErrIdempotencyKeyReused) {
			t.Errorf("same key, different amount: got %v, want IDEMPOTENCY_KEY_REUSED", err)
		} else {
			t.Logf("✓ IDEMPOTENCY_KEY_REUSED")
		}
		if b3, err := c.Balances(ctx, cust.ID); err == nil && b3.USDTMicro != b2.USDTMicro-6000 {
			t.Errorf("one charge must move exactly 6000 micro-USDT: %d -> %d", b2.USDTMicro, b3.USDTMicro)
		} else if err == nil {
			t.Logf("✓ exactly 0.006 USDT moved")
		}
	}
	overcharge(t, c, ctx, stamp, cust.ID)

	// 9. Adjustments both ways (credit comes from the operating wallet).
	if _, err := c.Adjust(ctx, "sbx-adj1:"+stamp, cust.ID, -1_000_000, "sandbox correction"); err != nil {
		report(t, "adjust (debit)", err)
	} else {
		t.Logf("✓ debit of 1 USDT collected into the operating wallet")
		if _, err := c.Adjust(ctx, "sbx-adj2:"+stamp, cust.ID, 500_000, "sandbox goodwill"); err != nil {
			report(t, "adjust (credit)", err)
		} else {
			t.Logf("✓ credit of 0.5 USDT paid from the operating wallet")
		}
	}
	fmt.Println()
}

// overcharge asks for far more USDT than the wallet can hold: it must be INSUFFICIENT_FUNDS.
func overcharge(t *testing.T, c *ISWallet, ctx context.Context, stamp, wallet string) {
	t.Helper()
	b, _ := c.Balances(ctx, wallet)
	_, err := c.Charge(ctx, "sbx-big:"+stamp, wallet, b.USDTMicro+1_000_000, "too much")
	switch {
	case errors.Is(err, ErrInsufficientFunds):
		t.Logf("✓ INSUFFICIENT_FUNDS")
	case errors.Is(err, ErrCurrencyMismatch) && b.USDTMicro == 0:
		t.Logf("✓ a customer who never converted gets CURRENCY_MISMATCH, not INSUFFICIENT_FUNDS (%v)", err)
	default:
		t.Errorf("overcharge: got %v", err)
	}
}
