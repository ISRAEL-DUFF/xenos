package billing

import (
	"context"
	"fmt"
	"sync"
)

// Fake is an in-memory ISpend for local development and tests. RateKobo is NGN
// kobo per 1 USDT (e.g. 150_000 = ₦1,500/USDT).
type Fake struct {
	mu        sync.Mutex
	RateKobo  int64 // NGN kobo per 1 USDT
	Down      bool  // simulate an outage
	customers map[string]*fakeCustomer
	seen      map[string]Movement // idempotency key -> result
	quotes    map[string]Quote
	expired   map[string]bool
	moves     map[string]fakeMove
	n         int
	Merchant  int64 // merchant USDT balance, micro-USDT
	// SignupCredit is given to each new customer (local development only).
	SignupCredit int64
}

type fakeCustomer struct {
	Customer
	ngn, usdt int64
}

type fakeMove struct {
	customerID string
	usdt       int64
	reversed   bool
}

func NewFake(rateKoboPerUSDT int64) *Fake {
	return &Fake{
		RateKobo:  rateKoboPerUSDT,
		customers: map[string]*fakeCustomer{},
		seen:      map[string]Movement{},
		quotes:    map[string]Quote{},
		expired:   map[string]bool{},
		moves:     map[string]fakeMove{},
	}
}

var errDown = fmt.Errorf("billing: ispend unreachable")

func (f *Fake) next(prefix string) string { f.n++; return fmt.Sprintf("%s_%d", prefix, f.n) }

func (f *Fake) CreateCustomer(_ context.Context, key, email, _ string) (Customer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Down {
		return Customer{}, errDown
	}
	if m, ok := f.seen[key]; ok {
		return f.customers[m.ID].Customer, nil
	}
	id := f.next("cus")
	c := &fakeCustomer{Customer: Customer{ID: id, VirtualAcct: "9900000000", VirtualBank: "FakeBank",
		NGNWalletID: id + "_ngn", USDTWalletID: id + "_usdt"}}
	c.usdt = f.SignupCredit
	f.customers[id] = c
	f.seen[key] = Movement{ID: id}
	return c.Customer, nil
}

// Deposit simulates inbound naira (test helper).
func (f *Fake) Deposit(customerID string, kobo int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.customers[customerID].ngn += kobo
}

func (f *Fake) Customer(_ context.Context, id string) (Customer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Down {
		return Customer{}, errDown
	}
	c, ok := f.customers[id]
	if !ok {
		return Customer{}, fmt.Errorf("billing: unknown customer %s", id)
	}
	return c.Customer, nil
}

func (f *Fake) Rate(context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Down {
		return 0, ErrQuoteUnavailable
	}
	return f.RateKobo, nil
}

func (f *Fake) Balances(_ context.Context, id string) (Balances, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Down {
		return Balances{}, errDown
	}
	c, ok := f.customers[id]
	if !ok {
		return Balances{}, fmt.Errorf("billing: unknown customer %s", id)
	}
	return Balances{NGNKobo: c.ngn, USDTMicro: c.usdt}, nil
}

func (f *Fake) Quote(_ context.Context, id string, amountNGN int64) (Quote, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Down {
		return Quote{}, ErrQuoteUnavailable
	}
	q := Quote{ID: f.next("q"), AmountNGN: amountNGN, AmountUSDT: amountNGN * 1_000_000 / f.RateKobo,
		Rate: fmt.Sprintf("%d.%02d", f.RateKobo/100, f.RateKobo%100)}
	f.quotes[q.ID] = q
	return q, nil
}

func (f *Fake) Convert(_ context.Context, key, id, quoteID string) (Movement, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Down {
		return Movement{}, errDown
	}
	if m, ok := f.seen[key]; ok {
		return m, nil
	}
	if f.expired[quoteID] {
		return Movement{}, ErrQuoteExpired
	}
	q, ok := f.quotes[quoteID]
	if !ok {
		return Movement{}, fmt.Errorf("billing: unknown quote %s", quoteID)
	}
	c, ok := f.customers[id]
	if !ok {
		return Movement{}, fmt.Errorf("billing: unknown customer %s", id)
	}
	if c.ngn < q.AmountNGN {
		return Movement{}, ErrInsufficientFunds
	}
	c.ngn -= q.AmountNGN
	c.usdt += q.AmountUSDT
	m := Movement{ID: f.next("mv")}
	f.seen[key] = m
	return m, nil
}

func (f *Fake) Charge(_ context.Context, key, id string, amt int64) (Movement, error) {
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
		return Movement{}, fmt.Errorf("billing: unknown customer %s", id)
	}
	if c.usdt < amt {
		return Movement{}, ErrInsufficientFunds
	}
	c.usdt -= amt
	f.Merchant += amt
	m := Movement{ID: f.next("mv")}
	f.moves[m.ID] = fakeMove{customerID: id, usdt: amt}
	f.seen[key] = m
	return m, nil
}

func (f *Fake) Reverse(_ context.Context, key, movementID string) (Movement, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Down {
		return Movement{}, errDown
	}
	if m, ok := f.seen[key]; ok {
		return m, nil
	}
	mv, ok := f.moves[movementID]
	if !ok || mv.reversed {
		return Movement{}, fmt.Errorf("billing: cannot reverse %s", movementID)
	}
	mv.reversed = true
	f.moves[movementID] = mv
	f.customers[mv.customerID].usdt += mv.usdt
	f.Merchant -= mv.usdt
	m := Movement{ID: f.next("mv")}
	f.seen[key] = m
	return m, nil
}

// Credit adds USDT directly to a customer's wallet (test helper).
func (f *Fake) Credit(customerID string, uusdt int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.customers[customerID].usdt += uusdt
}

// ExpireQuote makes Convert reject the quote (test helper).
func (f *Fake) ExpireQuote(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expired[id] = true
}

func (f *Fake) Adjust(_ context.Context, key, id string, amt int64, _ string) (Movement, error) {
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
		return Movement{}, fmt.Errorf("billing: unknown customer %s", id)
	}
	if c.usdt+amt < 0 {
		return Movement{}, ErrInsufficientFunds
	}
	c.usdt += amt
	f.Merchant -= amt
	m := Movement{ID: f.next("mv")}
	f.seen[key] = m
	return m, nil
}

func (f *Fake) CardTopUp(_ context.Context, key, id string, amountKobo int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Down {
		return "", errDown
	}
	if _, ok := f.customers[id]; !ok {
		return "", fmt.Errorf("billing: unknown customer %s", id)
	}
	return fmt.Sprintf("https://pay.example.test/checkout/%s?amount=%d", key, amountKobo), nil
}
