package firewall

import (
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	out, err := Render("vmbr0", []string{"203.0.113.11", "203.0.113.10", "203.0.113.10"}, []string{"2001:db8::105"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`elements = { 203.0.113.10, 203.0.113.11 }`, // sorted, deduplicated
		`elements = { 2001:db8::105 }`,
		`iifname "vmbr0" ip saddr @allow_smtp4 tcp dport 25 accept`,
		`iifname "vmbr0" tcp dport 25 counter drop`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// The allow rules must come before the drop, or exemptions would never match.
	if strings.Index(out, "accept") > strings.Index(out, "drop") {
		t.Error("accept rules must precede the drop rule")
	}
}

func TestRenderEmptySetsAreValid(t *testing.T) {
	out, err := Render("vmbr0", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "elements") {
		t.Errorf("empty sets must omit elements (nft rejects `elements = {  }`):\n%s", out)
	}
}

func TestRenderRejectsInjection(t *testing.T) {
	if _, err := Render("vmbr0", []string{"1.2.3.4 } ; drop"}, nil); err == nil {
		t.Error("expected an error for a malformed address")
	}
	if _, err := Render(`vmbr0" ; flush ruleset ; "`, nil, nil); err == nil {
		t.Error("expected an error for a malformed bridge name")
	}
	if _, err := Render("vmbr0", []string{"2001:db8::1"}, nil); err == nil {
		t.Error("an IPv6 address must not be accepted in the IPv4 set")
	}
}
