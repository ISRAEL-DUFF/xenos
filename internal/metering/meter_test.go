package metering

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/israel-duff/xenos/internal/billing"
	"github.com/israel-duff/xenos/internal/jobs"
	"github.com/israel-duff/xenos/internal/proxmox"
	"github.com/israel-duff/xenos/internal/store"
	"github.com/israel-duff/xenos/internal/store/db"
	"github.com/israel-duff/xenos/internal/testutil"
	"github.com/israel-duff/xenos/internal/vm"
)

const nano = 6000 // micro-USDT per hour, from the seeded plan

type mailbox struct {
	mu   sync.Mutex
	sent []string // "subject|body"
}

func (m *mailbox) Send(_ context.Context, _, subject, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, subject+"|"+body)
	return nil
}

func (m *mailbox) count(subject string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, s := range m.sent {
		if strings.HasPrefix(s, subject) {
			n++
		}
	}
	return n
}

type env struct {
	t     *testing.T
	st    *store.Store
	is    *billing.Fake
	pve   *proxmox.Fake
	prov  *vm.Provisioner
	meter *Meter
	mail  *mailbox
	now   time.Time
	ip    int
}

func newEnv(t *testing.T, start time.Time) *env {
	e := &env{t: t, st: testutil.DB(t), is: billing.NewFake(150_000), pve: proxmox.NewFake(), mail: &mailbox{}, now: start}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	clock := func() time.Time { return e.now }
	e.prov = &vm.Provisioner{Store: e.st, PVE: e.pve, Log: log, Now: clock,
		Cfg: vm.Config{Storage: "vmdata", Disk: "scsi0", AgentTimeout: time.Second, PollInterval: time.Millisecond, IPv4PrefixLen: 32}}
	e.meter = &Meter{Store: e.st, ISpend: e.is, Cache: billing.NewBalanceCache(e.is, time.Minute), Jobs: jobs.New(e.st.Pool),
		Mailer: e.mail, Log: log, Now: clock, Grace: 72 * time.Hour, MinRunwayHours: 24, LockKey: rand.Int63()}
	return e
}

// user creates a user with an iSpend customer and the given USDT balance.
func (e *env) user(email string, balance int64) (int64, string) {
	e.t.Helper()
	c, err := e.is.CreateCustomer(context.Background(), "signup:"+email, "user:"+email, email, "")
	if err != nil {
		e.t.Fatal(err)
	}
	e.is.Credit(c.ID, balance)
	var id int64
	must(e.t, e.st.Pool.QueryRow(context.Background(),
		`INSERT INTO users (email, password_hash, ispend_customer_id) VALUES ($1, 'x', $2) RETURNING id`, email, c.ID).Scan(&id))
	return id, c.ID
}

// runningVM provisions a nano VM for the user through the real worker code, so
// billing_from is set exactly as in production.
func (e *env) runningVM(userID int64) int64 {
	e.t.Helper()
	ctx := context.Background()
	e.ip++
	ip := fmt.Sprintf("203.0.113.%d", e.ip)
	var ipID int64
	must(e.t, e.st.Pool.QueryRow(ctx, `INSERT INTO ip_addresses (address, gateway, region) VALUES ($1::inet, '203.0.113.1', 'r') RETURNING id`, ip).Scan(&ipID))
	plan, _ := e.st.Q.GetActivePlanBySlug(ctx, "nano")
	tpl, _ := e.st.Q.GetActiveTemplateBySlug(ctx, "debian-12")
	id, err := e.st.Q.CreateVM(ctx, db.CreateVMParams{UserID: userID, Region: "r", PlanID: plan.ID, TemplateID: tpl.ID, Hostname: "h", AuthorizedKeys: "k"})
	must(e.t, err)
	must(e.t, e.st.Q.AssignIP(ctx, db.AssignIPParams{ID: ipID, VmID: pgtype.Int8{Int64: id, Valid: true}}))
	must(e.t, e.st.Q.SetVMIPv4(ctx, db.SetVMIPv4Params{ID: id, Ipv4ID: pgtype.Int8{Int64: ipID, Valid: true}}))
	e.job(vm.JobProvision, id)
	if s := e.vmState(id); s != "running" {
		e.t.Fatalf("vm state = %s", s)
	}
	return id
}

func (e *env) job(kind string, id int64) {
	e.t.Helper()
	payload, _ := json.Marshal(vm.Payload{VMID: id})
	must(e.t, e.prov.Handlers()[kind](context.Background(), &jobs.Job{Kind: kind, Payload: payload, Attempts: 1}))
}

// worker runs everything queued, as the worker process would.
func (e *env) worker() int { return testutil.DrainJobs(e.t, e.st, e.prov.Handlers()) }

func (e *env) tickAt(t time.Time) {
	e.t.Helper()
	e.now = t
	must(e.t, e.meter.Tick(context.Background()))
}

func (e *env) vmState(id int64) string {
	var s string
	must(e.t, e.st.Pool.QueryRow(context.Background(), `SELECT state FROM vms WHERE id=$1`, id).Scan(&s))
	return s
}

func (e *env) n(sql string, args ...any) int {
	var n int
	must(e.t, e.st.Pool.QueryRow(context.Background(), sql, args...).Scan(&n))
	return n
}

func (e *env) balance(cust string) int64 {
	b, err := e.is.Balances(context.Background(), cust)
	must(e.t, err)
	return b.USDTMicro
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func at(h, m int) time.Time { return time.Date(2026, 10, 5, h, m, 0, 0, time.UTC) }

func TestThreeHoursThreeCharges(t *testing.T) {
	e := newEnv(t, at(10, 20))
	uid, cust := e.user("a@x.co", 1_000_000)
	vmID := e.runningVM(uid)

	// Many ticks per hour must still produce exactly one charge per hour.
	for _, tm := range []time.Time{at(10, 20), at(10, 21), at(10, 59), at(11, 0), at(11, 30), at(12, 0), at(12, 59)} {
		e.tickAt(tm)
	}
	if n := e.n(`SELECT count(*) FROM usage_charges WHERE vm_id=$1 AND status='paid'`, vmID); n != 3 {
		t.Fatalf("paid usage rows = %d, want 3", n)
	}
	if e.is.Merchant != 3*nano || e.balance(cust) != 1_000_000-3*nano {
		t.Fatalf("merchant=%d balance=%d", e.is.Merchant, e.balance(cust))
	}
	e.tickAt(at(13, 0))
	if e.is.Merchant != 4*nano {
		t.Fatalf("hour 13 not charged: merchant=%d", e.is.Merchant)
	}
}

func TestStoppedVMsStillBill(t *testing.T) {
	e := newEnv(t, at(10, 5))
	uid, _ := e.user("a@x.co", 1_000_000)
	vmID := e.runningVM(uid)
	payload, _ := json.Marshal(vm.Payload{VMID: vmID, Action: vm.ActionStop})
	must(t, e.prov.Handlers()[vm.JobPower](context.Background(), &jobs.Job{Payload: payload}))
	if e.vmState(vmID) != "stopped" {
		t.Fatal("expected stopped")
	}
	e.tickAt(at(10, 6))
	e.tickAt(at(11, 0))
	if e.is.Merchant != 2*nano {
		t.Fatalf("stopped VM must bill: merchant=%d", e.is.Merchant)
	}
}

func TestOutageProducesCatchUpNotMissedHours(t *testing.T) {
	e := newEnv(t, at(10, 20))
	uid, cust := e.user("a@x.co", 1_000_000)
	vmID := e.runningVM(uid)

	e.is.Down = true
	e.tickAt(at(10, 20))
	e.tickAt(at(11, 5))
	e.tickAt(at(12, 10))
	if e.is.Merchant != 0 {
		t.Fatal("nothing can be charged during the outage")
	}
	if n := e.n(`SELECT count(*) FROM usage_charges WHERE vm_id=$1 AND status='pending'`, vmID); n != 3 {
		t.Fatalf("pending rows during outage = %d, want one per hour (3)", n)
	}

	e.is.Down = false
	e.tickAt(at(12, 11))
	if n := e.n(`SELECT count(*) FROM usage_charges WHERE vm_id=$1`, vmID); n != 3 {
		t.Fatalf("usage rows = %d, want exactly 3", n)
	}
	if n := e.n(`SELECT count(*) FROM usage_charges WHERE vm_id=$1 AND status='paid'`, vmID); n != 3 {
		t.Fatalf("paid rows = %d, want 3", n)
	}
	if e.is.Merchant != 3*nano || e.balance(cust) != 1_000_000-3*nano {
		t.Fatalf("catch-up wrong: merchant=%d balance=%d", e.is.Merchant, e.balance(cust))
	}
	e.tickAt(at(12, 12)) // and nothing more
	if e.is.Merchant != 3*nano {
		t.Fatal("catch-up must not double charge")
	}
}

func TestChargeInterruptedAfterISpendSucceeded(t *testing.T) {
	// iSpend took the money but we died before recording it: the row is still
	// pending, and retrying with the same key must not move money twice.
	e := newEnv(t, at(10, 20))
	uid, cust := e.user("a@x.co", 1_000_000)
	vmID := e.runningVM(uid)
	_, err := e.is.Charge(context.Background(), billing.ChargeKey(vmID, at(10, 0)), cust, nano, "vm")
	must(t, err)
	e.tickAt(at(10, 21))
	if e.is.Merchant != nano {
		t.Fatalf("merchant=%d, want one charge", e.is.Merchant)
	}
	if n := e.n(`SELECT count(*) FROM usage_charges WHERE status='paid'`); n != 1 {
		t.Fatalf("paid rows = %d", n)
	}
}

func TestMonthlyCap(t *testing.T) {
	// October 2026 has 744 hours; the cap is 730 hours of the hourly price.
	e := newEnv(t, time.Date(2026, 10, 1, 0, 5, 0, 0, time.UTC))
	uid, _ := e.user("a@x.co", 100_000_000)
	vmID := e.runningVM(uid)
	e.tickAt(time.Date(2026, 10, 31, 23, 30, 0, 0, time.UTC))

	if n := e.n(`SELECT count(*) FROM usage_charges WHERE vm_id=$1`, vmID); n != 744 {
		t.Fatalf("rows = %d, want one per hour (744)", n)
	}
	if e.is.Merchant != 730*nano {
		t.Fatalf("charged %d, want capped at %d", e.is.Merchant, 730*nano)
	}
	// The cap resets in November.
	e.tickAt(time.Date(2026, 11, 1, 0, 1, 0, 0, time.UTC))
	if e.is.Merchant != 731*nano {
		t.Fatalf("november hour not charged: %d", e.is.Merchant)
	}
}

func TestOutOfFundsSuspendsThenRestoresOnTopUp(t *testing.T) {
	e := newEnv(t, at(10, 20))
	uid, cust := e.user("a@x.co", nano) // exactly one hour of credit
	vmID := e.runningVM(uid)

	e.tickAt(at(10, 20)) // hour 10 paid, wallet now empty
	e.tickAt(at(11, 0))  // hour 11 cannot be paid
	if n := e.n(`SELECT count(*) FROM usage_charges WHERE status='unpaid'`); n != 1 {
		t.Fatalf("unpaid rows = %d", n)
	}
	if n := e.n(`SELECT count(*) FROM users WHERE grace_started_at IS NOT NULL`); n != 1 {
		t.Fatal("grace must start on the first failed charge")
	}
	if e.mail.count("Your Xenos wallet is empty") != 1 {
		t.Fatalf("expected one out-of-funds email, mailbox=%v", e.mail.sent)
	}

	e.worker() // the queued vm.suspend job
	if got := e.vmState(vmID); got != "suspended" {
		t.Fatalf("state = %s, want suspended", got)
	}
	for _, g := range e.pve.VMs {
		if g.Running {
			t.Fatal("suspended guest must be stopped")
		}
	}
	if len(e.pve.VMs) != 1 {
		t.Fatal("suspension keeps the disk")
	}

	// A suspended VM costs nothing, and the grace email is not repeated.
	e.tickAt(at(12, 0))
	e.tickAt(at(13, 0))
	if n := e.n(`SELECT count(*) FROM usage_charges`); n != 2 {
		t.Fatalf("usage rows = %d, want 2 (no billing while suspended)", n)
	}
	if e.mail.count("Your Xenos wallet is empty") != 1 {
		t.Fatal("grace email repeated")
	}

	// Top-up within grace: the debt is collected and the VM returns to stopped.
	e.is.Credit(cust, nano+24*nano)
	e.tickAt(at(14, 30))
	if n := e.n(`SELECT count(*) FROM usage_charges WHERE status='paid'`); n != 2 {
		t.Fatalf("unpaid hour not collected: paid=%d", n)
	}
	e.worker() // vm.resume
	if got := e.vmState(vmID); got != "stopped" {
		t.Fatalf("state = %s, want stopped", got)
	}
	if n := e.n(`SELECT count(*) FROM users WHERE grace_started_at IS NOT NULL`); n != 0 {
		t.Fatal("grace must clear once funded")
	}

	// Billing resumes from the hour of the restore.
	e.tickAt(at(14, 31))
	if n := e.n(`SELECT count(*) FROM usage_charges WHERE hour = $1`, at(14, 0)); n != 1 {
		t.Fatal("resumed VM must bill the current hour")
	}
	if e.mail.count("Your Xenos VMs are back") != 1 {
		t.Fatalf("restore email missing: %v", e.mail.sent)
	}
}

func TestGraceExpiryDeletesVMs(t *testing.T) {
	e := newEnv(t, at(10, 20))
	uid, _ := e.user("a@x.co", nano)
	vmID := e.runningVM(uid)
	e.tickAt(at(10, 20))
	e.tickAt(at(11, 0)) // fails: grace starts at 11:00
	e.worker()          // suspend

	e.tickAt(at(11, 0).Add(71 * time.Hour))
	if e.n(`SELECT count(*) FROM jobs WHERE kind='vm.delete'`) != 0 {
		t.Fatal("must not delete before the grace period ends")
	}
	e.tickAt(at(11, 0).Add(72 * time.Hour))
	e.worker()
	if got := e.vmState(vmID); got != "deleted" {
		t.Fatalf("state = %s, want deleted", got)
	}
	if len(e.pve.VMs) != 0 {
		t.Fatal("guest must be destroyed")
	}
	if e.n(`SELECT count(*) FROM ip_addresses WHERE vm_id IS NOT NULL`) != 0 {
		t.Fatal("IP must return to the pool")
	}
	e.tickAt(at(11, 0).Add(73 * time.Hour))
	if n := e.n(`SELECT count(*) FROM users WHERE grace_started_at IS NOT NULL`); n != 0 {
		t.Fatal("grace clears once the user has no VMs")
	}
}

func TestDeleteStopsBillingAfterCurrentHour(t *testing.T) {
	e := newEnv(t, at(10, 20))
	uid, _ := e.user("a@x.co", 1_000_000)
	vmID := e.runningVM(uid)
	e.tickAt(at(10, 21))
	e.now = at(10, 40)
	e.job(vm.JobDelete, vmID)
	e.tickAt(at(11, 0))
	e.tickAt(at(12, 0))
	if e.is.Merchant != nano {
		t.Fatalf("charged %d, want just the hour the VM was deleted in", e.is.Merchant)
	}
}

func TestDeleteDuringOutageStillCharges(t *testing.T) {
	e := newEnv(t, at(10, 20))
	uid, _ := e.user("a@x.co", 1_000_000)
	vmID := e.runningVM(uid)
	e.is.Down = true
	e.tickAt(at(11, 5)) // hours 10 and 11 recorded as pending
	e.now = at(11, 30)
	e.job(vm.JobDelete, vmID)
	e.is.Down = false
	e.tickAt(at(11, 40))
	if e.is.Merchant != 2*nano {
		t.Fatalf("hours before deletion must still be collected: merchant=%d", e.is.Merchant)
	}
}

func TestLowBalanceWarningOncePerDay(t *testing.T) {
	e := newEnv(t, at(10, 20))
	uid, cust := e.user("a@x.co", 22*nano) // 22 hours of runway: under the 24h minimum
	e.runningVM(uid)

	// Tick hourly for 26 hours, topping up one hour each time so the runway stays at ~22h.
	for i := 0; i < 26; i++ {
		e.is.Credit(cust, nano)
		e.meter.Cache.Invalidate(cust)
		e.tickAt(at(10, 20).Add(time.Duration(i) * time.Hour))
		if i == 0 && e.mail.count("Low Xenos wallet balance") != 1 {
			t.Fatalf("want a warning on the first low tick, got %v", e.mail.sent)
		}
		if i == 23 && e.mail.count("Low Xenos wallet balance") != 1 {
			t.Fatalf("warning repeated within a day: %v", e.mail.sent)
		}
	}
	if got := e.mail.count("Low Xenos wallet balance"); got != 2 {
		t.Fatalf("want a reminder after 24h (2 total), got %d: %v", got, e.mail.sent)
	}
	if !strings.Contains(e.mail.sent[0], "₦") {
		t.Fatalf("warning should state the naira needed: %s", e.mail.sent[0])
	}

	e.is.Credit(cust, 1_000_000)
	e.meter.Cache.Invalidate(cust)
	e.tickAt(at(10, 20).Add(27 * time.Hour))
	if n := e.n(`SELECT count(*) FROM users WHERE low_balance_notified_at IS NOT NULL`); n != 0 {
		t.Fatal("flag must reset once the balance recovers")
	}
}

func TestOnlyOneMeterRuns(t *testing.T) {
	e := newEnv(t, at(10, 20))
	uid, _ := e.user("a@x.co", 1_000_000)
	e.runningVM(uid)

	ctx := context.Background()
	conn, err := e.st.Pool.Acquire(ctx)
	must(t, err)
	defer conn.Release()
	_, err = conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, e.meter.LockKey) // another instance is mid-tick
	must(t, err)

	e.tickAt(at(10, 21))
	if n := e.n(`SELECT count(*) FROM usage_charges`); n != 0 {
		t.Fatal("a second meter must not run while the lock is held")
	}
	_, err = conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, e.meter.LockKey)
	must(t, err)
	e.tickAt(at(10, 22))
	if n := e.n(`SELECT count(*) FROM usage_charges`); n != 1 {
		t.Fatalf("meter should run once the lock is free, rows=%d", n)
	}
}

func TestFailedProvisioningIsNeverCharged(t *testing.T) {
	e := newEnv(t, at(10, 20))
	uid, _ := e.user("a@x.co", 1_000_000)
	ctx := context.Background()
	var ipID int64
	must(t, e.st.Pool.QueryRow(ctx, `INSERT INTO ip_addresses (address, gateway, region) VALUES ('203.0.113.9', '203.0.113.1', 'r') RETURNING id`).Scan(&ipID))
	plan, _ := e.st.Q.GetActivePlanBySlug(ctx, "nano")
	tpl, _ := e.st.Q.GetActiveTemplateBySlug(ctx, "debian-12")
	id, _ := e.st.Q.CreateVM(ctx, db.CreateVMParams{UserID: uid, Region: "r", PlanID: plan.ID, TemplateID: tpl.ID, Hostname: "h", AuthorizedKeys: "k"})
	_ = e.st.Q.AssignIP(ctx, db.AssignIPParams{ID: ipID, VmID: pgtype.Int8{Int64: id, Valid: true}})
	_ = e.st.Q.SetVMIPv4(ctx, db.SetVMIPv4Params{ID: id, Ipv4ID: pgtype.Int8{Int64: ipID, Valid: true}})

	e.pve.Fail["configure"] = fmt.Errorf("boom")
	payload, _ := json.Marshal(vm.Payload{VMID: id})
	_ = e.prov.Handlers()[vm.JobProvision](ctx, &jobs.Job{Payload: payload, Attempts: jobs.MaxAttempts})
	e.tickAt(at(11, 30))
	if e.is.Merchant != 0 || e.n(`SELECT count(*) FROM usage_charges`) != 0 {
		t.Fatal("a VM that never ran must not be billed")
	}
}

func TestChargesAreSpreadAcrossTheHour(t *testing.T) {
	// iswallet allows 100 calls a minute per key and sends no Retry-After, so the hourly charges
	// must not all land at :00. Each VM's charge falls due at a stable offset into the hour.
	e := newEnv(t, at(10, 20))
	e.meter.SpreadMinutes = 30
	uid, _ := e.user("a@x.co", 1_000_000)
	vm1 := e.runningVM(uid) // offset (1*17)%30 = 17 minutes
	vm2 := e.runningVM(uid) // offset (2*17)%30 = 4 minutes
	if e.meter.chargeOffset(vm1) != 17*time.Minute || e.meter.chargeOffset(vm2) != 4*time.Minute {
		t.Fatalf("offsets %v %v", e.meter.chargeOffset(vm1), e.meter.chargeOffset(vm2))
	}

	e.tickAt(at(10, 20)) // hour 10 is long due for both
	if e.is.Merchant != 2*nano {
		t.Fatalf("merchant=%d", e.is.Merchant)
	}
	e.tickAt(at(11, 0))
	e.tickAt(at(11, 3))
	if e.is.Merchant != 2*nano {
		t.Fatalf("nothing is due at the top of the hour: merchant=%d", e.is.Merchant)
	}
	e.tickAt(at(11, 4))
	if e.is.Merchant != 3*nano {
		t.Fatalf("VM 2 falls due at 11:04: merchant=%d", e.is.Merchant)
	}
	e.tickAt(at(11, 16))
	if e.is.Merchant != 3*nano {
		t.Fatal("VM 1 is not due before 11:17")
	}
	e.tickAt(at(11, 17))
	if e.is.Merchant != 4*nano {
		t.Fatalf("VM 1 falls due at 11:17: merchant=%d", e.is.Merchant)
	}
	// A late tick catches up every hour that is due, in order.
	e.tickAt(at(14, 30))
	if e.is.Merchant != 2*nano*5 { // hours 10..14 for both VMs
		t.Fatalf("catch-up: merchant=%d", e.is.Merchant)
	}
}

func TestChargeNarrationIdentifiesTheVMAndHour(t *testing.T) {
	e := newEnv(t, at(10, 20))
	uid, _ := e.user("a@x.co", 1_000_000)
	vmID := e.runningVM(uid)
	e.tickAt(at(10, 21))
	key := billing.ChargeKey(vmID, at(10, 0))
	want := fmt.Sprintf("vm:%d hour:2026100510", vmID)
	if got := e.is.Narrations[key]; got != want {
		t.Fatalf("narration = %q, want %q (it is the only metadata iswallet keeps for reconciliation)", got, want)
	}
}

// reuseFake refuses every charge as IDEMPOTENCY_KEY_REUSED, the one error that can never succeed on retry.
type reuseFake struct{ *billing.Fake }

func (reuseFake) Charge(context.Context, string, string, int64, string) (billing.Movement, error) {
	return billing.Movement{}, billing.ErrIdempotencyKeyReused
}

type alerts struct {
	mu   sync.Mutex
	sent []string
}

func (a *alerts) Notify(_ context.Context, key string, _ time.Duration, text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sent = append(a.sent, key+"|"+text)
}

func TestKeyReusedChargeIsAlertedNotRetriedAsTransient(t *testing.T) {
	e := newEnv(t, at(10, 20))
	al := &alerts{}
	e.meter.Alerts = al
	uid, _ := e.user("a@x.co", 1_000_000)
	vm1 := e.runningVM(uid)
	e.meter.ISpend = reuseFake{e.is}

	e.tickAt(at(10, 21))
	if len(al.sent) != 1 || !strings.Contains(al.sent[0], "IDEMPOTENCY_KEY_REUSED") {
		t.Fatalf("operators must hear about a key-generation bug: %v", al.sent)
	}
	// It is not an outage: it must not mark iswallet down or start the out-of-funds path.
	if n := e.n(`SELECT count(*) FROM users WHERE grace_started_at IS NOT NULL`); n != 0 {
		t.Fatal("a key-reuse error is not a lack of funds")
	}
	if n := e.n(`SELECT count(*) FROM usage_charges WHERE vm_id=$1 AND status='pending'`, vm1); n != 1 {
		t.Fatalf("the charge stays open for a human, pending rows = %d", n)
	}
}

// mismatchFake answers every charge with CURRENCY_MISMATCH, as iswallet does for a customer who
// has never converted and so holds no USDT.
type mismatchFake struct{ *billing.Fake }

func (mismatchFake) Charge(context.Context, string, string, int64, string) (billing.Movement, error) {
	return billing.Movement{}, billing.ErrCurrencyMismatch
}

func TestCurrencyMismatchForAnEmptyWalletIsOutOfFundsNotAnOutage(t *testing.T) {
	e := newEnv(t, at(10, 20))
	uid, _ := e.user("a@x.co", 0) // never converted: holds no USDT
	e.runningVM(uid)
	e.meter.ISpend = mismatchFake{e.is}
	e.tickAt(at(10, 21))

	if n := e.n(`SELECT count(*) FROM usage_charges WHERE status='unpaid'`); n != 1 {
		t.Fatalf("the charge must be unpaid, not left pending: %d", n)
	}
	if n := e.n(`SELECT count(*) FROM users WHERE grace_started_at IS NOT NULL`); n != 1 {
		t.Fatal("a customer with no USDT cannot pay: the out-of-funds path must start")
	}
}

func TestCurrencyMismatchDoesNotBlockOtherCustomers(t *testing.T) {
	e := newEnv(t, at(10, 20))
	e.meter.Alerts = &alerts{}
	empty, _ := e.user("empty@x.co", 0)
	funded, _ := e.user("funded@x.co", 1_000_000)
	e.runningVM(empty)
	vmOK := e.runningVM(funded)
	// Only the customer with no USDT gets the mismatch; the other is charged normally.
	e.meter.ISpend = selectiveMismatch{Fake: e.is, bad: map[string]bool{custOf(t, e, empty): true}}
	e.tickAt(at(10, 21))
	if n := e.n(`SELECT count(*) FROM usage_charges WHERE vm_id=$1 AND status='paid'`, vmOK); n != 1 {
		t.Fatal("one customer's CURRENCY_MISMATCH must not stop the rest of the tick")
	}
}

func custOf(t *testing.T, e *env, uid int64) string {
	var c string
	must(t, e.st.Pool.QueryRow(context.Background(), `SELECT ispend_customer_id FROM users WHERE id=$1`, uid).Scan(&c))
	return c
}

type selectiveMismatch struct {
	*billing.Fake
	bad map[string]bool
}

func (s selectiveMismatch) Charge(ctx context.Context, key, cust string, amt int64, n string) (billing.Movement, error) {
	if s.bad[cust] {
		return billing.Movement{}, billing.ErrCurrencyMismatch
	}
	return s.Fake.Charge(ctx, key, cust, amt, n)
}

func TestCurrencyMismatchDespiteFundsIsAlertedNotSuspended(t *testing.T) {
	e := newEnv(t, at(10, 20))
	al := &alerts{}
	e.meter.Alerts = al
	uid, _ := e.user("a@x.co", 1_000_000) // holds plenty of USDT
	e.runningVM(uid)
	e.meter.ISpend = mismatchFake{e.is}
	e.tickAt(at(10, 21))
	if len(al.sent) != 1 || !strings.Contains(al.sent[0], "CURRENCY_MISMATCH") {
		t.Fatalf("operators must be told: %v", al.sent)
	}
	if n := e.n(`SELECT count(*) FROM users WHERE grace_started_at IS NOT NULL`); n != 0 {
		t.Fatal("a funded customer must never be suspended for iswallet's mismatch")
	}
}
