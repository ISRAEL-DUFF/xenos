package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenos/internal/accounts"
	"github.com/israel-duff/xenos/internal/billing"
	"github.com/israel-duff/xenos/internal/store/db"
	"github.com/israel-duff/xenos/internal/vm"
)

const (
	maxAdjustmentUUSDT = 1_000 * 1_000_000 // 1,000 USDT per adjustment, a guard against typos
	// maxAdjustmentsPerDay bounds what one admin account can move in 24 hours (credits and debits both
	// count), so a stolen admin session cannot drain the operating wallet.
	maxAdjustmentsPerDay = 5_000 * 1_000_000
	minNoteLen           = 3
	maxNoteLen           = 200
)

// audit records an admin action. A failure to audit is logged but does not undo the action.
func (s *Server) audit(r *http.Request, action, target string, detail any) {
	if err := s.auditErr(r, action, target, detail); err != nil {
		s.Log.Error("audit write failed", "action", action, "target", target, "err", err)
	}
}

// auditErr is audit for actions that must not happen unrecorded (moving money): the caller aborts on error.
func (s *Server) auditErr(r *http.Request, action, target string, detail any) error {
	b, err := json.Marshal(detail)
	if err != nil {
		b = []byte("{}")
	}
	return s.Store.Q.InsertAudit(r.Context(), db.InsertAuditParams{
		AdminID: principalFrom(r.Context()).User.ID, Action: action, Target: target, Detail: b})
}

func likeEscape(q string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.TrimSpace(q))
}

func limitOffset(r *http.Request, def, max int) (int32, int32) {
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if n <= 0 || n > max {
		n = def
	}
	o, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if o < 0 {
		o = 0
	}
	return int32(n), int32(o)
}

// ---- users ----

type adminUserJSON struct {
	ID            int64      `json:"id"`
	Email         string     `json:"email"`
	Status        string     `json:"status"`
	IsAdmin       bool       `json:"is_admin"`
	VMLimit       int32      `json:"vm_limit"`
	VMCount       int32      `json:"vm_count"`
	EmailVerified bool       `json:"email_verified"`
	CreatedAt     time.Time  `json:"created_at"`
	USDTMicro     *int64     `json:"usdt_uusdt"` // null if iSpend could not be reached
	NGNKobo       *int64     `json:"ngn_kobo"`
	GraceEndsAt   *time.Time `json:"grace_ends_at,omitempty"`
}

func (s *Server) adminListUsers(w http.ResponseWriter, r *http.Request) {
	limit, offset := limitOffset(r, 50, 100)
	rows, err := s.Store.Q.AdminListUsers(r.Context(), db.AdminListUsersParams{
		Column1: likeEscape(r.URL.Query().Get("q")), Limit: limit, Offset: offset})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]adminUserJSON, 0, len(rows))
	for _, u := range rows {
		j := adminUserJSON{ID: u.ID, Email: u.Email, Status: u.Status, IsAdmin: u.IsAdmin, VMLimit: u.VmLimit,
			VMCount: u.VmCount, EmailVerified: u.EmailVerifiedAt.Valid, CreatedAt: u.CreatedAt}
		if u.IspendCustomerID.Valid {
			if b, err := s.Cache.Get(r.Context(), u.IspendCustomerID.String); err == nil {
				j.USDTMicro, j.NGNKobo = &b.USDTMicro, &b.NGNKobo
			}
		}
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) adminGetUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	u, err := s.Store.Q.GetUserByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	} else if err != nil {
		s.fail(w, r, err)
		return
	}
	count, err := s.Store.Q.CountActiveVMs(ctx, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	user := adminUserJSON{ID: u.ID, Email: u.Email, Status: u.Status, IsAdmin: u.IsAdmin, VMLimit: u.VmLimit,
		VMCount: int32(count), EmailVerified: u.EmailVerifiedAt.Valid, CreatedAt: u.CreatedAt}
	if u.GraceStartedAt.Valid {
		end := u.GraceStartedAt.Time.Add(s.Cfg.Grace())
		user.GraceEndsAt = &end
	}
	if u.IspendCustomerID.Valid {
		if b, err := s.ISpend.Balances(ctx, u.IspendCustomerID.String); err == nil {
			user.USDTMicro, user.NGNKobo = &b.USDTMicro, &b.NGNKobo
		}
	}

	vms, err := s.Store.Q.ListUserVMs(ctx, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	vmOut := make([]vmJSON, 0, len(vms))
	for _, v := range vms {
		vmOut = append(vmOut, newVMJSON(v.ID, v.Hostname, v.Region, v.PlanSlug, v.TemplateSlug, v.State, v.Ipv4, v.Ipv6, v.CiUser, v.PriceUusdtHourly, v.CreatedAt, v.Busy))
	}
	adj, err := s.Store.Q.ListUserAdjustments(ctx, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	adjOut := make([]map[string]any, 0, len(adj))
	for _, a := range adj {
		adjOut = append(adjOut, map[string]any{"id": a.ID, "amount_uusdt": a.AmountUusdt, "note": a.Note,
			"status": a.Status, "created_at": a.CreatedAt, "admin": a.AdminEmail})
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": user, "vms": vmOut, "adjustments": adjOut})
}

// adminUpdateUser changes an account's status or VM limit. Admin accounts are
// protected: their status is changed from the CLI, not through the web.
func (s *Server) adminUpdateUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in struct {
		Status  *string `json:"status"`
		VMLimit *int32  `json:"vm_limit"`
	}
	if !decode(w, r, &in) {
		return
	}
	ctx := r.Context()
	target, err := s.Store.Q.GetUserByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	} else if err != nil {
		s.fail(w, r, err)
		return
	}

	result := map[string]any{}
	if in.VMLimit != nil {
		if *in.VMLimit < 0 || *in.VMLimit > 100 {
			writeErr(w, http.StatusBadRequest, "vm_limit must be between 0 and 100")
			return
		}
		if _, err := s.Store.Q.SetUserVMLimit(ctx, db.SetUserVMLimitParams{ID: id, VmLimit: *in.VMLimit}); err != nil {
			s.fail(w, r, err)
			return
		}
		s.audit(r, "user.vm_limit", target.Email, map[string]any{"from": target.VmLimit, "to": *in.VMLimit})
		result["vm_limit"] = *in.VMLimit
	}
	if in.Status != nil {
		switch *in.Status {
		case "active", "suspended", "banned":
		default:
			writeErr(w, http.StatusBadRequest, "status must be active, suspended or banned")
			return
		}
		if target.IsAdmin {
			writeErr(w, http.StatusForbidden, "admin accounts cannot be suspended or banned from the web")
			return
		}
		var n int
		var err error
		switch *in.Status {
		case "banned":
			n, err = accounts.Ban(ctx, s.Store, s.Jobs, id)
			result["vms_suspending"] = &n
		case "suspended":
			n, err = accounts.Suspend(ctx, s.Store, s.Jobs, id)
			result["vms_suspending"] = &n
		default:
			n, err = accounts.Reactivate(ctx, s.Store, s.Jobs, id)
			result["vms_resuming"] = &n
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		s.audit(r, "user.status", target.Email, map[string]any{"from": target.Status, "to": *in.Status})
		result["status"] = *in.Status
	}
	if len(result) == 0 {
		writeErr(w, http.StatusBadRequest, "nothing to change")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// requestIDRe is the shape of a client-chosen adjustment request id (a UUID or similar).
var requestIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

// adminAdjust applies a manual balance change through iSpend and records it with the admin's note.
// The caller supplies a request_id: the same id from the same admin is the same adjustment, so a retry
// after a timeout or crash resumes the original (same iSpend idempotency key) instead of paying twice.
func (s *Server) adminAdjust(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in struct {
		AmountUUSDT int64  `json:"amount_uusdt"`
		Note        string `json:"note"`
		RequestID   string `json:"request_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	note := strings.TrimSpace(in.Note)
	switch {
	case !requestIDRe.MatchString(in.RequestID):
		writeErr(w, http.StatusBadRequest, "request_id is required (8-64 letters, digits, - or _): reuse it to retry safely")
		return
	case in.AmountUUSDT == 0:
		writeErr(w, http.StatusBadRequest, "amount must not be zero")
		return
	case in.AmountUUSDT > maxAdjustmentUUSDT || in.AmountUUSDT < -maxAdjustmentUUSDT:
		writeErr(w, http.StatusBadRequest, "amount is over the per-adjustment limit of 1,000 USDT")
		return
	case len(note) < minNoteLen || len(note) > maxNoteLen:
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("a note of %d-%d characters is required", minNoteLen, maxNoteLen))
		return
	}
	ctx := r.Context()
	target, err := s.Store.Q.GetUserByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	} else if err != nil {
		s.fail(w, r, err)
		return
	}
	if !target.IspendCustomerID.Valid {
		writeErr(w, http.StatusConflict, "this user has no wallet yet")
		return
	}
	adminID := principalFrom(ctx).User.ID
	reqID := textOf(in.RequestID)

	// A repeat of an earlier request resumes it; it never creates a second adjustment.
	adjID, err := s.Store.Q.CreateAdjustment(ctx, db.CreateAdjustmentParams{AdminID: adminID, UserID: id, AmountUusdt: in.AmountUUSDT, Note: note, RequestID: reqID})
	if errors.Is(err, pgx.ErrNoRows) {
		prev, err := s.Store.Q.GetAdjustmentByRequest(ctx, db.GetAdjustmentByRequestParams{AdminID: adminID, RequestID: reqID})
		if err != nil {
			s.fail(w, r, err)
			return
		}
		switch {
		case prev.UserID != id || prev.AmountUusdt != in.AmountUUSDT || prev.Note != note:
			writeErr(w, http.StatusConflict, "this request_id was already used for a different adjustment")
			return
		case prev.Status == "complete":
			writeJSON(w, http.StatusOK, map[string]any{"id": prev.ID, "amount_uusdt": prev.AmountUusdt, "note": prev.Note, "status": "complete"})
			return
		case prev.Status == "failed":
			writeErr(w, http.StatusConflict, "that request failed earlier: send it again with a new request_id")
			return
		}
		adjID = prev.ID // still pending (a crash after the money moved, or a timeout): replay it below
	} else if err != nil {
		s.fail(w, r, err)
		return
	} else {
		moved, err := s.Store.Q.SumAdminAdjustments24h(ctx, adminID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if moved > maxAdjustmentsPerDay { // this row is already counted in the sum
			_ = s.Store.Q.FailAdjustment(ctx, db.FailAdjustmentParams{ID: adjID, LastError: textOf("daily adjustment limit")})
			s.audit(r, "balance.adjust.refused", target.Email, map[string]any{"adjustment": adjID, "amount_uusdt": in.AmountUUSDT, "reason": "daily limit"})
			writeErr(w, http.StatusTooManyRequests, "this admin account has reached its 24-hour adjustment limit of 5,000 USDT")
			return
		}
	}

	// Record the intent before any money moves: an adjustment that cannot be audited does not happen.
	if err := s.auditErr(r, "balance.adjust.start", target.Email, map[string]any{"adjustment": adjID, "amount_uusdt": in.AmountUUSDT, "note": note}); err != nil {
		s.fail(w, r, err)
		return
	}
	mv, err := s.ISpend.Adjust(ctx, "adjustment:"+strconv.FormatInt(adjID, 10), target.IspendCustomerID.String, in.AmountUUSDT, note)
	if err != nil {
		_ = s.Store.Q.FailAdjustment(ctx, db.FailAdjustmentParams{ID: adjID, LastError: textOf(err.Error())})
		s.audit(r, "balance.adjust.failed", target.Email, map[string]any{"adjustment": adjID, "amount_uusdt": in.AmountUUSDT, "error": err.Error()})
		status := http.StatusBadGateway
		msg := "the wallet service rejected or could not complete the adjustment"
		if errors.Is(err, billing.ErrCurrencyMismatch) {
			status, msg = http.StatusConflict, "the wallets involved do not both hold USDT yet (a customer who has never converted, or an operating wallet that has not received any USDT)"
		}
		if errors.Is(err, billing.ErrInsufficientFunds) {
			status = http.StatusConflict
			if in.AmountUUSDT > 0 {
				msg = "the Xenos merchant wallet does not hold enough USDT to pay this credit"
			} else {
				msg = "the customer's balance is too low for this debit"
			}
		}
		writeErr(w, status, msg)
		return
	}
	if err := s.Store.Q.CompleteAdjustment(ctx, db.CompleteAdjustmentParams{ID: adjID, IspendMovementID: textOf(mv.ID)}); err != nil {
		s.fail(w, r, err) // the money moved; a retry with the same request_id replays the idempotent transfer and completes it
		return
	}
	s.Cache.Invalidate(target.IspendCustomerID.String)
	s.audit(r, "balance.adjust", target.Email, map[string]any{"adjustment": adjID, "amount_uusdt": in.AmountUUSDT, "note": note})
	writeJSON(w, http.StatusCreated, map[string]any{"id": adjID, "amount_uusdt": in.AmountUUSDT, "note": note, "status": "complete"})
}

// ---- VMs ----

func (s *Server) adminListVMs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rows, err := s.Store.Q.AdminListVMs(r.Context(), db.AdminListVMsParams{
		Column1: q.Get("state"), Column2: likeEscape(q.Get("q")), Column3: q.Get("flagged") == "true"})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, v := range rows {
		item := map[string]any{"id": v.ID, "hostname": v.Hostname, "state": v.State, "region": v.Region, "plan": v.PlanSlug,
			"price_uusdt_hourly": v.PriceUusdtHourly, "ipv4": v.Ipv4, "ipv6": v.Ipv6, "user_id": v.UserID, "owner": v.Email,
			"port25_unblocked": v.Port25Unblocked, "created_at": v.CreatedAt, "flagged": v.FlaggedAt.Valid, "flag_reason": v.FlagReason}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) adminVMAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		ctx := r.Context()
		v, err := s.Store.Q.GetVMForWork(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && v.State == "deleted") {
			writeErr(w, http.StatusNotFound, "not found")
			return
		} else if err != nil {
			s.fail(w, r, err)
			return
		}
		target := "vm:" + strconv.FormatInt(id, 10)
		switch action {
		case "stop":
			if v.State != "running" {
				writeErr(w, http.StatusConflict, "only a running VM can be stopped")
				return
			}
			err = s.Jobs.Enqueue(ctx, vm.JobPower, vm.Payload{VMID: id, Action: vm.ActionStop})
		case "delete":
			if v.State == "deleting" {
				writeJSON(w, http.StatusAccepted, map[string]string{"status": "already deleting"})
				return
			}
			err = s.Jobs.Enqueue(ctx, vm.JobDelete, vm.Payload{VMID: id})
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		s.audit(r, "vm."+action, target, map[string]any{"state": v.State})
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
	}
}

// adminPort25 flips the exemption flag. The block itself is an nftables rule on
// the Proxmox host, so the response says the rules must be regenerated and loaded.
func (s *Server) adminPort25(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in struct {
		Allow *bool `json:"allow"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Allow == nil {
		writeErr(w, http.StatusBadRequest, "allow is required")
		return
	}
	n, err := s.Store.Q.SetPort25(r.Context(), db.SetPort25Params{ID: id, Port25Unblocked: *in.Allow})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if n == 0 {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	s.audit(r, "vm.port25", "vm:"+strconv.FormatInt(id, 10), map[string]any{"allow": *in.Allow})
	writeJSON(w, http.StatusOK, map[string]any{"port25_unblocked": *in.Allow, "firewall_reload_required": true,
		"next_step": "run `xenosctl firewall nft` and load the output on the Proxmox host"})
}

// ---- capacity, jobs, revenue ----

type capacityJSON struct {
	VCPU struct {
		Committed int64  `json:"committed"`
		Physical  *int64 `json:"physical"`
	} `json:"vcpu"`
	RAMMB struct {
		Committed int64  `json:"committed"`
		Physical  *int64 `json:"physical"`
	} `json:"ram_mb"`
	Pool *struct {
		Name     string  `json:"name"`
		Used     int64   `json:"used_bytes"`
		Total    int64   `json:"total_bytes"`
		Fraction float64 `json:"fraction"`
	} `json:"pool"`
	IPs struct {
		Free  int64 `json:"free"`
		Total int64 `json:"total"`
	} `json:"ips"`
	HostReachable bool `json:"host_reachable"`
}

func (s *Server) adminCapacity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var out capacityJSON
	var err error
	if out.VCPU.Committed, err = s.Store.Q.CommittedVCPU(ctx); err != nil {
		s.fail(w, r, err)
		return
	}
	if out.RAMMB.Committed, err = s.Store.Q.CommittedRAMMB(ctx); err != nil {
		s.fail(w, r, err)
		return
	}
	if out.IPs.Free, err = s.Store.Q.CountFreeIPs(ctx); err != nil {
		s.fail(w, r, err)
		return
	}
	if out.IPs.Total, err = s.Store.Q.CountTotalIPs(ctx); err != nil {
		s.fail(w, r, err)
		return
	}
	if s.PVE != nil {
		if node, err := s.PVE.NodeInfo(ctx); err == nil {
			cpus, ramMB := int64(node.CPUs), node.MemTotal>>20
			out.VCPU.Physical, out.RAMMB.Physical, out.HostReachable = &cpus, &ramMB, true
			if pool, err := s.PVE.StoragePool(ctx, s.Cfg.PVEStorage); err == nil {
				out.Pool = &struct {
					Name     string  `json:"name"`
					Used     int64   `json:"used_bytes"`
					Total    int64   `json:"total_bytes"`
					Fraction float64 `json:"fraction"`
				}{s.Cfg.PVEStorage, pool.Used, pool.Total, pool.Fraction()}
			}
		} else {
			s.Log.Warn("capacity: proxmox unreachable", "err", err)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) adminListJobs(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "failed"
	}
	rows, err := s.Store.Q.ListJobsByStatus(r.Context(), status)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, j := range rows {
		out = append(out, map[string]any{"id": j.ID, "kind": j.Kind, "payload": json.RawMessage(j.Payload),
			"status": j.Status, "attempts": j.Attempts, "last_error": j.LastError, "created_at": j.CreatedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) adminRetryJob(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	n, err := s.Store.Q.RetryJob(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if n == 0 {
		writeErr(w, http.StatusNotFound, "no failed job with that id")
		return
	}
	s.audit(r, "job.retry", "job:"+strconv.FormatInt(id, 10), map[string]any{})
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
}

func (s *Server) adminRevenue(w http.ResponseWriter, r *http.Request) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 || days > 90 {
		days = 30
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	rows, err := s.Store.Q.RevenueByDay(r.Context(), db.RevenueByDayParams{Column1: today.AddDate(0, 0, -(days - 1)), Column2: today})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var totals struct{ NGN, Converted, Usage int64 }
	daysOut := make([]map[string]any, 0, len(rows))
	for _, d := range rows {
		totals.NGN += d.ConvertedNgnKobo
		totals.Converted += d.ConvertedUusdt
		totals.Usage += d.UsageUusdt
		daysOut = append(daysOut, map[string]any{"date": d.Day.Time.Format("2006-01-02"), "converted_ngn_kobo": d.ConvertedNgnKobo,
			"converted_uusdt": d.ConvertedUusdt, "usage_uusdt": d.UsageUusdt})
	}
	fx := map[string]any{"managed_in": "iSpend", "quoting_paused": true}
	if rate, err := s.ISpend.Rate(r.Context()); err == nil && rate > 0 {
		fx["rate_kobo_per_usdt"], fx["quoting_paused"] = rate, false
	}
	// The operating wallet pays admin credits, so show what it holds (null if iswallet cannot be reached).
	var merchant *int64
	if bal, err := s.ISpend.MerchantBalance(r.Context()); err == nil {
		merchant = &bal
	}
	writeJSON(w, http.StatusOK, map[string]any{"days": daysOut, "fx": fx, "merchant_uusdt": merchant, "totals": map[string]int64{
		"converted_ngn_kobo": totals.NGN, "converted_uusdt": totals.Converted, "usage_uusdt": totals.Usage}})
}
