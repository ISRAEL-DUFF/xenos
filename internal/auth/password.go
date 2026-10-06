// Package auth holds password hashing, opaque token helpers and rate limiting.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters (OWASP-recommended minimum: 19 MiB, t=2, p=1; we use a bit more).
const (
	argonTime    = 2
	argonMemKiB  = 32 * 1024
	argonThreads = 2
	argonKeyLen  = 32
	saltLen      = 16
)

var ErrBadHash = errors.New("auth: malformed password hash")

// hashSlots bounds concurrent argon2 runs: each needs 32 MiB, so an unbounded burst of login or reset
// requests could exhaust memory. Excess requests wait their turn instead of allocating.
var hashSlots = make(chan struct{}, 8)

func argonKey(pw string, salt []byte, t uint32, mem uint32, threads uint8, n uint32) []byte {
	hashSlots <- struct{}{}
	defer func() { <-hashSlots }()
	return argon2.IDKey([]byte(pw), salt, t, mem, threads, n)
}

func HashPassword(pw string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argonKey(pw, salt, argonTime, argonMemKiB, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemKiB, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// CheckPassword compares in constant time. A malformed hash returns an error.
func CheckPassword(pw, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, ErrBadHash
	}
	var version, mem, t int
	var p uint8
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false, ErrBadHash
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &t, &p); err != nil {
		return false, ErrBadHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, ErrBadHash
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, ErrBadHash
	}
	got := argonKey(pw, salt, uint32(t), uint32(mem), p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// DummyHash is verified against when a login names an unknown email, so
// response time does not reveal whether the account exists.
var DummyHash, _ = HashPassword("xenos-dummy-password")
