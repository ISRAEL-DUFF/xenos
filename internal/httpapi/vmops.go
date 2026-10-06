package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/israel-duff/xenos/internal/jobs"
	"github.com/israel-duff/xenos/internal/store/db"
	"github.com/israel-duff/xenos/internal/vm"
)

// maxSnapshotsPerVM bounds how much of the shared thin pool one VM can hold in snapshots.
const maxSnapshotsPerVM = 2

// busyConflict is the answer when a VM cannot take a long operation right now.
const busyConflict = "this VM is busy or not running or stopped: try again in a moment"

// ownedVM loads the caller's VM or writes the 404.
func (s *Server) ownedVM(w http.ResponseWriter, r *http.Request) (db.GetUserVMRow, bool) {
	id, ok := pathID(w, r)
	if !ok {
		return db.GetUserVMRow{}, false
	}
	v, err := s.Store.Q.GetUserVM(r.Context(), db.GetUserVMParams{ID: id, UserID: principalFrom(r.Context()).User.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not found")
		return v, false
	} else if err != nil {
		s.fail(w, r, err)
		return v, false
	}
	return v, true
}

// ---- resize ----

// resizeVM moves a VM to a larger plan. Nothing may shrink (a disk cannot), the wallet must cover the new
// price for the usual runway, and the new price applies from the next hour that is charged.
func (s *Server) resizeVM(w http.ResponseWriter, r *http.Request) {
	v, ok := s.ownedVM(w, r)
	if !ok {
		return
	}
	var in struct {
		Plan string `json:"plan"`
	}
	if !decode(w, r, &in) {
		return
	}
	ctx := r.Context()
	plan, err := s.Store.Q.GetActivePlanBySlug(ctx, in.Plan)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusBadRequest, "unknown plan")
		return
	} else if err != nil {
		s.fail(w, r, err)
		return
	}
	switch {
	case plan.ID == v.PlanID:
		writeErr(w, http.StatusBadRequest, "the VM is already on this plan")
		return
	case plan.Vcpu < v.Vcpu || plan.RamMb < v.RamMb || plan.DiskGb < v.DiskGb || plan.PriceUusdtHourly <= v.PriceUusdtHourly:
		writeErr(w, http.StatusBadRequest, "a VM can only move to a larger plan: CPU, memory and disk cannot shrink")
		return
	}
	if v.Busy.Valid || (v.State != "running" && v.State != "stopped") {
		writeErr(w, http.StatusConflict, busyConflict)
		return
	}
	if n, err := s.Store.Q.CountVMSnapshots(ctx, v.ID); err != nil {
		s.fail(w, r, err)
		return
	} else if n > 0 {
		writeErr(w, http.StatusConflict, "delete this VM's snapshots first: a disk with snapshots cannot be grown safely")
		return
	}

	user := principalFrom(ctx).User
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
	running, err := s.Store.Q.SumActiveHourly(ctx, user.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if need := (running - v.PriceUusdtHourly + plan.PriceUusdtHourly) * minRunwayHours; bal.USDTMicro < need {
		writeErr(w, http.StatusPaymentRequired,
			fmt.Sprintf("balance must cover %d hours of usage for all your VMs after the resize (%d micro-USDT needed, %d available)", minRunwayHours, need, bal.USDTMicro))
		return
	}

	err = s.Store.InTx(ctx, func(q *db.Queries, tx pgx.Tx) error {
		n, err := q.ClaimVMBusy(ctx, db.ClaimVMBusyParams{ID: v.ID, UserID: user.ID, Busy: pgtype.Text{String: "resizing", Valid: true},
			ResizePlanID: pgtype.Int8{Int64: plan.ID, Valid: true}})
		if err != nil {
			return err
		}
		if n == 0 {
			return errAbort
		}
		return jobs.EnqueueTx(ctx, tx, vm.JobResize, vm.Payload{VMID: v.ID})
	})
	if errors.Is(err, errAbort) {
		writeErr(w, http.StatusConflict, busyConflict)
		return
	} else if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued", "plan": plan.Slug})
}

// ---- snapshots ----

type snapshotJSON struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Server) listSnapshots(w http.ResponseWriter, r *http.Request) {
	v, ok := s.ownedVM(w, r)
	if !ok {
		return
	}
	rows, err := s.Store.Q.ListVMSnapshots(r.Context(), v.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]snapshotJSON, 0, len(rows))
	for _, x := range rows {
		out = append(out, snapshotJSON{ID: x.ID, Name: x.Name, Status: x.Status, CreatedAt: x.CreatedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": out, "limit": maxSnapshotsPerVM})
}

func (s *Server) createSnapshot(w http.ResponseWriter, r *http.Request) {
	v, ok := s.ownedVM(w, r)
	if !ok {
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &in) {
		return
	}
	name := strings.TrimSpace(in.Name)
	if l := len([]rune(name)); l < 1 || l > 60 {
		writeErr(w, http.StatusBadRequest, "give the snapshot a name of 1-60 characters")
		return
	}
	ctx := r.Context()
	var sid int64
	var apiErr *apiError
	err := s.Store.InTx(ctx, func(q *db.Queries, tx pgx.Tx) error {
		n, err := q.ClaimVMBusy(ctx, db.ClaimVMBusyParams{ID: v.ID, UserID: principalFrom(ctx).User.ID, Busy: pgtype.Text{String: "snapshotting", Valid: true}})
		if err != nil {
			return err
		}
		if n == 0 {
			apiErr = &apiError{http.StatusConflict, busyConflict}
			return errAbort
		}
		if c, err := q.CountVMSnapshots(ctx, v.ID); err != nil {
			return err
		} else if c >= maxSnapshotsPerVM {
			apiErr = &apiError{http.StatusConflict, fmt.Sprintf("a VM can hold %d snapshots: delete one first", maxSnapshotsPerVM)}
			return errAbort
		}
		if sid, err = q.CreateSnapshot(ctx, db.CreateSnapshotParams{VmID: v.ID, Name: name}); err != nil {
			return err
		}
		if err := q.SetSnapshotPVEName(ctx, db.SetSnapshotPVENameParams{ID: sid, PveName: vm.SnapshotName(sid)}); err != nil {
			return err
		}
		return jobs.EnqueueTx(ctx, tx, vm.JobSnapshot, vm.Payload{VMID: v.ID, SnapshotID: sid})
	})
	if errors.Is(err, errAbort) {
		writeErr(w, apiErr.status, apiErr.msg)
		return
	} else if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, snapshotJSON{ID: sid, Name: name, Status: "creating", CreatedAt: time.Now().UTC()})
}

func snapshotID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "sid"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusNotFound, "not found")
		return 0, false
	}
	return id, true
}

func (s *Server) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	v, ok := s.ownedVM(w, r)
	if !ok {
		return
	}
	sid, ok := snapshotID(w, r)
	if !ok {
		return
	}
	s.snapshotJob(w, r, v, sid, "snapshotting", vm.JobSnapshotDelete, "deleting", func(status string) bool { return status == "ready" || status == "deleting" })
}

// restoreSnapshot rolls the VM's disk back. The customer must confirm: everything written since is lost.
func (s *Server) restoreSnapshot(w http.ResponseWriter, r *http.Request) {
	v, ok := s.ownedVM(w, r)
	if !ok {
		return
	}
	sid, ok := snapshotID(w, r)
	if !ok {
		return
	}
	var in struct {
		Confirm bool `json:"confirm"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !in.Confirm {
		writeErr(w, http.StatusBadRequest, "confirm the restore: everything written since the snapshot will be lost")
		return
	}
	s.snapshotJob(w, r, v, sid, "restoring", vm.JobRestore, "restoring", func(status string) bool { return status == "ready" })
}

// snapshotJob claims the VM and queues a job on one of its snapshots.
func (s *Server) snapshotJob(w http.ResponseWriter, r *http.Request, v db.GetUserVMRow, sid int64, busy, kind, verb string, statusOK func(string) bool) {
	ctx := r.Context()
	var apiErr *apiError
	err := s.Store.InTx(ctx, func(q *db.Queries, tx pgx.Tx) error {
		snap, err := q.GetVMSnapshot(ctx, db.GetVMSnapshotParams{ID: sid, VmID: v.ID})
		if errors.Is(err, pgx.ErrNoRows) {
			apiErr = &apiError{http.StatusNotFound, "not found"}
			return errAbort
		} else if err != nil {
			return err
		}
		if !statusOK(snap.Status) {
			apiErr = &apiError{http.StatusConflict, fmt.Sprintf("this snapshot is %s and cannot be used for that", snap.Status)}
			return errAbort
		}
		n, err := q.ClaimVMBusy(ctx, db.ClaimVMBusyParams{ID: v.ID, UserID: principalFrom(ctx).User.ID, Busy: pgtype.Text{String: busy, Valid: true}})
		if err != nil {
			return err
		}
		if n == 0 {
			apiErr = &apiError{http.StatusConflict, busyConflict}
			return errAbort
		}
		if kind == vm.JobSnapshotDelete {
			if err := q.SetSnapshotStatus(ctx, db.SetSnapshotStatusParams{ID: sid, Status: "deleting"}); err != nil {
				return err
			}
		}
		return jobs.EnqueueTx(ctx, tx, kind, vm.Payload{VMID: v.ID, SnapshotID: sid})
	})
	if errors.Is(err, errAbort) {
		writeErr(w, apiErr.status, apiErr.msg)
		return
	} else if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued", "action": verb})
}

// ---- rebuild ----

// rebuildVM reinstalls the VM from a template. Everything on its disk, snapshots included, is erased, so the
// caller must confirm. The IP, plan and hostname stay. Customers who lost SSH access use this with new keys.
func (s *Server) rebuildVM(w http.ResponseWriter, r *http.Request) {
	v, ok := s.ownedVM(w, r)
	if !ok {
		return
	}
	var in struct {
		Template  string  `json:"template"`
		SSHKeyIDs []int64 `json:"ssh_key_ids"`
		Confirm   bool    `json:"confirm"`
		// BootScript runs once after the rebuild (a rebuild does not keep the previous one: it was erased after it ran).
		BootScript string `json:"boot_script"`
	}
	if !decode(w, r, &in) {
		return
	}
	if msg := validBootScript(in.BootScript); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	if !in.Confirm {
		writeErr(w, http.StatusBadRequest, "confirm the rebuild: everything on the VM's disk, including its snapshots, will be erased")
		return
	}
	ctx := r.Context()
	user := principalFrom(ctx).User
	if !s.rebuildLimit.Allow(strconvID(user.ID)) {
		writeErr(w, http.StatusTooManyRequests, "too many rebuilds, try again later")
		return
	}
	slug := in.Template
	if slug == "" {
		slug = v.TemplateSlug
	}
	tpl, err := s.Store.Q.GetActiveTemplateBySlug(ctx, slug)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusBadRequest, "unknown template")
		return
	} else if err != nil {
		s.fail(w, r, err)
		return
	}
	keys := ""
	if ids := uniqueIDs(in.SSHKeyIDs); len(ids) > 0 {
		list, err := s.Store.Q.GetSSHKeysByIDs(ctx, db.GetSSHKeysByIDsParams{UserID: user.ID, Column2: ids})
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if len(list) != len(ids) {
			writeErr(w, http.StatusBadRequest, "unknown SSH key")
			return
		}
		keys = strings.Join(list, "\n")
	} else { // keep the keys the VM already has
		work, err := s.Store.Q.GetVMForWork(ctx, v.ID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		keys = work.AuthorizedKeys
	}
	if strings.TrimSpace(keys) == "" {
		writeErr(w, http.StatusBadRequest, "choose at least one SSH key")
		return
	}

	err = s.Store.InTx(ctx, func(q *db.Queries, tx pgx.Tx) error {
		n, err := q.ClaimVMRebuild(ctx, db.ClaimVMRebuildParams{ID: v.ID, UserID: user.ID,
			RebuildTemplateID: pgtype.Int8{Int64: tpl.ID, Valid: true}, RebuildKeys: pgtype.Text{String: keys, Valid: true}, RebuildBootScript: bootScriptText(in.BootScript)})
		if err != nil {
			return err
		}
		if n == 0 {
			return errAbort
		}
		return jobs.EnqueueTx(ctx, tx, vm.JobRebuild, vm.Payload{VMID: v.ID})
	})
	if errors.Is(err, errAbort) {
		writeErr(w, http.StatusConflict, busyConflict)
		return
	} else if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued", "template": tpl.Slug})
}

func bootScriptText(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}
