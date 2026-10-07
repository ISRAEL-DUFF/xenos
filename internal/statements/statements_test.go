package statements

import "testing"

// Hostnames and labels are customer-controlled text: a cell a spreadsheet would read as a formula is defused.
func TestSafeCell(t *testing.T) {
	for in, want := range map[string]string{"web": "web", "": "", "=SUM(A1)": "'=SUM(A1)", "+1": "'+1", "-1": "'-1", "@x": "'@x", "a=b": "a=b"} {
		if got := safeCell(in); got != want {
			t.Errorf("safeCell(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseMonth(t *testing.T) {
	from, to, err := ParseMonth("2025-12")
	if err != nil || from.Format("2006-01-02") != "2025-12-01" || to.Format("2006-01-02") != "2026-01-01" {
		t.Fatalf("december: %v %v %v", from, to, err)
	}
	for _, bad := range []string{"", "2026", "2026-13", "oct 2026", "2019-12", "2999-01"} {
		if _, _, err := ParseMonth(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}
