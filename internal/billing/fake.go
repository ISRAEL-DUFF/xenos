package billing

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Fake is an in-memory ISpend for local development and tests. It mirrors the
// documented iswallet behaviour: one wallet per customer holding NGN and USDT,
// quotes that expire after 60 seconds and execute once, conversions that can be
// refused for platform liquidity, and transfers between wallets.
//
// RateKobo is NGN kobo per 1 USDT (150_000 = ₦1,500 per USDT).
type Fake struct {
	mu       sync.Mutex
	RateKobo int64
	Down     bool // simulate an outage
	// LiquidityShort makes Convert fail with ErrLiquidity, as when iswallet's USDT inventory is low.
	LiquidityShort bool
	// SignupCredit is given to each new customer's USDT balance (local development only).
	SignupCredit int64
	// DropConvertReply makes the next Convert EXECUTE but then fail as if the answer was lost in
	// transit: the situation that makes idempotent replay essential.
	DropConvertReply bool
	// ExpireNextQuote marks the next quote expired the moment it is issued.
	ExpireNextQuote bool
	// Now is the clock quotes expire against; defaults to time.Now.
	Now      func() time.Time
	QuoteTTL time.Duration // default 60s, as iswallet

	customers map[string]*fakeCustomer
	seen      map[string]Movement // idempotency key -> original result
	quotes    map[string]*fakeQuote
	n         int
	// Merchant is the Xenos merchant wallet's USDT balance, in micro-USDT.
	Merchant int64
	// Narrations records the narration of every distinct charge/adjustment, by idempotency key.
	Narrations map[string]string
}

type fakeCustomer struct {
	Customer
	ref       string
	ngn, usdt int64
}

type fakeQuote struct {
	Quote
	customer string
	usedBy   string // idempotency key that executed it
	expired  bool
}

func NewFake(rateKoboPerUSDT int64) *Fake {
	return &Fake{
		RateKobo:   rateKoboPerUSDT,
		QuoteTTL:   60 * time.Second,
		customers:  map[string]*fakeCustomer{},
		seen:       map[string]Movement{},
		quotes:     map[string]*fakeQuote{},
		Narrations: map[string]string{},
	}
}

var errDown = fmt.Errorf("billing: ispend unreachable")

func (f *Fake) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func (f *Fake) next(prefix string) string { f.n++; return fmt.Sprintf("%s_%d", prefix, f.n) }

func (f *Fake) CreateCustomer(_ context.Context, key, ref, _, _ string) (Customer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Down {
		return Customer{}, errDown
	}
	if m, ok := f.seen[key]; ok {
		return f.customers[m.ID].Customer, nil
	}
	for _, c := range f.customers { // same owner ref: the existing wallet, as WALLET_ALREADY_EXISTS
		if c.ref == ref {
			f.seen[key] = Movement{ID: c.ID}
			return c.Customer, nil
		}
	}
	id := f.next("wal")
	c := &fakeCustomer{ref: ref, usdt: f.SignupCredit, Customer: Customer{ID: id,
		VirtualAcct: fmt.Sprintf("99%08d", f.n), VirtualBank: "FakeBank", VirtualName: "XENOS CUSTOMER"}}
	f.customers[id] = c
	f.seen[key] = Movement{ID: id}
	return c.Customer, nil
}

func (f *Fake) Customer(_ context.Context, id string) (Customer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Down {
		return Customer{}, errDown
	}
	c, ok := f.customers[id]
	if !ok {
		return Customer{}, fmt.Errorf("billing: unknown wallet %s", id)
	}
	return c.Customer, nil
}

// Deposit simulates inbound naira arriving at the customer's virtual account (test helper).
func (f *Fake) Deposit(customerID string, kobo int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.customers[customerID].ngn += kobo
}

// Credit adds USDT directly to a customer's balance (test helper).
func (f *Fake) Credit(customerID string, uusdt int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.customers[customerID].usdt += uusdt
}

func (f *Fake) Balances(_ context.Context, id string) (Balances, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Down {
		return Balances{}, errDown
	}
	c, ok := f.customers[id]
	if !ok {
		return Balances{}, fmt.Errorf("billing: unknown wallet %s", id)
	}
	return Balances{NGNKobo: c.ngn, USDTMicro: c.usdt}, nil
}

func (f *Fake) Rate(context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Down {
		return 0, ErrQuoteUnavailable
	}
	return f.RateKobo, nil
}

func (f *Fake) Quote(_ context.Context, id string, amountNGN int64) (Quote, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Down {
		return Quote{}, ErrQuoteUnavailable
	}
	if _, ok := f.customers[id]; !ok {
		return Quote{}, fmt.Errorf("billing: unknown wallet %s", id)
	}
	ttl := f.QuoteTTL
	if ttl == 0 {
		ttl = 60 * time.Second
	}
	q := Quote{ID: f.next("cvq"), AmountNGN: amountNGN, AmountUSDT: amountNGN * 1_000_000 / f.RateKobo,
		Rate: fmt.Sprintf("%d.%02d", f.RateKobo/100, f.RateKobo%100), ExpiresAt: f.now().Add(ttl)}
	f.quotes[q.ID] = &fakeQuote{Quote: q, customer: id, expired: f.ExpireNextQuote}
	f.ExpireNextQuote = false
	return q, nil
}

func (f *Fake) Convert(_ context.Context, key, id, quoteID string) (Movement, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Down {
		return Movement{}, errDown
	}
	if m, ok := f.seen[key]; ok { // replay: the original result, even after the quote expired
		return m, nil
	}
	q, ok := f.quotes[quoteID]
	if !ok || q.customer != id {
		return Movement{}, fmt.Errorf("billing: unknown quote %s", quoteID)
	}
	if q.usedBy != "" {
		return Movement{}, ErrQuoteUsed
	}
	if q.expired || !f.now().Before(q.ExpiresAt) {
		return Movement{}, ErrQuoteExpired
	}
	if f.LiquidityShort {
		return Movement{}, ErrLiquidity
	}
	c := f.customers[id]
	if c.ngn < q.AmountNGN {
		return Movement{}, ErrInsufficientFunds
	}
	c.ngn -= q.AmountNGN
	c.usdt += q.AmountUSDT
	q.usedBy = key
	m := Movement{ID: f.next("cv"), CreditUUSDT: q.AmountUSDT}
	f.seen[key] = m
	if f.DropConvertReply {
		f.DropConvertReply = false
		return Movement{}, errDown
	}
	return m, nil
}

func (f *Fake) Charge(_ context.Context, key, id string, amt int64, narration string) (Movement, error) {
	return f.transfer(key, id, amt, narration, true)
}

func (f *Fake) Adjust(_ context.Context, key, id string, amt int64, note string) (Movement, error) {
	if amt >= 0 { // credit: merchant -> customer
		return f.transfer(key, id, amt, "adjustment: "+note, false)
	}
	return f.transfer(key, id, -amt, "adjustment: "+note, true)
}

// transfer moves amt micro-USDT between the customer and the merchant wallet.
// toMerchant: customer -> merchant (a charge or a debit); otherwise merchant -> customer.
func (f *Fake) transfer(key, id string, amt int64, narration string, toMerchant bool) (Movement, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Down {
		return Movement{}, errDown
	}
	if m, ok := f.seen[key]; ok {
		return m, nil
	}
	c, ok := f.customers[id]
	if !ok {
		return Movement{}, fmt.Errorf("billing: unknown wallet %s", id)
	}
	if toMerchant {
		if c.usdt < amt {
			return Movement{}, ErrInsufficientFunds
		}
		c.usdt -= amt
		f.Merchant += amt
	} else {
		if f.Merchant < amt {
			return Movement{}, ErrInsufficientFunds
		}
		f.Merchant -= amt
		c.usdt += amt
	}
	m := Movement{ID: f.next("tr")}
	f.seen[key] = m
	f.Narrations[key] = narration
	return m, nil
}

// ExpireQuote makes Convert reject the quote as expired (test helper).
func (f *Fake) ExpireQuote(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if q, ok := f.quotes[id]; ok {
		q.expired = true
	}
}

// FundMerchant adds USDT to the merchant wallet so credits and refunds can be paid (test helper).
func (f *Fake) FundMerchant(uusdt int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Merchant += uusdt
}

// Seen reports whether an idempotency key has been executed (test helper).
func (f *Fake) Seen(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.seen[key]
	return ok
}
