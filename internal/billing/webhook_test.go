package billing

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestParseWebhook(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	body := []byte(`{"wallet_id":"w1","amount":5000000,"currency":"NGN","balance_after":5000000,"txn_id":"tx9","operation_id":"op1","source_type":"pull_inflow","source_ref":"prov-1"}`)
	good := SignWebhook("s3cret", now, body, "dlv-1")

	ev, err := ParseWebhook("s3cret", good, body, now)
	if err != nil || ev.WalletID != "w1" || ev.Amount != 5000000 || ev.Currency != "NGN" || ev.TxnID != "tx9" ||
		ev.SourceType != SourceBankInflow || ev.DeliveryKey != "dlv-1" {
		t.Fatalf("good webhook: %+v %v", ev, err)
	}
	if ev.Type != EventCreditPosted || !ev.TypeInferred {
		t.Fatalf("a body with no type is inferred as a posted credit: %+v", ev)
	}
	if ev.DedupeKey() != "wallet.credit.posted:tx9" {
		t.Fatalf("dedupe key = %s", ev.DedupeKey())
	}

	hdr := func(mut func(http.Header)) http.Header {
		h := SignWebhook("s3cret", now, body, "d")
		mut(h)
		return h
	}
	cases := map[string]struct {
		secret string
		h      http.Header
		body   []byte
		now    time.Time
	}{
		"wrong secret":      {"other", good, body, now},
		"empty secret":      {"", good, body, now},
		"no signature":      {"s3cret", hdr(func(h http.Header) { h.Del(HeaderSignature) }), body, now},
		"no timestamp":      {"s3cret", hdr(func(h http.Header) { h.Del(HeaderTimestamp) }), body, now},
		"tampered body":     {"s3cret", good, append([]byte(" "), body...), now},
		"stale":             {"s3cret", good, body, now.Add(SignatureTolerance + time.Second)},
		"from the future":   {"s3cret", good, body, now.Add(-SignatureTolerance - time.Second)},
		"garbage sig":       {"s3cret", hdr(func(h http.Header) { h.Set(HeaderSignature, "sha256=zz") }), body, now},
		"timestamp swapped": {"s3cret", hdr(func(h http.Header) { h.Set(HeaderTimestamp, "1800000001") }), body, now},
	}
	for name, c := range cases {
		if _, err := ParseWebhook(c.secret, c.h, c.body, c.now); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: err = %v, want ErrBadSignature", name, err)
		}
	}
}

func TestParseWebhookReversalAndExplicitType(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	rev := []byte(`{"wallet_id":"w1","amount":5000000,"currency":"NGN","txn_id":"tx10","reversed_provider_reference":"prov-1","uncovered_amount":2000000}`)
	ev, err := ParseWebhook("s", SignWebhook("s", now, rev, "d2"), rev, now)
	if err != nil || ev.Type != EventCreditReversed || ev.ReversedProviderRef != "prov-1" || ev.UncoveredAmount != 2000000 {
		t.Fatalf("reversal: %+v %v", ev, err)
	}
	explicit := []byte(`{"event_type":"wallet.credit.posted","wallet_id":"w1","amount":1,"currency":"NGN"}`)
	if ev, _ := ParseWebhook("s", SignWebhook("s", now, explicit, "d3"), explicit, now); ev.Type != EventCreditPosted || ev.TypeInferred {
		t.Fatalf("an explicit type must be used as given: %+v", ev)
	}
	if ev.DedupeKey() == "" {
		t.Fatal("dedupe key must never be empty")
	}
}

func TestBalanceCache(t *testing.T) {
	ctx := context.Background()
	f := NewFake(150_000)
	c, _ := f.CreateCustomer(ctx, "k", "user:1", "a@b.c", "")
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
