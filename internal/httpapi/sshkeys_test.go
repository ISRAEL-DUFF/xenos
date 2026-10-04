package httpapi

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func newPubKey(t *testing.T) string {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	k, _ := ssh.NewPublicKey(pub)
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k)))
}

func signupClient(t *testing.T, base, email string) *client {
	c := newClient(t, base)
	if code, out := c.do("POST", "/v1/auth/signup", map[string]any{"email": email, "password": "long-enough-pw", "phone": "+2348012345678"}, nil); code != 201 {
		t.Fatalf("signup %s = %d %v", email, code, out)
	}
	return c
}

func TestSSHKeys(t *testing.T) {
	ts, _ := newTestServer(t)
	a := signupClient(t, ts.URL, "a@x.co")
	b := signupClient(t, ts.URL, "b@x.co")

	anon := newClient(t, ts.URL)
	if code, _ := anon.do("GET", "/v1/ssh-keys", nil, nil); code != 401 {
		t.Fatalf("anonymous list = %d", code)
	}

	key := newPubKey(t)
	code, out := a.do("POST", "/v1/ssh-keys", map[string]any{"name": "laptop", "public_key": key + " me@laptop"}, a.csrfHdr())
	if code != 201 || out["public_key"] != key || !strings.HasPrefix(out["fingerprint"].(string), "SHA256:") {
		t.Fatalf("create = %d %v", code, out)
	}
	id := int64(out["id"].(float64))

	if code, _ := a.do("POST", "/v1/ssh-keys", map[string]any{"name": "dup", "public_key": key}, a.csrfHdr()); code != 409 {
		t.Fatalf("duplicate = %d", code)
	}
	if code, _ := a.do("POST", "/v1/ssh-keys", map[string]any{"name": "x", "public_key": key}, nil); code != 403 {
		t.Fatalf("missing CSRF = %d", code)
	}
	for name, body := range map[string]map[string]any{
		"bad key":      {"name": "x", "public_key": "ssh-ed25519 nope"},
		"options":      {"name": "x", "public_key": `command="id" ` + newPubKey(t)},
		"empty name":   {"name": " ", "public_key": newPubKey(t)},
		"long name":    {"name": strings.Repeat("a", 65), "public_key": newPubKey(t)},
		"ctrl in name": {"name": "a\nb", "public_key": newPubKey(t)},
	} {
		if code, _ := a.do("POST", "/v1/ssh-keys", body, a.csrfHdr()); code != 400 {
			t.Errorf("%s = %d, want 400", name, code)
		}
	}

	// Another user can use the same key, but cannot see or delete A's.
	if code, _ := b.do("POST", "/v1/ssh-keys", map[string]any{"name": "same", "public_key": key}, b.csrfHdr()); code != 201 {
		t.Fatalf("B adds same key = %d", code)
	}
	if code, _ := b.do("DELETE", fmt.Sprintf("/v1/ssh-keys/%d", id), nil, b.csrfHdr()); code != 404 {
		t.Fatalf("B deleting A's key = %d", code)
	}
	if code, _ := a.do("DELETE", fmt.Sprintf("/v1/ssh-keys/%d", id), nil, a.csrfHdr()); code != 204 {
		t.Fatalf("A delete = %d", code)
	}
	if code, _ := a.do("DELETE", fmt.Sprintf("/v1/ssh-keys/%d", id), nil, a.csrfHdr()); code != 404 {
		t.Fatalf("second delete = %d", code)
	}
}

func TestSSHKeyLimit(t *testing.T) {
	ts, _ := newTestServer(t)
	a := signupClient(t, ts.URL, "a@x.co")
	for i := 0; i < maxSSHKeysPerUser; i++ {
		if code, _ := a.do("POST", "/v1/ssh-keys", map[string]any{"name": "k", "public_key": newPubKey(t)}, a.csrfHdr()); code != 201 {
			t.Fatalf("key %d = %d", i, code)
		}
	}
	if code, _ := a.do("POST", "/v1/ssh-keys", map[string]any{"name": "k", "public_key": newPubKey(t)}, a.csrfHdr()); code != 409 {
		t.Fatalf("over limit = %d", code)
	}
}
