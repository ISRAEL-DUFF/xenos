package httpapi

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func verify(t *testing.T, env *testEnv, email string) {
	t.Helper()
	if _, err := env.st.Pool.Exec(context.Background(), `UPDATE users SET email_verified_at = now() WHERE email=$1`, email); err != nil {
		t.Fatal(err)
	}
}

func bearer(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }

func mintToken(t *testing.T, u *vmFixture, name string) (int64, string) {
	t.Helper()
	code, out := u.c.do("POST", "/v1/tokens", map[string]any{"name": name}, u.c.csrfHdr())
	if code != 201 {
		t.Fatalf("create token = %d %v", code, out)
	}
	return int64(out["id"].(float64)), out["token"].(string)
}

func TestAPITokenLifecycle(t *testing.T) {
	env := newTestEnv(t)
	addIPs(t, env, 3)
	u := newVMUser(t, env, "a@x.co", 100*nanoDay)
	verify(t, env, "a@x.co")

	id, tok := mintToken(t, u, "pgdock")
	if !strings.HasPrefix(tok, "xt_") {
		t.Fatalf("token %q", tok)
	}
	// The list shows a prefix, never the token.
	code, out := u.c.do("GET", "/v1/tokens", nil, nil)
	if code != 200 {
		t.Fatalf("list = %d", code)
	}
	if raw := fmt.Sprint(out); strings.Contains(raw, tok) {
		t.Fatal("the token must be shown once, at creation")
	}

	// The token manages VMs for its owner.
	prog := newClient(t, env.ts.URL)
	if code, _ := prog.do("GET", "/v1/vms", nil, bearer(tok)); code != 200 {
		t.Fatalf("token list vms = %d", code)
	}
	if code, out := prog.do("POST", "/v1/vms", map[string]any{"plan": "nano", "template": "ubuntu-24.04", "hostname": "node-1", "ssh_key_ids": []int64{u.key}}, bearer(tok)); code != 202 {
		t.Fatalf("token create vm = %d %v", code, out)
	}
	if code, _ := prog.do("GET", "/v1/wallet", nil, bearer(tok)); code != 200 {
		t.Fatalf("token read wallet = %d", code)
	}

	// ...but not the account, money, more tokens, the console or the admin area.
	for _, tc := range []struct{ method, path string }{
		{"POST", "/v1/tokens"}, {"GET", "/v1/tokens"}, {"POST", "/v1/auth/change-password"}, {"POST", "/v1/wallet/convert"},
		{"PATCH", "/v1/wallet/settings"}, {"POST", "/v1/vms/1/console"}, {"GET", "/v1/admin/users"}, {"POST", "/v1/auth/logout"},
	} {
		if code, _ := prog.do(tc.method, tc.path, map[string]any{"name": "x"}, bearer(tok)); code != 403 {
			t.Errorf("token %s %s = %d, want 403", tc.method, tc.path, code)
		}
	}

	// An unknown token is anonymous.
	if code, _ := prog.do("GET", "/v1/vms", nil, bearer("xt_nope")); code != 401 {
		t.Errorf("unknown token = %d, want 401", code)
	}
	// Revoking ends it at once; another user cannot revoke it.
	other := newVMUser(t, env, "b@x.co", nanoDay)
	if code, _ := other.c.do("DELETE", fmt.Sprintf("/v1/tokens/%d", id), nil, other.c.csrfHdr()); code != 404 {
		t.Errorf("another user's revoke = %d, want 404", code)
	}
	if code, _ := u.c.do("DELETE", fmt.Sprintf("/v1/tokens/%d", id), nil, u.c.csrfHdr()); code != 204 {
		t.Fatalf("revoke = %d", code)
	}
	if code, _ := prog.do("GET", "/v1/vms", nil, bearer(tok)); code != 401 {
		t.Errorf("revoked token = %d, want 401", code)
	}
}

func TestAPITokenLimitsAndExpiry(t *testing.T) {
	env := newTestEnv(t)
	u := newVMUser(t, env, "a@x.co", nanoDay)
	if code, _ := u.c.do("POST", "/v1/tokens", map[string]any{"name": "x"}, u.c.csrfHdr()); code != 403 {
		t.Errorf("an unverified account minted a token: %d", code)
	}
	verify(t, env, "a@x.co")
	if code, _ := u.c.do("POST", "/v1/tokens", map[string]any{"name": ""}, u.c.csrfHdr()); code != 400 {
		t.Errorf("empty name = %d", code)
	}
	if code, _ := u.c.do("POST", "/v1/tokens", map[string]any{"name": "x", "expires_in_days": 1000}, u.c.csrfHdr()); code != 400 {
		t.Errorf("1000 days = %d", code)
	}
	_, tok := mintToken(t, u, "t0")
	for i := 1; i < maxAPITokens; i++ {
		mintToken(t, u, fmt.Sprintf("t%d", i))
	}
	if code, _ := u.c.do("POST", "/v1/tokens", map[string]any{"name": "one too many"}, u.c.csrfHdr()); code != 409 {
		t.Errorf("11th token = %d, want 409", code)
	}

	prog := newClient(t, env.ts.URL)
	if _, err := env.st.Pool.Exec(context.Background(), `UPDATE api_tokens SET expires_at = now() - interval '1 minute'`); err != nil {
		t.Fatal(err)
	}
	if code, _ := prog.do("GET", "/v1/vms", nil, bearer(tok)); code != 401 {
		t.Errorf("expired token = %d, want 401", code)
	}
}

func TestAPITokenStopsWhenTheAccountIsBannedOrReset(t *testing.T) {
	env := newTestEnv(t)
	u := newVMUser(t, env, "a@x.co", nanoDay)
	verify(t, env, "a@x.co")
	_, tok := mintToken(t, u, "pgdock")
	prog := newClient(t, env.ts.URL)

	if _, err := env.st.Pool.Exec(context.Background(), `UPDATE users SET status='banned'`); err != nil {
		t.Fatal(err)
	}
	if code, _ := prog.do("GET", "/v1/vms", nil, bearer(tok)); code != 401 {
		t.Errorf("a banned account's token = %d, want 401", code)
	}
	if _, err := env.st.Pool.Exec(context.Background(), `UPDATE users SET status='active'`); err != nil {
		t.Fatal(err)
	}
	if code, _ := prog.do("GET", "/v1/vms", nil, bearer(tok)); code != 200 {
		t.Fatalf("restored account = %d", code)
	}

	// A password reset (the recovery path after a compromise) revokes every token.
	if code, _ := u.c.do("POST", "/v1/auth/forgot-password", map[string]any{"email": "a@x.co"}, nil); code != 202 {
		t.Fatalf("forgot = %d", code)
	}
	resetToken := env.mailer.token(t)
	if code, out := newClient(t, env.ts.URL).do("POST", "/v1/auth/reset-password", map[string]any{"token": resetToken, "password": "a-brand-new-password"}, nil); code != 200 {
		t.Fatalf("reset = %d %v", code, out)
	}
	if code, _ := prog.do("GET", "/v1/vms", nil, bearer(tok)); code != 401 {
		t.Errorf("a token must not survive a password reset: %d", code)
	}
}

func TestLabelsIdempotencyAndBootScriptOnCreate(t *testing.T) {
	env := newTestEnv(t)
	addIPs(t, env, 4)
	u := newVMUser(t, env, "a@x.co", 100*nanoDay)
	verify(t, env, "a@x.co")
	_, tok := mintToken(t, u, "pgdock")
	prog := newClient(t, env.ts.URL)
	create := func(key string, body map[string]any) (int, map[string]any) {
		b := map[string]any{"plan": "nano", "template": "ubuntu-24.04", "hostname": "node-1", "ssh_key_ids": []int64{u.key}}
		for k, v := range body {
			b[k] = v
		}
		h := bearer(tok)
		if key != "" {
			h["Idempotency-Key"] = key
		}
		return prog.do("POST", "/v1/vms", b, h)
	}

	for name, body := range map[string]map[string]any{
		"bad label key":   {"labels": map[string]string{"Bad Key": "x"}},
		"bad label value": {"labels": map[string]string{"role": "has space"}},
		"too many labels": {"labels": manyLabels(17)},
		"huge script":     {"boot_script": strings.Repeat("a", 17<<10)},
	} {
		if code, _ := create("", body); code != 400 {
			t.Errorf("%s = %d, want 400", name, code)
		}
	}

	code, out := create("create-node-1", map[string]any{"labels": map[string]string{"pgdock.node": "n1", "role": "shared"}, "boot_script": "#!/bin/sh\necho hi"})
	if code != 202 {
		t.Fatalf("create = %d %v", code, out)
	}
	id := out["id"]
	if l := out["labels"].(map[string]any); l["role"] != "shared" || l["pgdock.node"] != "n1" {
		t.Fatalf("labels %v", out["labels"])
	}
	if b := out["boot_script"].(map[string]any); b["status"] != "pending" {
		t.Fatalf("boot script %v", out["boot_script"])
	}
	if n := count(t, env, `SELECT count(*) FROM jobs WHERE kind='vm.provision'`); n != 1 {
		t.Fatalf("provision jobs %d", n)
	}

	// The same key replays; a different request under it is refused; a new key makes a new VM.
	code, again := create("create-node-1", map[string]any{"labels": map[string]string{"pgdock.node": "n1", "role": "shared"}})
	if code != 200 || again["id"] != id {
		t.Fatalf("replay = %d %v, want 200 with the same VM", code, again)
	}
	if code, _ := create("create-node-1", map[string]any{"plan": "small"}); code != 422 {
		t.Errorf("key reused for another request = %d, want 422", code)
	}
	if n := count(t, env, `SELECT count(*) FROM vms`); n != 1 {
		t.Fatalf("VMs after replays: %d, want 1", n)
	}
	if code, out := create("create-node-2", map[string]any{"hostname": "node-2", "labels": map[string]string{"role": "pooler"}}); code != 202 {
		t.Fatalf("second create = %d %v", code, out)
	}

	// Find by label; replace labels.
	prog2 := func(path string) []any {
		resp, err := prog.http.Do(mustReq(t, "GET", env.ts.URL+path, tok))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var list []any
		_ = jsonDecode(resp, &list)
		return list
	}
	if got := prog2("/v1/vms?label=role=shared"); len(got) != 1 {
		t.Fatalf("label filter returned %d VMs, want 1", len(got))
	}
	if got := prog2("/v1/vms?label=role=shared&label=pgdock.node=zzz"); len(got) != 0 {
		t.Fatalf("both labels must match: got %d", len(got))
	}
	if got := prog2("/v1/vms"); len(got) != 2 {
		t.Fatalf("unfiltered: %d", len(got))
	}
	if code, out := prog.do("PATCH", fmt.Sprintf("/v1/vms/%v", id), map[string]any{"labels": map[string]string{"role": "retired"}}, bearer(tok)); code != 200 || out["labels"].(map[string]any)["role"] != "retired" {
		t.Fatalf("patch labels = %d %v", code, out)
	}
	if got := prog2("/v1/vms?label=role=shared"); len(got) != 0 {
		t.Fatalf("the old label must be gone: %d", len(got))
	}
}

func manyLabels(n int) map[string]string {
	m := map[string]string{}
	for i := 0; i < n; i++ {
		m[fmt.Sprintf("k%d", i)] = "v"
	}
	return m
}
