package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

// runningVM creates a VM for a fresh user and marks it running, as the worker would.
func runningVM(t *testing.T, env *testEnv, email string, balance int64) (*vmFixture, string) {
	t.Helper()
	addIPs(t, env, 3)
	u := newVMUser(t, env, email, balance)
	code, out := u.create(t, nil)
	if code != 202 {
		t.Fatalf("create = %d %v", code, out)
	}
	id := int64(out["id"].(float64))
	if _, err := env.st.Pool.Exec(context.Background(), `UPDATE vms SET state='running' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	return u, fmt.Sprintf("/v1/vms/%d", id)
}

func TestResizeValidation(t *testing.T) {
	env := newTestEnv(t)
	u, path := runningVM(t, env, "a@x.co", 100*nanoDay)

	for name, tc := range map[string]struct {
		plan string
		want int
	}{
		"same plan":    {"nano", 400},
		"unknown plan": {"huge", 400},
		"larger":       {"small", 202},
	} {
		if code, out := u.c.do("POST", path+"/resize", map[string]any{"plan": tc.plan}, u.c.csrfHdr()); code != tc.want {
			t.Errorf("%s = %d %v, want %d", name, code, out, tc.want)
		}
	}
	// The claim is held now: a second resize and any power action wait.
	if code, _ := u.c.do("POST", path+"/resize", map[string]any{"plan": "medium"}, u.c.csrfHdr()); code != 409 {
		t.Errorf("resize while resizing = %d, want 409", code)
	}
	if code, _ := u.c.do("POST", path+"/stop", nil, u.c.csrfHdr()); code != 409 {
		t.Errorf("stop while resizing = %d, want 409", code)
	}
	if _, out := u.c.do("GET", path, nil, nil); out["busy"] != "resizing" {
		t.Errorf("busy = %v", out["busy"])
	}
	if n := count(t, env, `SELECT count(*) FROM jobs WHERE kind='vm.resize'`); n != 1 {
		t.Errorf("resize jobs queued: %d", n)
	}
}

func TestResizeNeedsRunwayAndNeverShrinks(t *testing.T) {
	env := newTestEnv(t)
	u, path := runningVM(t, env, "a@x.co", 30*nanoDay/10) // 3 hours... of nano is plenty for nano, not for 24h of medium
	_ = u
	// 72 hours of nano pay for the nano VM (needs 24) but not 24 hours of medium (24000/h = 576000).
	if code, out := u.c.do("POST", path+"/resize", map[string]any{"plan": "medium"}, u.c.csrfHdr()); code != 402 {
		t.Fatalf("resize without the runway = %d %v, want 402", code, out)
	}
	// Move the VM to medium directly, then ask for the smaller plan back: never allowed.
	if _, err := env.st.Pool.Exec(context.Background(), `UPDATE vms SET plan_id = (SELECT id FROM plans WHERE slug='medium')`); err != nil {
		t.Fatal(err)
	}
	if code, _ := u.c.do("POST", path+"/resize", map[string]any{"plan": "small"}, u.c.csrfHdr()); code != 400 {
		t.Fatalf("shrinking = %d, want 400", code)
	}
}

func TestResizeIsOwnerOnlyAndNeedsASettledVM(t *testing.T) {
	env := newTestEnv(t)
	u, path := runningVM(t, env, "a@x.co", 100*nanoDay)
	other := newVMUser(t, env, "b@x.co", 100*nanoDay)
	if code, _ := other.c.do("POST", path+"/resize", map[string]any{"plan": "small"}, other.c.csrfHdr()); code != 404 {
		t.Errorf("another user's resize = %d, want 404", code)
	}
	if _, err := env.st.Pool.Exec(context.Background(), `UPDATE vms SET state='suspended'`); err != nil {
		t.Fatal(err)
	}
	if code, _ := u.c.do("POST", path+"/resize", map[string]any{"plan": "small"}, u.c.csrfHdr()); code != 409 {
		t.Errorf("resize of a suspended VM = %d, want 409", code)
	}
}

func TestSnapshotsAPI(t *testing.T) {
	env := newTestEnv(t)
	u, path := runningVM(t, env, "a@x.co", 100*nanoDay)
	other := newVMUser(t, env, "b@x.co", 100*nanoDay)

	snap := func(name string) (int, map[string]any) {
		return u.c.do("POST", path+"/snapshots", map[string]any{"name": name}, u.c.csrfHdr())
	}
	if code, _ := snap(" "); code != 400 {
		t.Errorf("empty name = %d, want 400", code)
	}
	code, out := snap("before upgrade")
	if code != 202 || out["status"] != "creating" {
		t.Fatalf("create = %d %v", code, out)
	}
	sid := int64(out["id"].(float64))
	// The VM is claimed while the snapshot is taken.
	if code, _ := snap("again"); code != 409 {
		t.Errorf("second snapshot while busy = %d, want 409", code)
	}
	if code, _ := u.c.do("POST", path+"/resize", map[string]any{"plan": "small"}, u.c.csrfHdr()); code != 409 {
		t.Errorf("resize while busy = %d, want 409", code)
	}

	// Pretend the worker finished: ready and released. Then the per-VM limit applies.
	ex := func(q string, a ...any) {
		if _, err := env.st.Pool.Exec(context.Background(), q, a...); err != nil {
			t.Fatal(err)
		}
	}
	ex(`UPDATE snapshots SET status='ready'`)
	ex(`UPDATE vms SET busy=NULL`)
	if code, _ := snap("second"); code != 202 {
		t.Fatalf("second snapshot = %d", code)
	}
	ex(`UPDATE snapshots SET status='ready'`)
	ex(`UPDATE vms SET busy=NULL`)
	if code, out := snap("third"); code != 409 {
		t.Fatalf("over the limit = %d %v, want 409", code, out)
	}
	if code, out := u.c.do("GET", path+"/snapshots", nil, nil); code != 200 || len(out["snapshots"].([]any)) != 2 {
		t.Fatalf("list = %d %v", code, out)
	}
	// A VM with snapshots cannot be resized.
	if code, _ := u.c.do("POST", path+"/resize", map[string]any{"plan": "small"}, u.c.csrfHdr()); code != 409 {
		t.Errorf("resize with snapshots = %d, want 409", code)
	}

	// Restore needs an explicit confirmation, and only the owner can use a snapshot.
	rp := fmt.Sprintf("%s/snapshots/%d/restore", path, sid)
	if code, _ := u.c.do("POST", rp, map[string]any{}, u.c.csrfHdr()); code != 400 {
		t.Errorf("restore without confirm = %d, want 400", code)
	}
	if code, _ := other.c.do("POST", rp, map[string]any{"confirm": true}, other.c.csrfHdr()); code != 404 {
		t.Errorf("another user's restore = %d, want 404", code)
	}
	if code, _ := other.c.do("GET", path+"/snapshots", nil, nil); code != 404 {
		t.Errorf("another user's list = %d, want 404", code)
	}
	if code, out := u.c.do("POST", rp, map[string]any{"confirm": true}, u.c.csrfHdr()); code != 202 {
		t.Fatalf("restore = %d %v", code, out)
	}
	if n := count(t, env, `SELECT count(*) FROM jobs WHERE kind='vm.restore'`); n != 1 {
		t.Errorf("restore jobs: %d", n)
	}
	ex(`UPDATE vms SET busy=NULL`)
	if code, _ := u.c.do("DELETE", fmt.Sprintf("%s/snapshots/%d", path, sid), nil, u.c.csrfHdr()); code != 202 {
		t.Errorf("delete snapshot = %d", code)
	}
	// A suspended account cannot take or restore snapshots.
	ex(`UPDATE users SET status='suspended'`)
	ex(`UPDATE vms SET busy=NULL`)
	if code, _ := snap("blocked"); code != 403 {
		t.Errorf("snapshot as a suspended user = %d, want 403", code)
	}
}

func TestRebuildAPI(t *testing.T) {
	env := newTestEnv(t)
	u, path := runningVM(t, env, "a@x.co", 100*nanoDay)
	other := newVMUser(t, env, "b@x.co", 100*nanoDay)

	if code, _ := u.c.do("POST", path+"/rebuild", map[string]any{"template": "debian-12"}, u.c.csrfHdr()); code != 400 {
		t.Errorf("rebuild without confirm = %d, want 400", code)
	}
	if code, _ := u.c.do("POST", path+"/rebuild", map[string]any{"template": "nope", "confirm": true}, u.c.csrfHdr()); code != 400 {
		t.Errorf("unknown template = %d, want 400", code)
	}
	if code, _ := u.c.do("POST", path+"/rebuild", map[string]any{"ssh_key_ids": []int64{other.key}, "confirm": true}, u.c.csrfHdr()); code != 400 {
		t.Errorf("another user's SSH key = %d, want 400", code)
	}
	if code, _ := other.c.do("POST", path+"/rebuild", map[string]any{"confirm": true}, other.c.csrfHdr()); code != 404 {
		t.Errorf("another user's rebuild = %d, want 404", code)
	}
	// Default template and keys: the VM's own.
	if code, out := u.c.do("POST", path+"/rebuild", map[string]any{"template": "debian-12", "ssh_key_ids": []int64{u.key}, "confirm": true}, u.c.csrfHdr()); code != 202 {
		t.Fatalf("rebuild = %d %v", code, out)
	}
	if code, _ := u.c.do("POST", path+"/rebuild", map[string]any{"confirm": true}, u.c.csrfHdr()); code != 409 {
		t.Errorf("a second rebuild while busy = %d, want 409", code)
	}
	if code, _ := u.c.do("POST", path+"/stop", nil, u.c.csrfHdr()); code != 409 {
		t.Errorf("stop while rebuilding = %d, want 409", code)
	}
	if n := count(t, env, `SELECT count(*) FROM jobs WHERE kind='vm.rebuild'`); n != 1 {
		t.Errorf("rebuild jobs: %d", n)
	}
	// Nothing changes on the VM row until the worker finishes.
	if n := count(t, env, `SELECT count(*) FROM vms v JOIN templates t ON t.id=v.template_id WHERE t.slug='ubuntu-24.04'`); n != 1 {
		t.Errorf("the template must not change before the worker finishes")
	}
}

func mustReq(t *testing.T, method, url, token string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}
