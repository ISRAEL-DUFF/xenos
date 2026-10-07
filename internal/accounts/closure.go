package accounts

import (
	"archive/zip"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/israel-duff/xenos/internal/billing"
	"github.com/israel-duff/xenos/internal/jobs"
	"github.com/israel-duff/xenos/internal/store"
	"github.com/israel-duff/xenos/internal/store/db"
	"github.com/israel-duff/xenos/internal/vm"
)

// Closing an account. It is refused while there is still something to settle, so closing never destroys money
// or leaves a bill; after it, the customer cannot sign in, and 30 days later (the grace in which support can
// reopen the account) the personal data is removed. The row stays, anonymised, with the charges, conversions
// and adjustments that must be kept for the retention period.

// Dust is the balance below which closure goes ahead: a few cents of USDT and a hundred naira.
const (
	DustUUSDT = 500_000
	DustKobo  = 10_000
)

// Blocker is one reason an account cannot be closed yet.
type Blocker struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// CloseOptions tune Close.
type CloseOptions struct {
	// DeleteVMs queues the deletion of every VM instead of refusing while any exist.
	DeleteVMs bool
	// Settled is for the operator: the customer was paid back by hand, so the balance check is skipped.
	Settled bool
}

// CloseBlockers lists what stands in the way of closing u. iSpend is only consulted for the balance.
func CloseBlockers(ctx context.Context, st *store.Store, is billing.ISpend, u db.User, opt CloseOptions) ([]Blocker, error) {
	var out []Blocker
	// A suspended account cannot close itself: that would shed the suspension and erase the evidence.
	if u.Status != "active" {
		return nil, fmt.Errorf("the account is %s", u.Status)
	}
	if !opt.DeleteVMs {
		if live, err := st.Q.UserHasLiveVMs(ctx, u.ID); err != nil {
			return nil, err
		} else if live {
			out = append(out, Blocker{"vms", "Delete your VMs first, or tick the box that deletes them for you."})
		}
	}
	if n, err := st.Q.CountUserFloatingIPs(ctx, pgtype.Int8{Int64: u.ID, Valid: true}); err != nil {
		return nil, err
	} else if n > 0 {
		out = append(out, Blocker{"floating", "Release your floating IPs first: they are billed by the hour until you do."})
	}
	if n, err := st.Q.CountUserNetworks(ctx, u.ID); err != nil {
		return nil, err
	} else if n > 0 {
		out = append(out, Blocker{"networks", "Delete your private networks first."})
	}
	if owed, err := st.Q.UserUnpaidTotal(ctx, u.ID); err != nil {
		return nil, err
	} else if owed > 0 {
		out = append(out, Blocker{"unpaid", "You have charges that are not paid yet. Top up so they can be collected, then close the account."})
	}
	if !opt.Settled && u.IspendCustomerID.Valid {
		b, err := is.Balances(ctx, u.IspendCustomerID.String)
		if err != nil {
			return nil, fmt.Errorf("could not read the wallet: %w", err)
		}
		if b.USDTMicro > DustUUSDT || b.NGNKobo > DustKobo {
			out = append(out, Blocker{"balance", "Your wallet still holds credit. We cannot pay it out automatically: contact support and we will settle it with you, then close the account."})
		}
	}
	return out, nil
}

// Close moves the account into its closing period, signs it out everywhere and revokes its API tokens. When
// there are blockers it changes nothing and returns them.
func Close(ctx context.Context, st *store.Store, q *jobs.Queue, is billing.ISpend, u db.User, opt CloseOptions) ([]Blocker, error) {
	blockers, err := CloseBlockers(ctx, st, is, u, opt)
	if err != nil || len(blockers) > 0 {
		return blockers, err
	}
	err = st.InTx(ctx, func(qr *db.Queries, tx pgx.Tx) error {
		// Serialise with VM creation and deposits, then re-check what the earlier look may have missed.
		if status, err := qr.LockUserStatus(ctx, u.ID); err != nil {
			return err
		} else if status != "active" {
			return errors.New("the account is not active")
		}
		if n, err := qr.CountUserFloatingIPs(ctx, pgtype.Int8{Int64: u.ID, Valid: true}); err != nil {
			return err
		} else if n > 0 {
			return errStillHasFloating
		}
		if n, err := qr.CountUserNetworks(ctx, u.ID); err != nil {
			return err
		} else if n > 0 {
			return errStillHasNetworks
		}
		if !opt.DeleteVMs {
			if live, err := qr.UserHasLiveVMs(ctx, u.ID); err != nil {
				return err
			} else if live {
				return errStillHasVMs
			}
		}
		if n, err := qr.BeginClosure(ctx, u.ID); err != nil {
			return err
		} else if n == 0 {
			return errors.New("the account is already closing or closed")
		}
		if err := qr.DeleteUserSessions(ctx, u.ID); err != nil {
			return err
		}
		if err := qr.RevokeUserAPITokens(ctx, u.ID); err != nil {
			return err
		}
		if opt.DeleteVMs {
			ids, err := qr.ListUserDeletableVMs(ctx, u.ID)
			if err != nil {
				return err
			}
			for _, id := range ids {
				if err := jobs.EnqueueTx(ctx, tx, vm.JobDelete, vm.Payload{VMID: id}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if errors.Is(err, errStillHasNetworks) {
		return []Blocker{{"networks", "Delete your private networks first."}}, nil
	}
	if errors.Is(err, errStillHasFloating) {
		return []Blocker{{"floating", "Release your floating IPs first: they are billed by the hour until you do."}}, nil
	}
	if errors.Is(err, errStillHasVMs) {
		return []Blocker{{"vms", "Delete your VMs first, or tick the box that deletes them for you."}}, nil
	}
	return nil, err
}

var errStillHasNetworks = errors.New("the account still has private networks")

var errStillHasFloating = errors.New("the account still has floating IPs")

var errStillHasVMs = errors.New("the account still has VMs")

// Reopen undoes a closure inside its grace period.
func Reopen(ctx context.Context, st *store.Store, userID int64) error {
	return st.InTx(ctx, func(q *db.Queries, tx pgx.Tx) error {
		n, err := q.ReopenAccount(ctx, userID)
		if err != nil {
			return err
		}
		if n == 0 {
			return errors.New("the account is not in its closing period")
		}
		// Anything created by a request that was in flight during the closure must not come back to life.
		if err := q.DeleteUserSessions(ctx, userID); err != nil {
			return err
		}
		return q.RevokeUserAPITokens(ctx, userID)
	})
}

// PurgeDue removes the personal data of accounts whose grace period has ended and returns how many it purged.
// An account that still has VMs on the host waits (their deletion is queued when the account is closed).
func PurgeDue(ctx context.Context, st *store.Store, now time.Time) (int, error) {
	due, err := st.Q.ListClosingDue(ctx, pgtype.Timestamptz{Time: now, Valid: true})
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, d := range due {
		if live, err := st.Q.UserHasLiveVMs(ctx, d.ID); err != nil {
			errs = append(errs, err)
			continue
		} else if live {
			continue
		}
		anon := "closed-" + strconv.FormatInt(d.ID, 10) + "@invalid"
		err := st.InTx(ctx, func(q *db.Queries, tx pgx.Tx) error {
			for _, step := range []func() error{
				func() error { return q.ScrubClosedUserVMs(ctx, d.ID) },
				func() error { return q.DeleteUserSSHKeys(ctx, d.ID) },
				func() error { return q.DeleteUserVerificationRows(ctx, d.ID) },
				func() error { return q.DeleteUserResetRows(ctx, d.ID) },
				func() error { return q.DeleteUserSessions(ctx, d.ID) },
				func() error { return q.DeleteUserAPITokens(ctx, d.ID) },
				func() error {
					return q.ScrubAuditTarget(ctx, db.ScrubAuditTargetParams{Target: d.Email, Target_2: anon})
				},
				func() error { return q.AnonymiseClosedUser(ctx, d.ID) },
				func() error {
					return q.InsertAudit(ctx, db.InsertAuditParams{Source: textOf("system"), Action: "account.purged", Target: anon, Detail: []byte("{}")})
				},
			} {
				if err := step(); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("purge user %d: %w", d.ID, err))
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

// ---- export ----

// Export writes everything held about the account as a zip: profile, keys, VMs, charges, conversions,
// adjustments and token metadata. It contains no password hash and no secret of any kind, and only this
// account's rows. Rows are written as they are read.
func Export(ctx context.Context, w io.Writer, st *store.Store, u db.User) error {
	zw := zip.NewWriter(w)
	file := func(name string) (*csv.Writer, error) {
		f, err := zw.Create(name)
		if err != nil {
			return nil, err
		}
		return csv.NewWriter(f), nil
	}
	ts := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }

	prof, err := zw.Create("profile.json")
	if err != nil {
		return err
	}
	p := map[string]any{"id": u.ID, "email": u.Email, "phone": u.Phone, "status": u.Status, "vm_limit": u.VmLimit,
		"auto_convert": u.AutoConvert, "created_at": ts(u.CreatedAt), "email_verified": u.EmailVerifiedAt.Valid, "exported_at": ts(time.Now())}
	if u.EmailVerifiedAt.Valid {
		p["email_verified_at"] = ts(u.EmailVerifiedAt.Time)
	}
	if u.AupAcceptedAt.Valid {
		p["acceptable_use_accepted_at"] = ts(u.AupAcceptedAt.Time)
	}
	if err := json.NewEncoder(prof).Encode(p); err != nil {
		return err
	}

	keys, err := st.Q.ExportSSHKeys(ctx, u.ID)
	if err != nil {
		return err
	}
	cw, err := file("ssh_keys.csv")
	if err != nil {
		return err
	}
	_ = cw.Write([]string{"name", "fingerprint", "public_key", "created_at"})
	for _, k := range keys {
		_ = cw.Write([]string{cell(k.Name), k.Fingerprint, cell(k.PublicKey), ts(k.CreatedAt)})
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return err
	}

	vms, err := st.Q.ExportVMs(ctx, u.ID)
	if err != nil {
		return err
	}
	if cw, err = file("vms.csv"); err != nil {
		return err
	}
	_ = cw.Write([]string{"id", "hostname", "region", "plan", "template", "state", "ipv4", "ipv6", "labels", "created_at", "deleted_at"})
	for _, v := range vms {
		deleted := ""
		if v.DeletedAt.Valid {
			deleted = ts(v.DeletedAt.Time)
		}
		_ = cw.Write([]string{strconv.FormatInt(v.ID, 10), cell(v.Hostname), v.Region, v.PlanSlug, v.TemplateSlug, v.State, v.Ipv4, v.Ipv6, cell(string(v.Labels)), ts(v.CreatedAt), deleted})
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return err
	}

	charges, err := st.Q.ExportCharges(ctx, u.ID)
	if err != nil {
		return err
	}
	if cw, err = file("charges.csv"); err != nil {
		return err
	}
	_ = cw.Write([]string{"id", "vm_id", "floating_ip_id", "hour_utc", "amount_uusdt", "status"})
	for _, c := range charges {
		_ = cw.Write([]string{strconv.FormatInt(c.ID, 10), strconv.FormatInt(c.VmID, 10), strconv.FormatInt(c.FloatingIpID, 10), ts(c.Hour), strconv.FormatInt(c.AmountUusdt, 10), c.Status})
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return err
	}

	convs, err := st.Q.ExportConversions(ctx, u.ID)
	if err != nil {
		return err
	}
	if cw, err = file("conversions.csv"); err != nil {
		return err
	}
	_ = cw.Write([]string{"id", "amount_ngn_kobo", "amount_uusdt", "rate_kobo_per_usdt", "status", "created_at"})
	for _, c := range convs {
		usdt := ""
		if c.AmountUusdt.Valid {
			usdt = strconv.FormatInt(c.AmountUusdt.Int64, 10)
		}
		_ = cw.Write([]string{strconv.FormatInt(c.ID, 10), strconv.FormatInt(c.AmountNgnKobo, 10), usdt, c.Rate, c.Status, ts(c.CreatedAt)})
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return err
	}

	adjs, err := st.Q.ExportAdjustments(ctx, u.ID)
	if err != nil {
		return err
	}
	if cw, err = file("adjustments.csv"); err != nil {
		return err
	}
	_ = cw.Write([]string{"id", "amount_uusdt", "note", "status", "created_at"})
	for _, a := range adjs {
		_ = cw.Write([]string{strconv.FormatInt(a.ID, 10), strconv.FormatInt(a.AmountUusdt, 10), cell(a.Note), a.Status, ts(a.CreatedAt)})
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return err
	}

	toks, err := st.Q.ExportTokens(ctx, u.ID)
	if err != nil {
		return err
	}
	if cw, err = file("api_tokens.csv"); err != nil {
		return err
	}
	_ = cw.Write([]string{"name", "prefix", "created_at", "last_used_at", "expires_at", "revoked_at"})
	for _, t := range toks {
		row := []string{t.Name, t.Prefix, ts(t.CreatedAt), "", "", ""}
		if t.LastUsedAt.Valid {
			row[3] = ts(t.LastUsedAt.Time)
		}
		if t.ExpiresAt.Valid {
			row[4] = ts(t.ExpiresAt.Time)
		}
		if t.RevokedAt.Valid {
			row[5] = ts(t.RevokedAt.Time)
		}
		_ = cw.Write(row)
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return err
	}
	return zw.Close()
}

func textOf(v string) pgtype.Text { return pgtype.Text{String: v, Valid: true} }

// cell neutralises spreadsheet formulas in text a person typed (hostnames, labels, notes).
func cell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}
