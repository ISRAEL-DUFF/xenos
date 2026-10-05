package billing

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestFakeConvertIdempotentAndCharge(t *testing.T) {
	ctx := context.Background()
	f := NewFake(150_000) // ₦1,500 per USDT, in kobo
	c, _ := f.CreateCustomer(ctx, "k1", "user:1", "a@b.c", "")
	f.Deposit(c.ID, 500_000)
	q, err := f.Quote(ctx, c.ID, 500_000)
	if err != nil {
		t.Fatal(err)
	}
	m1, _ := f.Convert(ctx, "conv:1", c.ID, q.ID)
	m2, _ := f.Convert(ctx, "conv:1", c.ID, q.ID)
	if m1 != m2 || m1.CreditUUSDT != q.AmountUSDT {
		t.Fatalf("replayed convert must return the same movement: %+v %+v", m1, m2)
	}
	b, _ := f.Balances(ctx, c.ID)
	if b.NGNKobo != 0 || b.USDTMicro != q.AmountUSDT {
		t.Fatalf("unexpected balances %+v", b)
	}

	key := ChargeKey(7, time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC))
	if key != "vm:7:hour:2026100410" {
		t.Fatal(key)
	}
	for i := 0; i < 3; i++ { // replays charge once
		if _, err := f.Charge(ctx, key, c.ID, 6000, "vm:7 hour:2026100410"); err != nil {
			t.Fatal(err)
		}
	}
	if f.Merchant != 6000 || f.Narrations[key] != "vm:7 hour:2026100410" {
		t.Fatalf("merchant=%d narration=%q", f.Merchant, f.Narrations[key])
	}
	if _, err := f.Charge(ctx, "big", c.ID, 1<<60, "x"); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatal("expected insufficient funds")
	}
}

func TestFakeQuoteRules(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	f := NewFake(150_000)
	f.Now = func() time.Time { return now }
	c, _ := f.CreateCustomer(ctx, "k", "user:1", "a@b.c", "")
	f.Deposit(c.ID, 2_000_000)

	q, _ := f.Quote(ctx, c.ID, 500_000)
	if !q.ExpiresAt.Equal(now.Add(60 * time.Second)) {
		t.Fatalf("quotes live 60s, expires %v", q.ExpiresAt)
	}
	now = now.Add(61 * time.Second)
	if _, err := f.Convert(ctx, "c1", c.ID, q.ID); !errors.Is(err, ErrQuoteExpired) {
		t.Fatalf("stale quote = %v, want ErrQuoteExpired", err)
	}

	q2, _ := f.Quote(ctx, c.ID, 500_000)
	if _, err := f.Convert(ctx, "c2", c.ID, q2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Convert(ctx, "c3", c.ID, q2.ID); !errors.Is(err, ErrQuoteUsed) {
		t.Fatalf("a used quote under a new key = %v, want ErrQuoteUsed", err)
	}

	f.LiquidityShort = true
	q3, _ := f.Quote(ctx, c.ID, 500_000)
	if _, err := f.Convert(ctx, "c4", c.ID, q3.ID); !errors.Is(err, ErrLiquidity) {
		t.Fatalf("liquidity = %v", err)
	}
}

func TestFakeAdjustBothDirections(t *testing.T) {
	ctx := context.Background()
	f := NewFake(150_000)
	c, _ := f.CreateCustomer(ctx, "k", "user:1", "a@b.c", "")
	f.Credit(c.ID, 1_000_000)

	if _, err := f.Adjust(ctx, "a1", c.ID, 500_000, "goodwill"); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("a credit is paid from the merchant wallet, which is empty: %v", err)
	}
	f.FundMerchant(2_000_000)
	if _, err := f.Adjust(ctx, "a2", c.ID, 500_000, "goodwill"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Adjust(ctx, "a3", c.ID, -200_000, "correction"); err != nil {
		t.Fatal(err)
	}
	if b, _ := f.Balances(ctx, c.ID); b.USDTMicro != 1_300_000 || f.Merchant != 1_700_000 {
		t.Fatalf("balance=%d merchant=%d", b.USDTMicro, f.Merchant)
	}
	if _, err := f.Adjust(ctx, "a4", c.ID, -9_000_000, "too much"); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatal("a debit beyond the balance must fail")
	}
}

func TestFakeCreateCustomerIsIdempotentPerRef(t *testing.T) {
	ctx := context.Background()
	f := NewFake(150_000)
	a, _ := f.CreateCustomer(ctx, "k1", "user:1", "a@b.c", "")
	b, _ := f.CreateCustomer(ctx, "k2", "user:1", "a@b.c", "") // different key, same owner ref
	if a.ID != b.ID || a.VirtualAcct == "" {
		t.Fatalf("same owner ref must return the existing wallet: %+v %+v", a, b)
	}
}
