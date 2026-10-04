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
	c, _ := f.CreateCustomer(ctx, "k1", "a@b.c", "")
	f.Deposit(c.ID, 500_000)
	q, err := f.Quote(ctx, c.ID, 500_000)
	if err != nil {
		t.Fatal(err)
	}
	m1, _ := f.Convert(ctx, "conv:1", c.ID, q.ID)
	m2, _ := f.Convert(ctx, "conv:1", c.ID, q.ID)
	if m1 != m2 {
		t.Fatal("replayed convert must return same movement")
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
		if _, err := f.Charge(ctx, key, c.ID, 6000); err != nil {
			t.Fatal(err)
		}
	}
	if f.Merchant != 6000 {
		t.Fatalf("merchant=%d", f.Merchant)
	}
	if _, err := f.Charge(ctx, "big", c.ID, 1<<60); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatal("expected insufficient funds")
	}
}
