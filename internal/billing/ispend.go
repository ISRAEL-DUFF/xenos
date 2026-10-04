// Package billing talks to the iSpend wallet service and meters VM usage.
// All amounts are int64 micro-USDT (uusdt) or NGN kobo; never floats.
package billing

import (
	"context"
	"errors"
)

var (
	ErrInsufficientFunds = errors.New("billing: insufficient funds")
	ErrQuoteUnavailable  = errors.New("billing: ispend cannot quote")
	ErrQuoteExpired      = errors.New("billing: quote expired")
)

type Customer struct {
	ID           string
	VirtualAcct  string // NGN virtual account number for bank transfer
	VirtualBank  string
	NGNWalletID  string
	USDTWalletID string
}

type Balances struct {
	NGNKobo   int64
	USDTMicro int64
}

type Quote struct {
	ID         string
	AmountNGN  int64  // kobo
	AmountUSDT int64  // micro-USDT
	Rate       string // NGN per USDT, decimal string for display
}

// Movement is the result of a ledger operation. Replaying a call with the same
// idempotency key must return the original Movement unchanged.
type Movement struct {
	ID string
}

// ISpend is the subset of the iSpend service API the VPS needs. Every mutating
// call carries an idempotency key.
type ISpend interface {
	CreateCustomer(ctx context.Context, key, email, phone string) (Customer, error)
	// Customer returns the customer's wallets and NGN virtual account.
	Customer(ctx context.Context, customerID string) (Customer, error)
	Balances(ctx context.Context, customerID string) (Balances, error)
	// Rate is the current NGN kobo per 1 USDT buy rate including the tenant spread. Display only.
	Rate(ctx context.Context) (int64, error)
	Quote(ctx context.Context, customerID string, amountNGN int64) (Quote, error)
	Convert(ctx context.Context, key, customerID, quoteID string) (Movement, error)
	// Charge moves USDT from the customer's wallet to the VPS merchant wallet.
	// Returns ErrInsufficientFunds when the wallet cannot cover amount.
	Charge(ctx context.Context, key, customerID string, amountUUSDT int64) (Movement, error)
	// Reverse undoes a prior movement (refunds), keyed by its own idempotency key.
	Reverse(ctx context.Context, key, movementID string) (Movement, error)
}
