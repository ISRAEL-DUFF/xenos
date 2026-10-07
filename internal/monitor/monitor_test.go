package monitor

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/israel-duff/xenos/internal/alert"
	"github.com/israel-duff/xenos/internal/hosts"
	"github.com/israel-duff/xenos/internal/proxmox"
	"github.com/israel-duff/xenos/internal/store"
	"github.com/israel-duff/xenos/internal/store/db"
	"github.com/israel-duff/xenos/internal/testutil"
)

type inbox struct {
	mu   sync.Mutex
	sent []string
}

func (m *inbox) Send(_ context.Context, _, subject, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, subject+"|"+body)
	return nil
}

func (m *inbox) count(substr string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, s := range m.sent {
		if strings.Contains(s, substr) {
			n++
		}
	}
	return n
}

type env struct {
	t   *testing.T
	st  *store.Store
	pve *proxmox.Fake
	box *inbox
	mon *Monitor
	now time.Time
}

func newEnv(t *testing.T) *env {
	e := &env{t: t, st: testutil.DB(t), pve: proxmox.NewFake(), box: &inbox{}, now: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	clock := func() time.Time { return e.now }
	n := &alert.Notifier{Store: e.st, Log: log, Now: clock, Mailer: e.box, ToEmail: "ops@x.co"}
	e.mon = &Monitor{Store: e.st, PVE: e.pve, Notify: n, Log: log, Now: clock, Cfg: DefaultConfig("vmdata")}
	e.mon.Cfg.MinFreeIPs = 0 // most tests have an empty IP pool
	return e
}

func (e *env) tick() {
	e.t.Helper()
	if err := e.mon.Tick(context.Background()); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.st.Pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) user() int64 {
	var id int64
	if err := e.st.Pool.QueryRow(context.Background(), `INSERT INTO users (email, password_hash) VALUES ('u@x.co', 'x') RETURNING id`).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// runningVM inserts a running nano VM with the given Proxmox id.
func (e *env) runningVM(uid int64, vmid int) int64 {
	ctx := context.Background()
	plan, _ := e.st.Q.GetActivePlanBySlug(ctx, "nano")
	tpl, _ := e.st.Q.GetActiveTemplateBySlug(ctx, "debian-12")
	var id int64
	if err := e.st.Pool.QueryRow(ctx,
		`INSERT INTO vms (user_id, region, plan_id, template_id, proxmox_vmid, hostname, state) VALUES ($1,'r',$2,$3,$4,'miner','running') RETURNING id`,
		uid, plan.ID, tpl.ID, vmid).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	e.pve.VMs[vmid] = &proxmox.FakeVM{ID: vmid, Running: true}
	return id
}

func TestHeartbeatRecorded(t *testing.T) {
	e := newEnv(t)
	e.tick()
	at, err := e.st.Q.GetHeartbeat(context.Background(), "worker")
	if err != nil || !at.Equal(e.now) {
		t.Fatalf("heartbeat = %v %v", at, err)
	}
}

func TestFailedJobsAlertedOnce(t *testing.T) {
	e := newEnv(t)
	e.exec(`INSERT INTO jobs (kind, status, attempts, last_error) VALUES ('vm.provision', 'failed', 3, 'clone: 500 no space left')`)
	e.tick()
	e.tick()
	if e.box.count("vm.provision: clone: 500 no space left") != 1 {
		t.Fatalf("want exactly one alert naming the job, got %v", e.box.sent)
	}
	e.exec(`INSERT INTO jobs (kind, status, attempts, last_error) VALUES ('conversion.run', 'failed', 3, 'quote unavailable')`)
	e.tick()
	if e.box.count("conversion.run") != 1 {
		t.Fatalf("a new failure must alert: %v", e.box.sent)
	}
}

func TestDiskPoolAndRAMThresholds(t *testing.T) {
	e := newEnv(t)
	e.pve.Pool = proxmox.Usage{Used: 79, Total: 100}
	e.pve.MemTotal = 8 << 30
	e.tick()
	if len(e.box.sent) != 0 {
		t.Fatalf("below thresholds nothing should alert: %v", e.box.sent)
	}

	e.pve.Pool = proxmox.Usage{Used: 80 << 30, Total: 100 << 30}
	e.tick()
	if e.box.count("pool-full") != 1 || e.box.count("80% full") != 1 {
		t.Fatalf("pool alert missing: %v", e.box.sent)
	}
	e.tick()
	if e.box.count("pool-full") != 1 {
		t.Fatal("pool alert must respect its cooldown")
	}

	// 8 GiB host, nano VMs promise 1 GiB each: 8 running VMs commit 100%.
	uid := e.user()
	for i := 0; i < 8; i++ {
		e.runningVM(uid, 100+i)
	}
	e.tick()
	if e.box.count("ram-committed") != 1 {
		t.Fatalf("RAM alert missing: %v", e.box.sent)
	}
}

func TestProxmoxUnreachableAlerts(t *testing.T) {
	e := newEnv(t)
	e.pve.Fail["pool"] = fmt.Errorf("connection refused")
	e.tick()
	if e.box.count("proxmox-unreachable") != 1 {
		t.Fatalf("want an unreachable alert: %v", e.box.sent)
	}
}

func TestFreeIPsLow(t *testing.T) {
	e := newEnv(t)
	e.mon.Cfg.MinFreeIPs = 2
	e.exec(`INSERT INTO ip_addresses (address, gateway, region) VALUES ('203.0.113.10', '203.0.113.1', 'r')`)
	e.tick()
	if e.box.count("ips-low") != 1 {
		t.Fatalf("want a low-IP alert: %v", e.box.sent)
	}
}

func TestBillingTroubleAlerts(t *testing.T) {
	e := newEnv(t)
	uid := e.user()
	vmID := e.runningVM(uid, 100)
	e.exec(`INSERT INTO usage_charges (user_id, vm_id, hour, amount_uusdt, status, created_at) VALUES ($1, $2, $3, 6000, 'pending', $4)`,
		uid, vmID, e.now.Truncate(time.Hour), e.now.Add(-time.Hour))
	e.tick()
	if e.box.count("charges-stuck") != 1 {
		t.Fatalf("stale pending charges must alert: %v", e.box.sent)
	}

	e.exec(`INSERT INTO conversions (user_id, amount_ngn_kobo, status, last_error, created_at) VALUES ($1, 500000, 'pending', 'quote unavailable', $2)`,
		uid, e.now.Add(-time.Hour))
	e.tick()
	if e.box.count("conversions-stuck") != 1 {
		t.Fatalf("stuck conversions must alert: %v", e.box.sent)
	}
}

func TestWebhookSignatureFailuresAlert(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 4; i++ {
		e.exec(`INSERT INTO webhook_failures (at, ip) VALUES ($1, '1.2.3.4')`, e.now.Add(-time.Minute))
	}
	e.tick()
	if e.box.count("webhook-signature") != 0 {
		t.Fatal("4 failures is under the threshold")
	}
	e.exec(`INSERT INTO webhook_failures (at, ip) VALUES ($1, '1.2.3.4')`, e.now.Add(-time.Minute))
	e.tick()
	if e.box.count("webhook-signature") != 1 {
		t.Fatalf("5 failures should alert: %v", e.box.sent)
	}
	e.exec(`INSERT INTO webhook_failures (at, ip) VALUES ($1, '1.2.3.4')`, e.now.Add(-48*time.Hour))
	e.tick()
	var old int
	_ = e.st.Pool.QueryRow(context.Background(), `SELECT count(*) FROM webhook_failures WHERE at < $1`, e.now.Add(-24*time.Hour)).Scan(&old)
	if old != 0 {
		t.Fatal("old failures should be pruned")
	}
}

func TestCPUWatchFlagsSustainedMining(t *testing.T) {
	e := newEnv(t)
	uid := e.user()
	id := e.runningVM(uid, 100)
	flagged := func() bool {
		var n int
		_ = e.st.Pool.QueryRow(context.Background(), `SELECT count(*) FROM vms WHERE id=$1 AND flagged_at IS NOT NULL`, id).Scan(&n)
		return n == 1
	}
	step := func(d time.Duration, cpu float64) {
		e.now = e.now.Add(d)
		e.pve.CPU[100] = cpu
		e.tick()
	}

	step(0, 0.99) // starts the clock
	step(5*time.Hour, 0.97)
	if flagged() {
		t.Fatal("5h is under the 6h window")
	}
	step(30*time.Minute, 0.70) // a dip between 50% and 90% does not reset the clock
	step(31*time.Minute, 0.98)
	if !flagged() {
		t.Fatal("6h of sustained high CPU must flag the VM")
	}
	if e.box.count("flagged for review") != 1 || e.box.count("miner") != 1 {
		t.Fatalf("want one alert naming the VM: %v", e.box.sent)
	}
	step(time.Hour, 0.99)
	if e.box.count("flagged for review") != 1 {
		t.Fatal("flag must alert only once")
	}
}

func TestCPUWatchResetsWhenLoadDrops(t *testing.T) {
	e := newEnv(t)
	uid := e.user()
	id := e.runningVM(uid, 100)
	step := func(d time.Duration, cpu float64) {
		e.now = e.now.Add(d)
		e.pve.CPU[100] = cpu
		e.tick()
	}
	step(0, 0.99)
	step(4*time.Hour, 0.10) // genuinely idle: reset
	step(3*time.Hour, 0.99) // a new window starts here
	step(3*time.Hour, 0.99) // only 3h into it
	var n int
	_ = e.st.Pool.QueryRow(context.Background(), `SELECT count(*) FROM vms WHERE id=$1 AND flagged_at IS NOT NULL`, id).Scan(&n)
	if n != 0 {
		t.Fatal("a busy build job that went idle must not be flagged")
	}
}

func TestFlagQueries(t *testing.T) {
	e := newEnv(t)
	uid := e.user()
	id := e.runningVM(uid, 100)
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(e.st.Q.FlagVM(context.Background(), db.FlagVMParams{ID: id, FlaggedAt: pgtype.Timestamptz{Time: e.now, Valid: true}, FlagReason: pgtype.Text{String: "why", Valid: true}}))
	rows, err := e.st.Q.ListFlaggedVMs(context.Background())
	must(err)
	if len(rows) != 1 || rows[0].Email != "u@x.co" || rows[0].FlagReason != "why" {
		t.Fatalf("flagged = %+v", rows)
	}
	must(e.st.Q.ClearVMFlag(context.Background(), id))
	if rows, _ = e.st.Q.ListFlaggedVMs(context.Background()); len(rows) != 0 {
		t.Fatal("flag should clear")
	}
}

func TestEachHostIsWatchedAndAlertedByName(t *testing.T) {
	e := newEnv(t)
	good, bad := proxmox.NewFake(), proxmox.NewFake()
	bad.Fail["memory"] = fmt.Errorf("connection refused")
	set := hosts.NewSet(&hosts.Host{Name: "pve1", API: good, Storage: "vmdata"}, &hosts.Host{Name: "pve2", API: bad, Storage: "vmdata"})
	if err := set.Attach(context.Background(), e.st); err != nil {
		t.Fatal(err)
	}
	e.mon.Hosts = set
	e.tick()
	if e.box.count("host pve2") != 1 || e.box.count("host pve1") != 0 {
		t.Fatalf("alerts = %v", e.box.sent)
	}
	// A disabled host is left alone.
	e.exec(`UPDATE hosts SET status='disabled' WHERE name='pve2'`)
	e.now = e.now.Add(2 * time.Hour)
	before := e.box.count("host pve2")
	e.tick()
	if e.box.count("host pve2") != before {
		t.Fatal("a disabled host must not alert")
	}
}
