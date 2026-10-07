// Package metering charges running VMs hour by hour from the owner's iSpend
// USDT wallet and enforces the out-of-funds path: suspend, grace, delete.
//
// Model: an hour is charged in advance, the moment it begins. A VM is billable
// from the hour it reaches running until the hour its owner asked to delete it
// (or it was suspended). Every (VM, hour) gets exactly one usage_charges row
// and one iSpend idempotency key, so retries and catch-up after an outage can
// never charge twice or skip an hour.
package metering

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/israel-duff/xenos/internal/billing"
	"github.com/israel-duff/xenos/internal/jobs"
	"github.com/israel-duff/xenos/internal/mail"
	"github.com/israel-duff/xenos/internal/metrics"
	"github.com/israel-duff/xenos/internal/store"
	"github.com/israel-duff/xenos/internal/store/db"
	"github.com/israel-duff/xenos/internal/vm"
)

const (
	DefaultLockKey = 0x58454e4f53 // "XENOS"
	hour           = time.Hour
)

type Meter struct {
	Store  *store.Store
	ISpend billing.ISpend
	Cache  *billing.BalanceCache
	Jobs   *jobs.Queue
	Mailer mail.Mailer
	Log    *slog.Logger
	Now    func() time.Time

	// Grace is how long suspended VMs are kept before deletion (plan: 72h).
	Grace time.Duration
	// MinRunwayHours is the balance, in hours of usage, needed to create a VM,
	// to leave grace, and below which a low-balance warning is sent (plan: 24).
	MinRunwayHours int64
	// Alerts delivers operator alerts; may be nil.
	Alerts interface {
		Notify(ctx context.Context, key string, cooldown time.Duration, text string)
	}
	// Metrics, if set, records each pass; may be nil.
	Metrics *metrics.Metrics
	// LockKey is the Postgres advisory lock that keeps a single meter running.
	LockKey int64
	// SpreadMinutes spreads each VM's hourly charge over the first part of the hour instead of
	// firing every charge at :00. iswallet rate-limits to 100 calls a minute per key and sends
	// no Retry-After, so a burst at the top of the hour would be mostly refused. 0 disables it.
	SpreadMinutes int
}

// chargeOffset is how long after the top of the hour a VM's charge becomes due: a stable per-VM
// value in [0, SpreadMinutes), so charges are spread out rather than bunched.
func (m *Meter) chargeOffset(vmID int64) time.Duration {
	if m.SpreadMinutes <= 0 {
		return 0
	}
	return time.Duration((vmID*17)%int64(m.SpreadMinutes)) * time.Minute
}

func (m *Meter) now() time.Time {
	if m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}

// Run ticks every interval until ctx ends. Ticks are idempotent, so running
// more often than hourly only means failed charges are retried sooner and
// the first hour of a new VM is billed within a minute of it starting.
func (m *Meter) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := m.Tick(ctx); err != nil && ctx.Err() == nil {
			m.Log.Error("metering tick", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick does one metering pass. Only one pass runs at a time across processes.
func (m *Meter) Tick(ctx context.Context) error {
	key := m.LockKey
	if key == 0 {
		key = DefaultLockKey
	}
	conn, err := m.Store.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&got); err != nil {
		return err
	}
	if !got {
		return nil // another instance is metering
	}
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, key) }()

	started := time.Now()
	t := &tick{m: m, now: m.now(), balances: map[string]billing.Balances{}}
	var errs []error
	for _, step := range []func(context.Context) error{t.retryOpen, t.chargeNew, t.finishBilling, t.enforce, t.warnLow} {
		if err := step(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if t.down {
		m.Log.Warn("iSpend unreachable: charges left pending and will be retried")
	}
	err = errors.Join(errs...)
	m.Metrics.MeterTick(time.Since(started), err == nil, t.down)
	return err
}

type tick struct {
	m        *Meter
	now      time.Time
	down     bool // iSpend failed this tick; stop calling it
	balances map[string]billing.Balances
}

type charge struct {
	id, userID, vmID int64
	hour             time.Time
	amount           int64
	status           string
	customer         string
}

// balance reads a customer's wallet directly (not cached) once per tick.
func (t *tick) balance(ctx context.Context, customer string) (billing.Balances, bool) {
	if b, ok := t.balances[customer]; ok {
		return b, true
	}
	if t.down {
		return billing.Balances{}, false
	}
	b, err := t.m.ISpend.Balances(ctx, customer)
	if err != nil {
		t.fail("balances", err)
		return billing.Balances{}, false
	}
	t.balances[customer] = b
	return b, true
}

func (t *tick) fail(what string, err error) {
	t.down = true
	t.m.Log.Warn("ispend call failed", "op", what, "err", err)
}

// retryOpen re-attempts charges that are pending (iSpend was unreachable) or
// unpaid (wallet was empty). Unpaid ones are only retried once the wallet can cover them.
func (t *tick) retryOpen(ctx context.Context) error {
	rows, err := t.m.Store.Q.ListOpenCharges(ctx)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if !r.IspendCustomerID.Valid {
			continue
		}
		c := charge{r.ID, r.UserID, r.VmID, r.Hour, r.AmountUusdt, r.Status, r.IspendCustomerID.String}
		if c.status == "unpaid" {
			bal, ok := t.balance(ctx, c.customer)
			if !ok || bal.USDTMicro < c.amount {
				continue
			}
		}
		if err := t.attempt(ctx, c); err != nil {
			return err
		}
	}
	return nil
}

// attempt asks iSpend to move the money. The idempotency key is derived from
// the VM and hour, so repeating this for the same charge is always safe.
func (t *tick) attempt(ctx context.Context, c charge) error {
	if t.down {
		return nil
	}
	narration := fmt.Sprintf("vm:%d hour:%s", c.vmID, c.hour.UTC().Format("2006010215"))
	mv, err := t.m.ISpend.Charge(ctx, billing.ChargeKey(c.vmID, c.hour), c.customer, c.amount, narration)
	switch {
	case err == nil:
		delete(t.balances, c.customer)
		t.m.Cache.Invalidate(c.customer)
		return t.m.Store.Q.MarkChargePaid(ctx, db.MarkChargePaidParams{ID: c.id, IspendMovementID: textOf(mv.ID)})
	case errors.Is(err, billing.ErrInsufficientFunds):
		return t.unpaid(ctx, c)
	case errors.Is(err, billing.ErrCurrencyMismatch):
		// iswallet answers CURRENCY_MISMATCH (not INSUFFICIENT_FUNDS) when a customer has never
		// converted and so holds no USDT at all. That customer cannot pay: same as being out of funds.
		// If they DO hold enough USDT the mismatch is on our side (e.g. the operating wallet): alert,
		// and leave the charge open without blocking everyone else's.
		bal, berr := t.m.ISpend.Balances(ctx, c.customer)
		if berr != nil {
			t.fail("balances", berr)
			return nil
		}
		if bal.USDTMicro < c.amount {
			return t.unpaid(ctx, c)
		}
		t.m.Log.Error("charge refused as CURRENCY_MISMATCH although the customer holds USDT", "vm_id", c.vmID, "err", err)
		if t.m.Alerts != nil {
			t.m.Alerts.Notify(ctx, "charge-currency-mismatch", time.Hour,
				"iswallet refused a charge as CURRENCY_MISMATCH although the customer holds enough USDT. The Xenos operating wallet may not hold a USDT balance yet; check with iswallet. The charge stays open.")
		}
		return nil
	case errors.Is(err, billing.ErrIdempotencyKeyReused):
		// Cannot succeed on retry: this charge's key was used with different details. Alert (rate
		// limited) and leave the charge open for a human; other charges are unaffected.
		t.m.Log.Error("charge key reused with a different payload", "vm_id", c.vmID, "hour", c.hour, "err", err)
		if t.m.Alerts != nil {
			t.m.Alerts.Notify(ctx, "charge-key-reused", time.Hour, fmt.Sprintf(
				"A usage charge (vm %d, hour %s) was refused by iswallet as IDEMPOTENCY_KEY_REUSED. This is a key-generation bug; the charge stays open until reviewed.", c.vmID, c.hour.Format(time.RFC3339)))
		}
		return nil
	default:
		t.fail("charge", err)
		return nil
	}
}

// unpaid records a charge the customer cannot cover and starts the out-of-funds path.
func (t *tick) unpaid(ctx context.Context, c charge) error {
	if c.status != "unpaid" {
		if err := t.m.Store.Q.MarkChargeUnpaid(ctx, c.id); err != nil {
			return err
		}
	}
	return t.startGrace(ctx, c.userID)
}

// startGrace begins the out-of-funds countdown the first time a charge fails.
func (t *tick) startGrace(ctx context.Context, userID int64) error {
	n, err := t.m.Store.Q.StartGrace(ctx, db.StartGraceParams{ID: userID, GraceStartedAt: tsOf(t.now)})
	if err != nil || n == 0 {
		return err
	}
	u, err := t.m.Store.Q.GetUserByID(ctx, userID)
	if err != nil {
		return err
	}
	t.m.Log.Info("grace period started", "user_id", userID)
	t.mail(ctx, u.Email, "Your Xenos wallet is empty",
		fmt.Sprintf("Your wallet can no longer cover your VMs, so they are being suspended. Top up within %d hours to keep them; after that they and their data are deleted.", int(t.m.Grace.Hours())))
	return nil
}

// chargeNew creates the usage row for every hour that has begun and not yet been
// recorded, then tries to collect it. The row and the billing_from cursor move
// in one transaction, so a crash never skips or repeats an hour.
func (t *tick) chargeNew(ctx context.Context) error {
	vms, err := t.m.Store.Q.ListBillingVMs(ctx)
	if err != nil {
		return err
	}
	for _, v := range vms {
		end := t.now
		if v.BillingUntil.Valid && v.BillingUntil.Time.Before(end) {
			end = v.BillingUntil.Time
		}
		last := end.UTC().Truncate(hour)
		for h := v.BillingFrom.Time.UTC().Truncate(hour); !h.After(last); h = h.Add(hour) {
			if t.now.Before(h.Add(t.m.chargeOffset(v.ID))) {
				break // this hour's charge is not due yet; the cursor stays put and later hours wait behind it
			}
			c, err := t.record(ctx, v, h)
			if err != nil {
				return err
			}
			if c != nil {
				if err := t.attempt(ctx, *c); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// record inserts the usage row for one VM-hour, applying the monthly cap, and
// advances the VM's cursor. It returns the charge to collect, or nil if there is nothing to collect.
func (t *tick) record(ctx context.Context, v db.ListBillingVMsRow, h time.Time) (*charge, error) {
	amount := v.PriceUusdtHourly
	monthStart := time.Date(h.Year(), h.Month(), 1, 0, 0, 0, 0, time.UTC)
	charged, err := t.m.Store.Q.MonthChargedForVM(ctx, db.MonthChargedForVMParams{VmID: v.ID, Hour: monthStart, Hour_2: monthStart.AddDate(0, 1, 0)})
	if err != nil {
		return nil, err
	}
	if rem := v.PriceUusdtMonthlyCap - charged; rem < amount {
		amount = max(rem, 0)
	}
	status := "pending"
	if amount == 0 {
		status = "paid" // monthly cap reached: still one row per hour, nothing to collect
	}

	var id int64
	inserted := true
	err = t.m.Store.InTx(ctx, func(q *db.Queries, _ pgx.Tx) error {
		var err error
		id, err = q.InsertUsageCharge(ctx, db.InsertUsageChargeParams{UserID: v.UserID, VmID: v.ID, Hour: h, AmountUusdt: amount, Status: status})
		if errors.Is(err, pgx.ErrNoRows) {
			inserted = false // already recorded; open ones are handled by retryOpen
			err = nil
		}
		if err != nil {
			return err
		}
		return q.SetBillingFrom(ctx, db.SetBillingFromParams{ID: v.ID, BillingFrom: tsOf(h.Add(hour))})
	})
	if err != nil || !inserted || amount == 0 {
		return nil, err
	}
	u, err := t.m.Store.Q.GetUserByID(ctx, v.UserID)
	if err != nil || !u.IspendCustomerID.Valid {
		return nil, err // no wallet yet: stays pending until the account is linked
	}
	return &charge{id, v.UserID, v.ID, h, amount, "pending", u.IspendCustomerID.String}, nil
}

func (t *tick) finishBilling(ctx context.Context) error {
	return t.m.Store.Q.FinishBilling(ctx)
}

// enforce runs the out-of-funds path for users in grace: suspend their VMs,
// restore them when funded, delete them when the grace period ends.
func (t *tick) enforce(ctx context.Context) error {
	q := t.m.Store.Q
	toSuspend, err := q.ListGraceVMsToSuspend(ctx)
	if err != nil {
		return err
	}
	for _, id := range toSuspend {
		if err := t.m.Jobs.Enqueue(ctx, vm.JobSuspend, vm.Payload{VMID: id}); err != nil {
			return err
		}
	}

	users, err := q.ListGraceUsers(ctx)
	if err != nil {
		return err
	}
	for _, u := range users {
		live, err := q.UserHasLiveVMs(ctx, u.ID)
		if err != nil {
			return err
		}
		if !live { // everything is gone (deleted by the customer or by expiry): the debt no longer needs a countdown
			if err := q.ClearGrace(ctx, u.ID); err != nil {
				return err
			}
			continue
		}
		if !t.now.Before(u.GraceStartedAt.Time.Add(t.m.Grace)) {
			ids, err := q.ListUserVMsToDelete(ctx, u.ID)
			if err != nil {
				return err
			}
			for _, id := range ids {
				t.m.Log.Info("grace expired: deleting vm", "user_id", u.ID, "vm_id", id)
				if err := t.m.Jobs.Enqueue(ctx, vm.JobDelete, vm.Payload{VMID: id}); err != nil {
					return err
				}
			}
			continue
		}
		if err := t.maybeRestore(ctx, u); err != nil {
			return err
		}
	}
	return nil
}

// maybeRestore returns a user's suspended VMs to stopped once every outstanding
// charge is settled and the wallet covers the minimum runway again.
func (t *tick) maybeRestore(ctx context.Context, u db.ListGraceUsersRow) error {
	q := t.m.Store.Q
	if !u.IspendCustomerID.Valid {
		return nil
	}
	owed, err := q.UserUnpaidTotal(ctx, u.ID)
	if err != nil || owed > 0 {
		return err
	}
	vms, err := q.ListUserSuspendedVMs(ctx, u.ID)
	if err != nil || len(vms) == 0 {
		return err
	}
	var hourly int64
	for _, v := range vms {
		hourly += v.PriceUusdtHourly
	}
	bal, ok := t.balance(ctx, u.IspendCustomerID.String)
	if !ok || bal.USDTMicro < hourly*t.m.MinRunwayHours {
		return nil
	}
	err = t.m.Store.InTx(ctx, func(q *db.Queries, tx pgx.Tx) error {
		if err := q.ClearGrace(ctx, u.ID); err != nil {
			return err
		}
		for _, v := range vms {
			if err := jobs.EnqueueTx(ctx, tx, vm.JobResume, vm.Payload{VMID: v.ID}); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		t.m.Log.Info("funded during grace: restoring vms", "user_id", u.ID, "vms", len(vms))
		t.mail(ctx, u.Email, "Your Xenos VMs are back",
			"Your wallet is funded again. Your VMs were returned to the stopped state; start them from the dashboard.")
	}
	return err
}

// warnLow emails customers whose balance covers fewer than MinRunwayHours of usage, at most once a day.
func (t *tick) warnLow(ctx context.Context) error {
	q := t.m.Store.Q
	users, err := q.ListActiveBillingUsers(ctx)
	if err != nil {
		return err
	}
	for _, u := range users {
		if !u.IspendCustomerID.Valid || u.Hourly == 0 {
			continue
		}
		bal, err := t.m.Cache.Get(ctx, u.IspendCustomerID.String)
		if err != nil {
			t.m.Log.Warn("low-balance check skipped", "user_id", u.ID, "err", err)
			continue
		}
		need := u.Hourly * t.m.MinRunwayHours
		if bal.USDTMicro >= need {
			if u.LowBalanceNotifiedAt.Valid {
				if err := q.SetLowBalanceNotified(ctx, db.SetLowBalanceNotifiedParams{ID: u.ID}); err != nil {
					return err
				}
			}
			continue
		}
		if u.LowBalanceNotifiedAt.Valid && t.now.Sub(u.LowBalanceNotifiedAt.Time) < 24*time.Hour {
			continue
		}
		body := fmt.Sprintf("Your wallet covers about %d hours of usage. Top it up to avoid your VMs being suspended.", bal.USDTMicro/u.Hourly)
		if rate, err := t.m.ISpend.Rate(ctx); err == nil {
			shortfall := need - bal.USDTMicro
			kobo := (shortfall*rate + 999_999) / 1_000_000 // round up
			body += fmt.Sprintf(" At today's rate you need about ₦%d.%02d to get back to %d hours.", kobo/100, kobo%100, t.m.MinRunwayHours)
		}
		t.mail(ctx, u.Email, "Low Xenos wallet balance", body)
		if err := q.SetLowBalanceNotified(ctx, db.SetLowBalanceNotifiedParams{ID: u.ID, LowBalanceNotifiedAt: tsOf(t.now)}); err != nil {
			return err
		}
	}
	return nil
}

func (t *tick) mail(ctx context.Context, to, subject, body string) {
	if err := t.m.Mailer.Send(ctx, to, subject, body); err != nil {
		t.m.Log.Error("send email", "to", to, "err", err)
	}
}

func tsOf(v time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: v, Valid: true} }
func textOf(v string) pgtype.Text         { return pgtype.Text{String: v, Valid: true} }
