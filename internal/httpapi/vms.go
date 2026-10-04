package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/israel-duff/xenos/internal/auth"
	"github.com/israel-duff/xenos/internal/jobs"
	"github.com/israel-duff/xenos/internal/store/db"
	"github.com/israel-duff/xenos/internal/vm"
)

// minRunwayHours is how many hours of usage a wallet must cover before it can create a VM.
const minRunwayHours = 24

var hostnameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

type vmJSON struct {
	ID          int64     `json:"id"`
	Hostname    string    `json:"hostname"`
	Region      string    `json:"region"`
	Plan        string    `json:"plan"`
	Template    string    `json:"template"`
	State       string    `json:"state"`
	IPv4        string    `json:"ipv4,omitempty"`
	IPv6        string    `json:"ipv6,omitempty"`
	SSHUser     string    `json:"ssh_user"`
	SSHCommand  string    `json:"ssh_command,omitempty"`
	HourlyUUSDT int64     `json:"price_uusdt_hourly"`
	CreatedAt   time.Time `json:"created_at"`
}

func newVMJSON(id int64, hostname, region, plan, tpl, state, ipv4 string, ipv6 pgtype.Text, user string, hourly int64, created time.Time) vmJSON {
	v := vmJSON{ID: id, Hostname: hostname, Region: region, Plan: plan, Template: tpl, State: state,
		IPv4: ipv4, SSHUser: user, HourlyUUSDT: hourly, CreatedAt: created}
	if ipv6.Valid {
		v.IPv6 = ipv6.String
	}
	if ipv4 != "" {
		v.SSHCommand = fmt.Sprintf("ssh %s@%s", user, ipv4)
	}
	return v
}

func (s *Server) createVM(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	var in struct {
		Plan      string  `json:"plan"`
		Template  string  `json:"template"`
		Hostname  string  `json:"hostname"`
		SSHKeyIDs []int64 `json:"ssh_key_ids"`
	}
	if !decode(w, r, &in) {
		return
	}
	ctx := r.Context()

	hostname := strings.ToLower(strings.TrimSpace(in.Hostname))
	if hostname == "" {
		tok, _, err := auth.NewToken()
		if err != nil {
			s.fail(w, r, err)
			return
		}
		hostname = "vm-" + strings.ToLower(strings.NewReplacer("-", "", "_", "").Replace(tok))[:6]
	}
	if !hostnameRe.MatchString(hostname) {
		writeErr(w, http.StatusBadRequest, "hostname must be 1-63 characters: lowercase letters, digits and hyphens, not starting or ending with a hyphen")
		return
	}
	plan, err := s.Store.Q.GetActivePlanBySlug(ctx, in.Plan)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusBadRequest, "unknown plan")
		return
	} else if err != nil {
		s.fail(w, r, err)
		return
	}
	tpl, err := s.Store.Q.GetActiveTemplateBySlug(ctx, in.Template)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusBadRequest, "unknown template")
		return
	} else if err != nil {
		s.fail(w, r, err)
		return
	}

	ids := uniqueIDs(in.SSHKeyIDs)
	if len(ids) == 0 {
		writeErr(w, http.StatusBadRequest, "choose at least one SSH key")
		return
	}
	keys, err := s.Store.Q.GetSSHKeysByIDs(ctx, db.GetSSHKeysByIDsParams{UserID: p.User.ID, Column2: ids})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(keys) != len(ids) {
		writeErr(w, http.StatusBadRequest, "unknown SSH key")
		return
	}

	// Wallet: read once before the transaction (it is a network call).
	user := p.User
	if !user.IspendCustomerID.Valid {
		s.linkISpend(ctx, &user)
		if !user.IspendCustomerID.Valid {
			writeErr(w, http.StatusServiceUnavailable, "wallet is not available yet, try again shortly")
			return
		}
	}
	bal, err := s.ISpend.Balances(ctx, user.IspendCustomerID.String)
	if err != nil {
		s.Log.Error("ispend balances", "user_id", user.ID, "err", err)
		writeErr(w, http.StatusServiceUnavailable, "wallet is not available right now, try again shortly")
		return
	}

	var created int64
	var apiErr *apiError
	err = s.Store.InTx(ctx, func(q *db.Queries, tx pgx.Tx) error {
		// Lock the user row so concurrent creates cannot both pass the limit check.
		if _, err := q.LockUser(ctx, user.ID); err != nil {
			return err
		}
		active, err := q.CountActiveVMs(ctx, user.ID)
		if err != nil {
			return err
		}
		if active >= int64(user.VmLimit) {
			apiErr = &apiError{http.StatusConflict, fmt.Sprintf("VM limit reached (%d)", user.VmLimit)}
			return errAbort
		}
		running, err := q.SumActiveHourly(ctx, user.ID)
		if err != nil {
			return err
		}
		need := (running + plan.PriceUusdtHourly) * minRunwayHours
		if bal.USDTMicro < need {
			apiErr = &apiError{http.StatusPaymentRequired,
				fmt.Sprintf("balance must cover %d hours of usage for all your VMs (%d micro-USDT needed, %d available)", minRunwayHours, need, bal.USDTMicro)}
			return errAbort
		}
		id, err := q.CreateVM(ctx, db.CreateVMParams{UserID: user.ID, Region: s.Cfg.Region, PlanID: plan.ID,
			TemplateID: tpl.ID, Hostname: hostname, AuthorizedKeys: strings.Join(keys, "\n")})
		if err != nil {
			return err
		}
		ip, err := q.ClaimFreeIP(ctx, s.Cfg.Region)
		if errors.Is(err, pgx.ErrNoRows) {
			apiErr = &apiError{http.StatusServiceUnavailable, "no capacity available right now, try again later"}
			return errAbort
		} else if err != nil {
			return err
		}
		if err := q.AssignIP(ctx, db.AssignIPParams{ID: ip.ID, VmID: pgtype.Int8{Int64: id, Valid: true}}); err != nil {
			return err
		}
		if err := q.SetVMIPv4(ctx, db.SetVMIPv4Params{ID: id, Ipv4ID: pgtype.Int8{Int64: ip.ID, Valid: true}}); err != nil {
			return err
		}
		// Enqueued in the same transaction: the VM and its job exist together or not at all.
		if err := jobs.EnqueueTx(ctx, tx, vm.JobProvision, vm.Payload{VMID: id}); err != nil {
			return err
		}
		created = id
		return nil
	})
	if errors.Is(err, errAbort) {
		writeErr(w, apiErr.status, apiErr.msg)
		return
	} else if err != nil {
		s.fail(w, r, err)
		return
	}
	s.respondVM(w, r, created, user.ID, http.StatusAccepted)
}

func (s *Server) listVMs(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Store.Q.ListUserVMs(r.Context(), principalFrom(r.Context()).User.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]vmJSON, 0, len(rows))
	for _, v := range rows {
		out = append(out, newVMJSON(v.ID, v.Hostname, v.Region, v.PlanSlug, v.TemplateSlug, v.State, v.Ipv4, v.Ipv6, v.CiUser, v.PriceUusdtHourly, v.CreatedAt))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getVM(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	s.respondVM(w, r, id, principalFrom(r.Context()).User.ID, http.StatusOK)
}

// respondVM always filters by owner, so a VM id from another account is a 404.
func (s *Server) respondVM(w http.ResponseWriter, r *http.Request, id, userID int64, status int) {
	v, err := s.Store.Q.GetUserVM(r.Context(), db.GetUserVMParams{ID: id, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	} else if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, status, newVMJSON(v.ID, v.Hostname, v.Region, v.PlanSlug, v.TemplateSlug, v.State, v.Ipv4, v.Ipv6, v.CiUser, v.PriceUusdtHourly, v.CreatedAt))
}

// powerAction validates the request against the VM's current state and queues
// the job. The worker performs it and records the new state.
func (s *Server) powerAction(action string, allowedFrom string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		v, err := s.Store.Q.GetUserVM(r.Context(), db.GetUserVMParams{ID: id, UserID: principalFrom(r.Context()).User.ID})
		if errors.Is(err, pgx.ErrNoRows) {
			writeErr(w, http.StatusNotFound, "not found")
			return
		} else if err != nil {
			s.fail(w, r, err)
			return
		}
		if v.State != allowedFrom {
			writeErr(w, http.StatusConflict, fmt.Sprintf("cannot %s a VM that is %s", action, v.State))
			return
		}
		if err := s.Jobs.Enqueue(r.Context(), vm.JobPower, vm.Payload{VMID: id, Action: action}); err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued", "action": action})
	}
}

func (s *Server) deleteVM(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	v, err := s.Store.Q.GetUserVM(r.Context(), db.GetUserVMParams{ID: id, UserID: principalFrom(r.Context()).User.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	} else if err != nil {
		s.fail(w, r, err)
		return
	}
	if v.State == "deleting" {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "already deleting"})
		return
	}
	if err := s.Jobs.Enqueue(r.Context(), vm.JobDelete, vm.Payload{VMID: id}); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
}

type apiError struct {
	status int
	msg    string
}

var errAbort = errors.New("abort")

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not found")
		return 0, false
	}
	return id, true
}

func uniqueIDs(in []int64) []int64 {
	seen := map[int64]bool{}
	out := make([]int64, 0, len(in))
	for _, id := range in {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}
