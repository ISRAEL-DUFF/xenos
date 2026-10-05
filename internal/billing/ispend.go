// Package billing talks to the iswallet (iSpend) wallet service and meters VM usage.
// All amounts are int64 micro-USDT (uusdt) or NGN kobo; never floats. The real
// client converts to and from iswallet's own USDT minor unit at the edge.
package billing

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrInsufficientFunds: the wallet cannot cover the amount. Never returned for a transient fault.
	ErrInsufficientFunds = errors.New("billing: insufficient funds")
	// ErrQuoteUnavailable: rates are paused (FX_UNAVAILABLE). Conversion waits; deposits stay safe in the NGN balance.
	ErrQuoteUnavailable = errors.New("billing: ispend cannot quote")
	// ErrQuoteExpired: past the quote's 60 seconds. Retry with a NEW quote, never the stale id.
	ErrQuoteExpired = errors.New("billing: quote expired")
	// ErrQuoteUsed: the quote was already executed under a different idempotency key. Needs investigation.
	ErrQuoteUsed = errors.New("billing: quote already used")
	// ErrLiquidity: iswallet itself lacks the USDT to back the credit (INSUFFICIENT_LIQUIDITY). Not the
	// customer's fault: hold the conversion, retry later, tell iswallet.
	ErrLiquidity = errors.New("billing: iswallet has insufficient USDT liquidity")
	// ErrRateLimited: HTTP 429. Back off; there is no Retry-After.
	ErrRateLimited = errors.New("billing: rate limited by ispend")
	// ErrUnrepresentable: an amount cannot be expressed in iswallet's USDT minor unit without rounding.
	ErrUnrepresentable = errors.New("billing: amount not representable in the USDT minor unit")
)

// Customer is a customer's iswallet wallet. iswallet has one wallet per customer
// holding both NGN and USDT balances, so ID (the wallet id) is the only identity.
type Customer struct {
	ID          string
	VirtualAcct string // NUBAN for bank transfer; empty until issued
	VirtualBank string
	VirtualName string
}

type Balances struct {
	NGNKobo   int64
	USDTMicro int64
}

type Quote struct {
	ID         string
	AmountNGN  int64     // kobo debited
	AmountUSDT int64     // micro-USDT credited
	Rate       string    // NGN per USDT as a decimal string, for display
	ExpiresAt  time.Time // quotes live 60 seconds
}

// Movement is the result of a ledger operation. Replaying a call with the same
// idempotency key returns the original Movement unchanged.
type Movement struct {
	ID          string
	CreditUUSDT int64 // Convert only: the USDT actually credited, in micro-USDT
}

// ISpend is the subset of the iswallet API Xenos needs. Every mutating call
// carries an idempotency key.
type ISpend interface {
	// CreateCustomer creates the customer's wallet (ref is our stable id for them, e.g. "user:8821";
	// the client namespaces it) and issues its virtual account. It is idempotent: an existing wallet
	// is returned. If only the virtual-account step fails the Customer comes back without account
	// details and Customer() can fetch them later.
	CreateCustomer(ctx context.Context, key, ref, email, phone string) (Customer, error)
	// Customer ensures the wallet has a virtual account and returns it (idempotent).
	Customer(ctx context.Context, customerID string) (Customer, error)
	Balances(ctx context.Context, customerID string) (Balances, error)
	// Rate is the current NGN kobo per 1 USDT buy rate including iswallet's spread. Display only.
	Rate(ctx context.Context) (int64, error)
	Quote(ctx context.Context, customerID string, amountNGN int64) (Quote, error)
	Convert(ctx context.Context, key, customerID, quoteID string) (Movement, error)
	// Charge moves USDT from the customer's wallet to the Xenos merchant wallet. narration is
	// recorded on the ledger entry (iswallet has no structured metadata on transfers yet).
	Charge(ctx context.Context, key, customerID string, amountUUSDT int64, narration string) (Movement, error)
	// Adjust moves USDT between the merchant wallet and a customer's wallet on an admin's
	// instruction: positive credits the customer, negative debits them. note goes in the narration.
	Adjust(ctx context.Context, key, customerID string, amountUUSDT int64, note string) (Movement, error)
}
