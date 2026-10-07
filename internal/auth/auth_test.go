package auth

import (
	"testing"
	"time"
)

func TestPasswordRoundTrip(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := CheckPassword("correct horse battery", h); !ok {
		t.Fatal("expected match")
	}
	if ok, _ := CheckPassword("wrong", h); ok {
		t.Fatal("expected mismatch")
	}
	if _, err := CheckPassword("x", "garbage"); err == nil {
		t.Fatal("expected error for malformed hash")
	}
}

func TestTokenHashStable(t *testing.T) {
	tok, h, _ := NewToken()
	if string(HashToken(tok)) != string(h) {
		t.Fatal("hash mismatch")
	}
	tok2, _, _ := NewToken()
	if tok == tok2 {
		t.Fatal("tokens must be unique")
	}
}

func TestLimiter(t *testing.T) {
	l := NewLimiter(3, time.Hour)
	now := time.Now()
	l.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if !l.Allow("ip") {
			t.Fatal("should allow")
		}
	}
	if l.Allow("ip") {
		t.Fatal("4th must be blocked")
	}
	if !l.Allow("other") {
		t.Fatal("separate key unaffected")
	}
	now = now.Add(2 * time.Hour)
	if !l.Allow("ip") {
		t.Fatal("window should reset")
	}
}
