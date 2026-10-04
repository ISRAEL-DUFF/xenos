package billing

import (
	"context"
	"fmt"
	"sync"
)

// Fake is an in-memory ISpend for local development and tests. Rate is NGN
// kobo per 1 USDT (e.g. 150_000_00 = ₦150,000/USDT).
type Fake struct {
	mu        sync.Mutex
	Rate      int64
	Down      bool // simulate an outage
	customers map[string]*fakeCustomer
	seen      map[string]Movement // idempotency key -> result
	quotes    map[string]Quote
	moves     map[string]fakeMove
	n         int
	Merchant  int64 // merchant USDT balance, micro-USDT
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
		Rate:      rateKoboPerUSDT,
		customers: map[string]*fakeCustomer{},
		seen:      map[string]Movement{},
		quotes:    map[string]Quote{},
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
	q := Quote{ID: f.next("q"), AmountNGN: amountNGN, AmountUSDT: amountNGN * 1_000_000 / f.Rate,
		Rate: fmt.Sprintf("%d.%02d", f.Rate/100, f.Rate%100)}
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
	q, ok := f.quotes[quoteID]
	if !ok {
		return Movement{}, fmt.Errorf("billing: unknown quote %s", quoteID)
	}
	c := f.customers[id]
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
	c := f.customers[id]
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
