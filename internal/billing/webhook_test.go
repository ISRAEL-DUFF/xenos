package billing

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestParseWebhook(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	body := []byte(`{"id":"evt_1","type":"deposit.confirmed","customer_id":"cus_9","amount_kobo":500000}`)
	good := SignWebhook("s3cret", now, body)

	ev, err := ParseWebhook("s3cret", good, body, now)
	if err != nil || ev.ID != "evt_1" || ev.CustomerID != "cus_9" || ev.AmountKobo != 500000 {
		t.Fatalf("good webhook: %+v %v", ev, err)
	}

	cases := map[string]struct {
		secret, header string
		body           []byte
		now            time.Time
	}{
		"wrong secret":    {"other", good, body, now},
		"empty secret":    {"", good, body, now},
		"missing header":  {"s3cret", "", body, now},
		"tampered body":   {"s3cret", good, append([]byte(" "), body...), now},
		"stale":           {"s3cret", good, body, now.Add(SignatureTolerance + time.Second)},
		"from the future": {"s3cret", good, body, now.Add(-SignatureTolerance - time.Second)},
		"garbage":         {"s3cret", "t=abc,v1=zz", body, now},
	}
	for name, c := range cases {
		if _, err := ParseWebhook(c.secret, c.header, c.body, c.now); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: err = %v, want ErrBadSignature", name, err)
		}
	}
}

func TestBalanceCache(t *testing.T) {
	ctx := context.Background()
	f := NewFake(150_000)
	c, _ := f.CreateCustomer(ctx, "k", "a@b.c", "")
	now := time.Unix(0, 0)
	cache := NewBalanceCache(f, time.Minute)
	cache.Now = func() time.Time { return now }

	f.Credit(c.ID, 100)
	b, _ := cache.Get(ctx, c.ID)
	f.Credit(c.ID, 100)
	if b2, _ := cache.Get(ctx, c.ID); b2.USDTMicro != b.USDTMicro {
		t.Fatal("expected cached value within TTL")
	}
	now = now.Add(2 * time.Minute)
	if b3, _ := cache.Get(ctx, c.ID); b3.USDTMicro != 200 {
		t.Fatalf("expected refresh after TTL, got %d", b3.USDTMicro)
	}
	f.Credit(c.ID, 50)
	cache.Invalidate(c.ID)
	if b4, _ := cache.Get(ctx, c.ID); b4.USDTMicro != 250 {
		t.Fatalf("expected refresh after invalidate, got %d", b4.USDTMicro)
	}
}
