package billing

import (
	"errors"
	"testing"
)

func TestMicroMinorConversion(t *testing.T) {
	// 3 decimals: 6,000 micro-USDT (our nano hourly price) is exactly 6 minor units.
	if got, err := MicroToMinor(6000, 3); err != nil || got != 6 {
		t.Fatalf("6000 micro @3dp = %d %v", got, err)
	}
	if _, err := MicroToMinor(6500, 3); !errors.Is(err, ErrUnrepresentable) {
		t.Fatal("a price finer than the minor unit must be refused, not rounded")
	}
	if got, _ := MicroToMinor(6000, 6); got != 6000 {
		t.Fatal("6dp is the identity")
	}
	if got, _ := MicroToMinor(6000, 8); got != 600_000 {
		t.Fatalf("8dp = %d", got)
	}
	if MinorToMicro(30781, 3) != 30_781_000 || MinorToMicro(30781, 6) != 30781 || MinorToMicro(3078199, 8) != 30781 {
		t.Fatal("minor -> micro")
	}
	if MinorToMicro(-5, 3) != -5000 {
		t.Fatal("negative balances (after a reversal) must convert too")
	}
}

func TestKoboPerUSDT(t *testing.T) {
	// The guide's example: effective rate 0.00061562 USDT per NGN  =>  ~₦1,624.46 per USDT.
	got, err := KoboPerUSDT("0.00061562")
	if err != nil || got != 162438 {
		t.Fatalf("kobo per USDT = %d %v, want 162438", got, err)
	}
	if NairaPerUSDT("0.00061562") != "1624.38" {
		t.Fatalf("display = %s", NairaPerUSDT("0.00061562"))
	}
	if k, _ := KoboPerUSDT("0.000625"); k != 160000 {
		t.Fatalf("market rate: %d", k) // ₦1,600.00
	}
	for _, bad := range []string{"", "0", "-1", "abc"} {
		if _, err := KoboPerUSDT(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestNairaPerUSDTFromAmounts(t *testing.T) {
	// The live sandbox: ₦49,300.00 became 35.861363 USDT, i.e. ₦1,374.74 per USDT. The quote's
	// rounded fx_rate would have displayed ₦1,375.52.
	if got := NairaPerUSDTFromAmounts(4_930_000, 35_861_363); got != "1374.74" {
		t.Fatalf("rate = %s", got)
	}
	if NairaPerUSDTFromAmounts(0, 1) != "" || NairaPerUSDTFromAmounts(1, 0) != "" {
		t.Fatal("no rate without both amounts")
	}
}
