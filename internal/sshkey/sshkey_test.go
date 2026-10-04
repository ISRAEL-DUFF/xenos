package sshkey

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func authorized(t *testing.T, pub any) string {
	t.Helper()
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k)))
}

func TestParse(t *testing.T) {
	edPub, _, _ := ed25519.GenerateKey(rand.Reader)
	ed := authorized(t, edPub)
	rsa2048, _ := rsa.GenerateKey(rand.Reader, 2048)
	rsa1024, _ := rsa.GenerateKey(rand.Reader, 1024)

	p, err := Parse(ed + " me@laptop")
	if err != nil || p.Normalized != ed || !strings.HasPrefix(p.Fingerprint, "SHA256:") {
		t.Fatalf("ed25519 with comment: %+v %v", p, err)
	}
	if _, err := Parse(authorized(t, &rsa2048.PublicKey)); err != nil {
		t.Fatalf("rsa2048: %v", err)
	}
	cases := []struct {
		name, in string
		want     error
	}{
		{"empty", "", ErrInvalid},
		{"garbage", "ssh-ed25519 notbase64!!", ErrInvalid},
		{"options", `command="rm -rf /" ` + ed, ErrOptions},
		{"two keys", ed + "\n" + ed, ErrMultiple},
		{"weak rsa", authorized(t, &rsa1024.PublicKey), ErrWeak},
	}
	for _, c := range cases {
		if _, err := Parse(c.in); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v want %v", c.name, err, c.want)
		}
	}
}
