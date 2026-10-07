package main

import "testing"

func TestParseUSDT(t *testing.T) {
	for in, want := range map[string]int64{"0.048": 48_000, "1": 1_000_000, "12.5": 12_500_000, "0.000001": 1, ".5": 500_000, "0": 0} {
		if got, err := parseUSDT(in); err != nil || got != want {
			t.Errorf("%q = %d %v, want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "-1", "1.1234567", "1,5", "9999999999"} {
		if _, err := parseUSDT(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
	if formatUSDT(48_000) != "0.048000" || formatSignedUSDT(-1_500_000) != "-1.500000" || formatSignedUSDT(5) != "+0.000005" {
		t.Error("formatting")
	}
}

func TestFlagHelpers(t *testing.T) {
	rest, yes := takeFlag([]string{"nano", "--yes", "0.01"}, "--yes")
	if !yes || len(rest) != 2 || rest[1] != "0.01" {
		t.Errorf("takeFlag: %v %v", rest, yes)
	}
	rest, v := takeValue([]string{"a", "--ci-user", "ubuntu", "b"}, "--ci-user")
	if v != "ubuntu" || len(rest) != 2 || rest[1] != "b" {
		t.Errorf("takeValue: %v %q", rest, v)
	}
}
