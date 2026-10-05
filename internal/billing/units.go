package billing

import (
	"fmt"
	"math/big"
)

// Xenos works in micro-USDT (6 decimals). iswallet works in its own USDT minor
// unit, whose scale must be configured explicitly: a wrong scale would charge
// customers 1000x too much or too little, so there is no default.

func pow10(n int) int64 {
	v := int64(1)
	for i := 0; i < n; i++ {
		v *= 10
	}
	return v
}

// MicroToMinor converts micro-USDT to iswallet minor units. It refuses to round: an
// amount that does not land exactly on a minor unit is an error, not a rounded charge.
func MicroToMinor(micro int64, decimals int) (int64, error) {
	switch {
	case decimals == 6:
		return micro, nil
	case decimals < 6:
		f := pow10(6 - decimals)
		if micro%f != 0 {
			return 0, fmt.Errorf("%w: %d micro-USDT with %d decimals", ErrUnrepresentable, micro, decimals)
		}
		return micro / f, nil
	default:
		return micro * pow10(decimals-6), nil
	}
}

// MinorToMicro converts iswallet minor units to micro-USDT. Extra precision beyond
// micro is truncated toward zero (balances and credits are never overstated).
func MinorToMicro(minor int64, decimals int) int64 {
	switch {
	case decimals == 6:
		return minor
	case decimals < 6:
		return minor * pow10(6-decimals)
	default:
		return minor / pow10(decimals-6)
	}
}

// KoboPerUSDT turns iswallet's rate (USDT per 1 NGN, a decimal string such as
// "0.00061562") into kobo per 1 USDT, rounded to the nearest kobo.
func KoboPerUSDT(usdtPerNGN string) (int64, error) {
	r, ok := new(big.Rat).SetString(usdtPerNGN)
	if !ok || r.Sign() <= 0 {
		return 0, fmt.Errorf("billing: bad rate %q", usdtPerNGN)
	}
	kobo := new(big.Rat).Quo(big.NewRat(100, 1), r) // 100 kobo per NGN / (USDT per NGN)
	n := new(big.Int).Add(new(big.Int).Mul(kobo.Num(), big.NewInt(2)), kobo.Denom())
	n.Quo(n, new(big.Int).Mul(kobo.Denom(), big.NewInt(2))) // round half up
	if !n.IsInt64() {
		return 0, fmt.Errorf("billing: rate %q out of range", usdtPerNGN)
	}
	return n.Int64(), nil
}

// NairaPerUSDT formats iswallet's rate as "NGN per USDT" with two decimals, for display.
func NairaPerUSDT(usdtPerNGN string) string {
	r, ok := new(big.Rat).SetString(usdtPerNGN)
	if !ok || r.Sign() <= 0 {
		return ""
	}
	return new(big.Rat).Inv(r).FloatString(2)
}
