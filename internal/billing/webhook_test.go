package billing

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

const postedBody = `{"id":"evt_8f21","idempotency_key":"dlv-1","event_type":"wallet.credit.posted","schema_version":"v1",` +
	`"occurred_at":"2026-10-05T09:14:02Z","wallet_id":"w1","data":{"wallet_id":"w1","amount":5000000,"currency":"NGN",` +
	`"balance_after":5000000,"txn_id":"tx9","operation_id":"op1","source_type":"pull_inflow","source_ref":"prov-1"}}`

func TestParseWebhook(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	body := []byte(postedBody)
	good := SignWebhook("s3cret", now, body, "dlv-1")

	ev, err := ParseWebhook("s3cret", good, body, now)
	if err != nil || ev.ID != "evt_8f21" || ev.Type != EventCreditPosted || ev.WalletID != "w1" || ev.Amount != 5000000 ||
		ev.Currency != "NGN" || ev.TxnID != "tx9" || ev.SourceType != SourceBankInflow || ev.DeliveryKey != "dlv-1" ||
		ev.OccurredAt.IsZero() {
		t.Fatalf("good webhook: %+v %v", ev, err)
	}
	if ev.DedupeKey() != "wallet.credit.posted:tx9" {
		t.Fatalf("dedupe key = %s (the ledger txn id, not the delivery key)", ev.DedupeKey())
	}
	// Without a txn id the stable event id is the fallback.
	noTxn := []byte(`{"id":"evt_1","event_type":"wallet.credit.posted","wallet_id":"w","data":{"amount":1,"currency":"NGN"}}`)
	if e, _ := ParseWebhook("s", SignWebhook("s", now, noTxn, "d"), noTxn, now); e.DedupeKey() != "wallet.credit.posted:evt_1" {
		t.Fatalf("fallback dedupe key = %s", e.DedupeKey())
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

func TestParseWebhookReversal(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	rev := []byte(`{"id":"evt_2","event_type":"wallet.credit.reversed","wallet_id":"w1","data":{"wallet_id":"w1","amount":5000000,` +
		`"currency":"NGN","txn_id":"tx10","reversed_provider_reference":"prov-1","uncovered_amount":2000000}}`)
	ev, err := ParseWebhook("s", SignWebhook("s", now, rev, "d2"), rev, now)
	if err != nil || ev.Type != EventCreditReversed || ev.ReversedProviderRef != "prov-1" || ev.UncoveredAmount != 2000000 {
		t.Fatalf("reversal: %+v %v", ev, err)
	}
}

// We never infer an event's meaning from its shape: a validly signed body that does not say what it
// is must be refused, not guessed at (a reversal misread as a deposit would convert money we do not hold).
func TestParseWebhookRequiresAnEventType(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	for name, body := range map[string]string{
		"inner payload only": `{"wallet_id":"w","amount":1,"currency":"NGN","txn_id":"t","source_type":"pull_inflow"}`,
		"empty type":         `{"id":"e","event_type":"","wallet_id":"w","data":{"amount":1}}`,
		"not json":           `nope`,
	} {
		b := []byte(body)
		_, err := ParseWebhook("s", SignWebhook("s", now, b, "d"), b, now)
		if err == nil || errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: want a malformed-body error (not a signature error), got %v", name, err)
		}
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
