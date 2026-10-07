package vm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/israel-duff/xenos/internal/jobs"
	"github.com/israel-duff/xenos/internal/proxmox"
	"github.com/israel-duff/xenos/internal/store"
	"github.com/israel-duff/xenos/internal/store/db"
	"github.com/israel-duff/xenos/internal/testutil"
)

type env struct {
	t    *testing.T
	st   *store.Store
	pve  *proxmox.Fake
	prov *Provisioner
}

func newEnv(t *testing.T) *env {
	st := testutil.DB(t)
	e := &env{t: t, st: st, pve: proxmox.NewFake()}
	e.prov = e.newProvisioner()
	return e
}

func (e *env) newProvisioner() *Provisioner {
	return &Provisioner{Store: e.st, PVE: e.pve, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Cfg: Config{Storage: "vmdata", Disk: "scsi0", AgentTimeout: 200 * time.Millisecond, PollInterval: 10 * time.Millisecond,
			IPv4PrefixLen: 32, Nameservers: "1.1.1.1"}}
}

// seedVM inserts a user, one free IP and a pending VM holding it, as the API would.
func (e *env) seedVM(ip string) int64 {
	e.t.Helper()
	ctx := context.Background()
	var uid int64
	if err := e.st.Pool.QueryRow(ctx, `INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id`,
		ip+"@x.co").Scan(&uid); err != nil {
		e.t.Fatal(err)
	}
	var ipID int64
	if err := e.st.Pool.QueryRow(ctx, `INSERT INTO ip_addresses (address, gateway, region) VALUES ($1::inet, '203.0.113.1', 'test-1') RETURNING id`, ip).Scan(&ipID); err != nil {
		e.t.Fatal(err)
	}
	q := e.st.Q
	plan, _ := q.GetActivePlanBySlug(ctx, "nano")
	tpl, _ := q.GetActiveTemplateBySlug(ctx, "ubuntu-24.04")
	id, err := q.CreateVM(ctx, db.CreateVMParams{UserID: uid, Region: "test-1", PlanID: plan.ID, TemplateID: tpl.ID,
		Hostname: "web", AuthorizedKeys: "ssh-ed25519 AAAA"})
	if err != nil {
		e.t.Fatal(err)
	}
	must(e.t, q.AssignIP(ctx, db.AssignIPParams{ID: ipID, VmID: pgtype.Int8{Int64: id, Valid: true}}))
	must(e.t, q.SetVMIPv4(ctx, db.SetVMIPv4Params{ID: id, Ipv4ID: pgtype.Int8{Int64: ipID, Valid: true}}))
	return id
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (e *env) call(p *Provisioner, kind string, id int64, action string, attempt int) error {
	payload, _ := json.Marshal(Payload{VMID: id, Action: action})
	return p.Handlers()[kind](context.Background(), &jobs.Job{Kind: kind, Payload: payload, Attempts: attempt})
}

func (e *env) state(id int64) string {
	var s string
	must(e.t, e.st.Pool.QueryRow(context.Background(), `SELECT state FROM vms WHERE id=$1`, id).Scan(&s))
	return s
}

func (e *env) ipHolder(ip string) *int64 {
	var v *int64
	must(e.t, e.st.Pool.QueryRow(context.Background(), `SELECT vm_id FROM ip_addresses WHERE address=$1::inet`, ip).Scan(&v))
	return v
}

func TestProvisionHappyPath(t *testing.T) {
	e := newEnv(t)
	id := e.seedVM("203.0.113.10")
	must(t, e.call(e.prov, JobProvision, id, "", 1))

	if got := e.state(id); got != "running" {
		t.Fatalf("state = %s", got)
	}
	if len(e.pve.VMs) != 1 {
		t.Fatalf("want 1 guest, have %v", e.pve.IDs())
	}
	for _, g := range e.pve.VMs {
		c := g.Config
		if !g.Running || g.DiskGB != 20 || c.Cores != 1 || c.MemoryMB != 1024 || c.CIUser != "root" ||
			c.IPConfig0 != "ip=203.0.113.10/32,gw=203.0.113.1" || !strings.HasPrefix(c.SSHKeys, "ssh-ed25519") || g.Template != 9000 {
			t.Fatalf("unexpected guest %+v", g)
		}
	}
	// The guest is locked to its own address before it ever ran.
	for _, g := range e.pve.VMs {
		if !g.Isolated || len(g.Allowed) != 1 || g.Allowed[0] != "203.0.113.10" {
			t.Fatalf("guest must be isolated to its address, got isolated=%v allowed=%v", g.Isolated, g.Allowed)
		}
	}
	iso, start := -1, -1
	for i, c := range e.pve.Calls {
		if strings.HasPrefix(c, "isolate:") {
			iso = i
		}
		if strings.HasPrefix(c, "start:") && start < 0 {
			start = i
		}
	}
	if iso < 0 || iso > start {
		t.Fatalf("isolate must run before start: %v", e.pve.Calls)
	}
	// Re-running a finished job changes nothing.
	must(t, e.call(e.prov, JobProvision, id, "", 1))
	if len(e.pve.VMs) != 1 {
		t.Fatal("rerun must not create another guest")
	}
}

func TestProvisionRetriesThenCleansUp(t *testing.T) {
	e := newEnv(t)
	id := e.seedVM("203.0.113.11")
	e.pve.Fail["configure"] = errors.New("boom")

	for attempt := 1; attempt < jobs.MaxAttempts; attempt++ {
		if err := e.call(e.prov, JobProvision, id, "", attempt); err == nil {
			t.Fatal("expected failure")
		}
		if got := e.state(id); got != "provisioning" {
			t.Fatalf("attempt %d: state = %s, want provisioning", attempt, got)
		}
		if e.ipHolder("203.0.113.11") == nil {
			t.Fatal("IP must stay reserved while retries remain")
		}
	}
	if err := e.call(e.prov, JobProvision, id, "", jobs.MaxAttempts); err == nil {
		t.Fatal("expected final failure")
	}
	if got := e.state(id); got != "error" {
		t.Fatalf("state = %s, want error", got)
	}
	if len(e.pve.VMs) != 0 {
		t.Fatalf("half-built guest left behind: %v", e.pve.IDs())
	}
	if e.ipHolder("203.0.113.11") != nil {
		t.Fatal("IP must be released after final failure")
	}
}

func TestAbortKeepsIPWhenCleanupFails(t *testing.T) {
	e := newEnv(t)
	id := e.seedVM("203.0.113.12")
	e.pve.Fail["configure"] = errors.New("boom")
	e.pve.Fail["destroy"] = errors.New("cannot destroy")
	if err := e.call(e.prov, JobProvision, id, "", jobs.MaxAttempts); err == nil {
		t.Fatal("expected failure")
	}
	if e.state(id) != "error" {
		t.Fatalf("state = %s", e.state(id))
	}
	if e.ipHolder("203.0.113.12") == nil {
		t.Fatal("IP must stay reserved when the guest could not be removed")
	}
}

func TestCrashMidProvisionRecovers(t *testing.T) {
	e := newEnv(t)
	id := e.seedVM("203.0.113.13")

	// First worker dies right after the clone, before configuring.
	e.pve.BeforeOp = func(op string, _ int) {
		if op == "configure" {
			panic("worker killed")
		}
	}
	func() {
		defer func() { _ = recover() }()
		_ = e.call(e.prov, JobProvision, id, "", 1)
	}()
	e.pve.BeforeOp = nil
	if got := e.state(id); got != "provisioning" || len(e.pve.VMs) != 1 {
		t.Fatalf("after crash: state=%s guests=%v", got, e.pve.IDs())
	}

	// A fresh worker picks the job up again.
	must(t, e.call(e.newProvisioner(), JobProvision, id, "", 2))
	if got := e.state(id); got != "running" {
		t.Fatalf("state = %s", got)
	}
	if len(e.pve.VMs) != 1 {
		t.Fatalf("want exactly one guest, have %v", e.pve.IDs())
	}
}

func TestAgentTimeoutFailsAttempt(t *testing.T) {
	e := newEnv(t)
	id := e.seedVM("203.0.113.14")
	e.pve.Fail["agent"] = errors.New("no agent")
	err := e.call(e.prov, JobProvision, id, "", 1)
	if err == nil || !strings.Contains(err.Error(), "guest agent") {
		t.Fatalf("err = %v", err)
	}
}

func TestDelete(t *testing.T) {
	e := newEnv(t)
	id := e.seedVM("203.0.113.15")
	must(t, e.call(e.prov, JobProvision, id, "", 1))
	must(t, e.call(e.prov, JobDelete, id, "", 1))

	if got := e.state(id); got != "deleted" {
		t.Fatalf("state = %s", got)
	}
	if len(e.pve.VMs) != 0 || e.ipHolder("203.0.113.15") != nil {
		t.Fatalf("guest or IP left behind: %v", e.pve.IDs())
	}
	must(t, e.call(e.prov, JobDelete, id, "", 1)) // idempotent
}

func TestDeleteBeforeProvisioningStopsProvisioning(t *testing.T) {
	e := newEnv(t)
	id := e.seedVM("203.0.113.16")
	must(t, e.call(e.prov, JobDelete, id, "", 1))
	must(t, e.call(e.prov, JobProvision, id, "", 1)) // the queued provision job runs afterwards
	if e.state(id) != "deleted" || len(e.pve.VMs) != 0 || e.ipHolder("203.0.113.16") != nil {
		t.Fatalf("state=%s guests=%v", e.state(id), e.pve.IDs())
	}
}

func TestPowerActions(t *testing.T) {
	e := newEnv(t)
	id := e.seedVM("203.0.113.17")
	must(t, e.call(e.prov, JobProvision, id, "", 1))
	vmid := e.pve.IDs()[0]

	must(t, e.call(e.prov, JobPower, id, ActionStop, 1))
	if e.state(id) != "stopped" || e.pve.VMs[vmid].Running {
		t.Fatalf("stop: state=%s running=%v", e.state(id), e.pve.VMs[vmid].Running)
	}
	must(t, e.call(e.prov, JobPower, id, ActionReboot, 1)) // not running: ignored
	if e.pve.VMs[vmid].Running {
		t.Fatal("reboot must not start a stopped VM")
	}
	must(t, e.call(e.prov, JobPower, id, ActionStart, 1))
	if e.state(id) != "running" || !e.pve.VMs[vmid].Running {
		t.Fatalf("start: state=%s", e.state(id))
	}
	must(t, e.call(e.prov, JobPower, id, ActionReboot, 1))
	if e.state(id) != "running" {
		t.Fatalf("reboot: state=%s", e.state(id))
	}

	// A retried stop whose first attempt already stopped the guest only fixes the record.
	e.pve.VMs[vmid].Running = false
	before := len(e.pve.Calls)
	must(t, e.call(e.prov, JobPower, id, ActionStop, 2))
	if e.state(id) != "stopped" {
		t.Fatalf("state = %s", e.state(id))
	}
	for _, c := range e.pve.Calls[before:] {
		if strings.HasPrefix(c, "shutdown") {
			t.Fatal("must not shut down an already stopped guest")
		}
	}
}

func TestIPv6For(t *testing.T) {
	got := IPv6For(mustPrefix(t, "2001:db8:1:2::/64"), 5).String()
	if got != "2001:db8:1:2::105" {
		t.Fatalf("got %s", got)
	}
}

func mustPrefix(t *testing.T, s string) netip.Prefix {
	p, err := netip.ParsePrefix(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
