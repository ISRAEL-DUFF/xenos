package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/israel-duff/xenos/internal/jobs"
	"github.com/israel-duff/xenos/internal/store/db"
	"github.com/israel-duff/xenos/internal/vm"
)

type floatingJSON struct {
	ID          int64     `json:"id"`
	Address     string    `json:"address"`
	Region      string    `json:"region"`
	Host        string    `json:"host"`
	Label       string    `json:"label"`
	VMID        *int64    `json:"vm_id"`
	Applied     bool      `json:"applied"` // the guest has been configured to match vm_id
	AllocatedAt time.Time `json:"allocated_at"`
	PriceUUSDT  int64     `json:"price_uusdt_hourly"`
}

func (s *Server) floatingView(id int64, addr, region, host, label string, vmID, applied pgtype.Int8, at pgtype.Timestamptz) floatingJSON {
	f := floatingJSON{ID: id, Address: addr, Region: region, Host: host, Label: label, AllocatedAt: at.Time, PriceUUSDT: s.Cfg.FloatingIPPriceUUSDT}
	if vmID.Valid {
		f.VMID = &vmID.Int64
		f.Applied = applied.Valid && applied.Int64 == vmID.Int64
	} else {
		f.Applied = !applied.Valid
	}
	return f
}

func (s *Server) ownedFloating(w http.ResponseWriter, r *http.Request) (db.GetUserFloatingIPRow, bool) {
	id, ok := pathID(w, r)
	if !ok {
		return db.GetUserFloatingIPRow{}, false
	}
	f, err := s.Store.Q.GetUserFloatingIP(r.Context(), db.GetUserFloatingIPParams{ID: id, UserID: pgtype.Int8{Int64: principalFrom(r.Context()).User.ID, Valid: true}})
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not found")
		return f, false
	} else if err != nil {
		s.fail(w, r, err)
		return f, false
	}
	return f, true
}

func viewOf(s *Server, f db.GetUserFloatingIPRow) floatingJSON {
	return s.floatingView(f.ID, f.Address, f.Region, f.Host, f.Label, f.VmID, f.AppliedVmID, f.AllocatedAt)
}

func (s *Server) listFloatingIPs(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Store.Q.ListUserFloatingIPs(r.Context(), pgtype.Int8{Int64: principalFrom(r.Context()).User.ID, Valid: true})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]floatingJSON, 0, len(rows))
	for _, f := range rows {
		out = append(out, s.floatingView(f.ID, f.Address, f.Region, f.Host, f.Label, f.VmID, f.AppliedVmID, f.AllocatedAt))
	}
	writeJSON(w, http.StatusOK, map[string]any{"floating_ips": out, "limit": s.Cfg.FloatingIPLimit, "price_uusdt_hourly": s.Cfg.FloatingIPPriceUUSDT})
}

func (s *Server) getFloatingIP(w http.ResponseWriter, r *http.Request) {
	if f, ok := s.ownedFloating(w, r); ok {
		writeJSON(w, http.StatusOK, viewOf(s, f))
	}
}

// allocateFloatingIP gives the account an address from the pool. Billing starts at the top of this hour.
func (s *Server) allocateFloatingIP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := principalFrom(ctx).User
	var in struct {
		Label string `json:"label"`
		VMID  *int64 `json:"vm_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	label := strings.TrimSpace(in.Label)
	if len(label) > 64 {
		writeErr(w, http.StatusBadRequest, "the label is at most 64 characters")
		return
	}
	var target db.GetUserVMRow
	if in.VMID != nil {
		v, err := s.Store.Q.GetUserVM(ctx, db.GetUserVMParams{ID: *in.VMID, UserID: user.ID})
		if errors.Is(err, pgx.ErrNoRows) {
			writeErr(w, http.StatusNotFound, "vm not found")
			return
		} else if err != nil {
			s.fail(w, r, err)
			return
		}
		if msg := attachable(v); msg != "" {
			writeErr(w, http.StatusConflict, msg)
			return
		}
		target = v
	}
	if !user.IspendCustomerID.Valid {
		writeErr(w, http.StatusServiceUnavailable, "wallet is not available yet, try again shortly")
		return
	}
	bal, err := s.ISpend.Balances(ctx, user.IspendCustomerID.String)
	if err != nil {
		s.Log.Error("ispend balances", "user_id", user.ID, "err", err)
		writeErr(w, http.StatusServiceUnavailable, "wallet is not available right now, try again shortly")
		return
	}

	var apiErr *apiError
	var created db.ClaimFloatingIPRow
	err = s.Store.InTx(ctx, func(q *db.Queries, tx pgx.Tx) error {
		if status, err := q.LockUserStatus(ctx, user.ID); err != nil {
			return err
		} else if status != "active" {
			apiErr = &apiError{http.StatusForbidden, "your account is suspended or closing; contact support"}
			return errAbort
		}
		n, err := q.CountUserFloatingIPs(ctx, pgtype.Int8{Int64: user.ID, Valid: true})
		if err != nil {
			return err
		}
		if n >= int64(s.Cfg.FloatingIPLimit) {
			apiErr = &apiError{http.StatusConflict, fmt.Sprintf("floating IP limit reached (%d)", s.Cfg.FloatingIPLimit)}
			return errAbort
		}
		running, err := q.SumActiveHourly(ctx, user.ID)
		if err != nil {
			return err
		}
		if need := (running + s.Cfg.FloatingIPPriceUUSDT) * minRunwayHours; bal.USDTMicro < need {
			apiErr = &apiError{http.StatusPaymentRequired, fmt.Sprintf("balance must cover %d hours of usage (%d micro-USDT needed, %d available)", minRunwayHours, need, bal.USDTMicro)}
			return errAbort
		}
		region, wantHost := s.Cfg.Region, ""
		if in.VMID != nil {
			region, wantHost = target.Region, target.Host
		}
		created, err = q.ClaimFloatingIP(ctx, db.ClaimFloatingIPParams{
			UserID: pgtype.Int8{Int64: user.ID, Valid: true}, Label: label,
			BillingFrom: pgtype.Timestamptz{Time: time.Now().UTC().Truncate(time.Hour), Valid: true}, WantRegion: region, WantHost: wantHost})
		if errors.Is(err, pgx.ErrNoRows) {
			apiErr = &apiError{http.StatusServiceUnavailable, "no floating IPs available right now, try again later"}
			return errAbort
		}
		if err != nil {
			return err
		}
		if in.VMID != nil {
			if _, err := q.SetFloatingTarget(ctx, db.SetFloatingTargetParams{ID: created.ID, UserID: pgtype.Int8{Int64: user.ID, Valid: true}, VmID: pgtype.Int8{Int64: target.ID, Valid: true}}); err != nil {
				return err
			}
			return enqueueFloating(ctx, tx, created.ID)
		}
		return nil
	})
	if errors.Is(err, errAbort) && apiErr != nil {
		writeErr(w, apiErr.status, apiErr.msg)
		return
	} else if err != nil {
		s.fail(w, r, err)
		return
	}
	f := s.floatingView(created.ID, created.Address, created.Region, created.Host, created.Label, pgtype.Int8{}, pgtype.Int8{}, created.AllocatedAt)
	if in.VMID != nil {
		f.VMID, f.Applied = in.VMID, false
	}
	writeJSON(w, http.StatusCreated, f)
}

// attachable says why a VM cannot take a floating IP right now.
func attachable(v db.GetUserVMRow) string {
	if v.State != "running" && v.State != "stopped" {
		return "the VM must be running or stopped to take a floating IP"
	}
	return ""
}

// attachFloatingIP points the address at a VM, moving it if it pointed elsewhere. Asking for where it already
// points is a no-op.
func (s *Server) attachFloatingIP(w http.ResponseWriter, r *http.Request) {
	f, ok := s.ownedFloating(w, r)
	if !ok {
		return
	}
	var in struct {
		VMID int64 `json:"vm_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	ctx := r.Context()
	user := principalFrom(ctx).User
	v, err := s.Store.Q.GetUserVM(ctx, db.GetUserVMParams{ID: in.VMID, UserID: user.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "vm not found")
		return
	} else if err != nil {
		s.fail(w, r, err)
		return
	}
	if v.Region != f.Region {
		writeErr(w, http.StatusConflict, "the floating IP and the VM are in different regions")
		return
	}
	if v.Host != f.Host {
		writeErr(w, http.StatusConflict, "the floating IP lives on a different host than the VM: a floating IP can only move between VMs on the same host")
		return
	}
	if msg := attachable(v); msg != "" {
		writeErr(w, http.StatusConflict, msg)
		return
	}
	if f.VmID.Valid && f.VmID.Int64 == v.ID {
		writeJSON(w, http.StatusOK, viewOf(s, f))
		return
	}
	owner := pgtype.Int8{Int64: user.ID, Valid: true}
	err = s.Store.InTx(ctx, func(q *db.Queries, tx pgx.Tx) error {
		if _, err := q.SetFloatingTarget(ctx, db.SetFloatingTargetParams{ID: f.ID, UserID: owner, VmID: pgtype.Int8{Int64: v.ID, Valid: true}}); err != nil {
			return err
		}
		return enqueueFloating(ctx, tx, f.ID)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	f.VmID = pgtype.Int8{Int64: v.ID, Valid: true}
	writeJSON(w, http.StatusAccepted, viewOf(s, f))
}

func (s *Server) detachFloatingIP(w http.ResponseWriter, r *http.Request) {
	f, ok := s.ownedFloating(w, r)
	if !ok {
		return
	}
	if !f.VmID.Valid {
		writeJSON(w, http.StatusOK, viewOf(s, f))
		return
	}
	ctx := r.Context()
	owner := pgtype.Int8{Int64: principalFrom(ctx).User.ID, Valid: true}
	err := s.Store.InTx(ctx, func(q *db.Queries, tx pgx.Tx) error {
		if _, err := q.SetFloatingTarget(ctx, db.SetFloatingTargetParams{ID: f.ID, UserID: owner}); err != nil {
			return err
		}
		return enqueueFloating(ctx, tx, f.ID)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	f.VmID = pgtype.Int8{}
	writeJSON(w, http.StatusAccepted, viewOf(s, f))
}

// releaseFloatingIP returns the address to the pool. Billing stops now (the hour in progress is already paid).
func (s *Server) releaseFloatingIP(w http.ResponseWriter, r *http.Request) {
	f, ok := s.ownedFloating(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	owner := pgtype.Int8{Int64: principalFrom(ctx).User.ID, Valid: true}
	err := s.Store.InTx(ctx, func(q *db.Queries, tx pgx.Tx) error {
		if _, err := q.ReleaseFloatingIP(ctx, db.ReleaseFloatingIPParams{ID: f.ID, UserID: owner, BillingUntil: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}}); err != nil {
			return err
		}
		if f.AppliedVmID.Valid { // still configured on a guest: take it off before the address can be handed out again
			return enqueueFloating(ctx, tx, f.ID)
		}
		return nil
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "released"})
}

func enqueueFloating(ctx context.Context, tx pgx.Tx, id int64) error {
	return jobs.EnqueueTx(ctx, tx, vm.JobFloating, vm.Payload{FloatingID: id})
}
