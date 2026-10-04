// Package sshkey validates and normalises user-supplied SSH public keys.
package sshkey

import (
	"errors"
	"strings"

	"golang.org/x/crypto/ssh"
)

var (
	ErrInvalid  = errors.New("not a valid OpenSSH public key")
	ErrOptions  = errors.New("key options are not allowed")
	ErrWeak     = errors.New("key is too weak (RSA needs at least 2048 bits)")
	ErrMultiple = errors.New("paste exactly one key")
)

// Parsed is a validated key in canonical form.
type Parsed struct {
	Normalized  string // "<type> <base64>", no comment or options; safe for cloud-init
	Fingerprint string // SHA256:...
}

// Parse accepts one authorized_keys-style line. Options such as command="..."
// are rejected because the key is injected into the VM's authorized_keys, and
// the comment is dropped so nothing user-controlled reaches cloud-init.
func Parse(raw string) (Parsed, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Parsed{}, ErrInvalid
	}
	if strings.ContainsAny(raw, "\r\n") {
		return Parsed{}, ErrMultiple
	}
	pub, _, options, _, err := ssh.ParseAuthorizedKey([]byte(raw))
	if err != nil {
		return Parsed{}, ErrInvalid
	}
	if len(options) > 0 {
		return Parsed{}, ErrOptions
	}
	switch pub.Type() {
	case ssh.KeyAlgoRSA:
		if cp, ok := pub.(ssh.CryptoPublicKey); ok {
			if k, ok := cp.CryptoPublicKey().(interface{ Size() int }); ok && k.Size()*8 < 2048 {
				return Parsed{}, ErrWeak
			}
		}
	case ssh.KeyAlgoDSA:
		return Parsed{}, ErrWeak
	}
	return Parsed{
		Normalized:  strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))),
		Fingerprint: ssh.FingerprintSHA256(pub),
	}, nil
}
