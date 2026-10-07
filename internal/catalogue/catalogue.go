// Package catalogue manages the plans and templates customers can pick. The operator CLI and the admin API
// share it, so both enforce the same rules and write the same audit records.
//
// Rules:
//   - A plan's specs (vCPU, RAM, disk) cannot be edited. A different size is a new plan with a new slug.
//   - A plan's hourly price can change. Metering reads the plan's price each hour, so the new price applies to
//     existing VMs from the next charged hour; the change is applied only when the caller confirms it, after
//     seeing how many VMs it touches.
//   - Disabling a plan or a template only stops new VMs (and resize targets). Existing VMs keep running and
//     keep being billed.
package catalogue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/israel-duff/xenos/internal/hosts"
	"github.com/israel-duff/xenos/internal/proxmox"
	"github.com/israel-duff/xenos/internal/store"
	"github.com/israel-duff/xenos/internal/store/db"
)

// ErrInvalid wraps every refusal that is the caller's to fix; its message is safe to show.
var ErrInvalid = errors.New("invalid")

// ErrNotFound is returned for an unknown slug.
var ErrNotFound = errors.New("not found")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// Actor is who made a change, for the audit log: an admin user, or an operator command with no user.
type Actor struct {
	AdminID int64  // 0 for the CLI
	Source  string // for example "xenosctl:root"; empty for a web admin
}

type Service struct {
	Store *store.Store
	// PVE, when set, is used to check that a template's VMID exists on the host before it is added.
	PVE proxmox.API
	// Hosts, when set, names the hosts a template can live on and is used to check the VMID on that host.
	Hosts *hosts.Set
}

// checkTemplate looks for a template's VMID on a host; with nothing to ask it passes.
func (s *Service) checkTemplate(ctx context.Context, host string, vmid int) error {
	var api proxmox.API
	switch {
	case s.Hosts != nil:
		h, ok := s.Hosts.Get(host)
		if !ok {
			return invalid("no host called %q (known: %s)", host, strings.Join(s.Hosts.Names(), ", "))
		}
		api = h.API
	case s.PVE != nil:
		api = s.PVE
	default:
		return nil
	}
	st, err := api.Status(ctx, vmid)
	if err != nil {
		return fmt.Errorf("could not check host %s for VMID %d (use the skip option to add it anyway): %w", host, vmid, err)
	}
	if !st.Exists {
		return invalid("VMID %d does not exist on host %s", vmid, host)
	}
	return nil
}

const (
	maxHourlyUUSDT = 100 * 1_000_000 // 100 USDT per hour is far above any sane plan: a typo guard
	hoursPerMonth  = 730
	maxMonthHours  = 744
)

var (
	slugRe   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)
	ciUserRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
)

// PlanInput describes a new plan. CapUUSDT 0 means "a full month" (hourly x 730), which is no cap in practice.
type PlanInput struct {
	Slug        string
	VCPU        int
	RAMMB       int
	DiskGB      int
	HourlyUUSDT int64
	CapUUSDT    int64
}

func validPrice(hourly, cap int64) (int64, error) {
	if hourly <= 0 || hourly > maxHourlyUUSDT {
		return 0, invalid("the hourly price must be between 1 micro-USDT and %d USDT", maxHourlyUUSDT/1_000_000)
	}
	if cap == 0 {
		cap = hourly * hoursPerMonth
	}
	if cap < hourly || cap > hourly*maxMonthHours {
		return 0, invalid("the monthly cap must be between one hour and %d hours of the hourly price", maxMonthHours)
	}
	return cap, nil
}

// AddPlan creates an active plan.
func (s *Service) AddPlan(ctx context.Context, in PlanInput, by Actor) (db.Plan, error) {
	switch {
	case !slugRe.MatchString(in.Slug):
		return db.Plan{}, invalid("the slug must be 1-32 characters: lowercase letters, digits and hyphens")
	case in.VCPU < 1 || in.VCPU > 64:
		return db.Plan{}, invalid("vCPU must be between 1 and 64")
	case in.RAMMB < 256 || in.RAMMB > 512*1024:
		return db.Plan{}, invalid("memory must be between 256 MB and 512 GB")
	case in.DiskGB < 1 || in.DiskGB > 4096:
		return db.Plan{}, invalid("disk must be between 1 and 4096 GB")
	}
	cap, err := validPrice(in.HourlyUUSDT, in.CapUUSDT)
	if err != nil {
		return db.Plan{}, err
	}
	if _, err := s.Store.Q.GetPlanAny(ctx, in.Slug); err == nil {
		return db.Plan{}, invalid("a plan called %q already exists", in.Slug)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return db.Plan{}, err
	}
	p, err := s.Store.Q.InsertPlan(ctx, db.InsertPlanParams{Slug: in.Slug, Vcpu: int32(in.VCPU), RamMb: int32(in.RAMMB),
		DiskGb: int32(in.DiskGB), PriceUusdtHourly: in.HourlyUUSDT, PriceUusdtMonthlyCap: cap})
	if err != nil {
		return db.Plan{}, err
	}
	s.audit(ctx, by, "plan.add", in.Slug, map[string]any{"vcpu": in.VCPU, "ram_mb": in.RAMMB, "disk_gb": in.DiskGB,
		"hourly_uusdt": in.HourlyUUSDT, "monthly_cap_uusdt": cap})
	return p, nil
}

// PriceImpact says what a price change would do.
type PriceImpact struct {
	Slug           string `json:"slug"`
	OldHourlyUUSDT int64  `json:"old_hourly_uusdt"`
	NewHourlyUUSDT int64  `json:"new_hourly_uusdt"`
	OldCapUUSDT    int64  `json:"old_monthly_cap_uusdt"`
	NewCapUUSDT    int64  `json:"new_monthly_cap_uusdt"`
	BillingVMs     int64  `json:"billing_vms"`
	// MonthlyDeltaUUSDT is the change in what those VMs would cost over a 730-hour month if all ran full time.
	MonthlyDeltaUUSDT int64 `json:"monthly_delta_uusdt"`
	Applied           bool  `json:"applied"`
}

// SetPrice changes a plan's hourly price (and monthly cap; 0 keeps the cap at the same number of hours as
// before). Without confirm it only reports the impact.
func (s *Service) SetPrice(ctx context.Context, slug string, hourly, cap int64, confirm bool, by Actor) (PriceImpact, error) {
	p, err := s.Store.Q.GetPlanAny(ctx, slug)
	if errors.Is(err, pgx.ErrNoRows) {
		return PriceImpact{}, ErrNotFound
	} else if err != nil {
		return PriceImpact{}, err
	}
	if cap == 0 { // keep the cap's proportion: hours of the hourly price
		cap = hourly * (p.PriceUusdtMonthlyCap / p.PriceUusdtHourly)
	}
	cap, err = validPrice(hourly, cap)
	if err != nil {
		return PriceImpact{}, err
	}
	n, err := s.Store.Q.CountBillingVMsOnPlan(ctx, p.ID)
	if err != nil {
		return PriceImpact{}, err
	}
	im := PriceImpact{Slug: slug, OldHourlyUUSDT: p.PriceUusdtHourly, NewHourlyUUSDT: hourly, OldCapUUSDT: p.PriceUusdtMonthlyCap,
		NewCapUUSDT: cap, BillingVMs: n, MonthlyDeltaUUSDT: (hourly - p.PriceUusdtHourly) * hoursPerMonth * n}
	if !confirm {
		return im, nil
	}
	if err := s.Store.Q.SetPlanPrice(ctx, db.SetPlanPriceParams{ID: p.ID, PriceUusdtHourly: hourly, PriceUusdtMonthlyCap: cap}); err != nil {
		return im, err
	}
	im.Applied = true
	s.audit(ctx, by, "plan.price", slug, im)
	return im, nil
}

// SetPlanActive enables or disables a plan for new VMs and resize targets.
func (s *Service) SetPlanActive(ctx context.Context, slug string, active bool, by Actor) error {
	p, err := s.Store.Q.GetPlanAny(ctx, slug)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if p.Active == active {
		return nil
	}
	if err := s.Store.Q.SetPlanActive(ctx, db.SetPlanActiveParams{ID: p.ID, Active: active}); err != nil {
		return err
	}
	s.audit(ctx, by, "plan.active", slug, map[string]any{"active": active})
	return nil
}

// TemplateInput describes a new template.
type TemplateInput struct {
	Slug      string
	Name      string
	VMID      int
	CIUser    string // default "root"
	SkipCheck bool   // do not look for the VMID on the host
	Host      string // the host the VMID is on; default "default"
}

// AddTemplate registers a template. When a Proxmox client is configured, the VMID must exist on the host.
func (s *Service) AddTemplate(ctx context.Context, in TemplateInput, by Actor) (db.Template, error) {
	if in.CIUser == "" {
		in.CIUser = "root"
	}
	switch {
	case !slugRe.MatchString(in.Slug):
		return db.Template{}, invalid("the slug must be 1-32 characters: lowercase letters, digits and hyphens")
	case len(in.Name) < 1 || len(in.Name) > 60:
		return db.Template{}, invalid("the name must be 1-60 characters")
	case in.VMID < 100 || in.VMID > 999999999:
		return db.Template{}, invalid("the Proxmox VMID must be between 100 and 999999999")
	case !ciUserRe.MatchString(in.CIUser):
		return db.Template{}, invalid("the login user is not a valid user name")
	}
	if _, err := s.Store.Q.GetTemplateAny(ctx, in.Slug); err == nil {
		return db.Template{}, invalid("a template called %q already exists", in.Slug)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return db.Template{}, err
	}
	if in.Host == "" {
		switch {
		case s.Hosts == nil:
			in.Host = "default"
		case len(s.Hosts.Names()) == 1:
			in.Host = s.Hosts.Names()[0]
		default:
			return db.Template{}, invalid("there is more than one host: say which one holds this VMID (%s)", strings.Join(s.Hosts.Names(), ", "))
		}
	}
	if !in.SkipCheck {
		if err := s.checkTemplate(ctx, in.Host, in.VMID); err != nil {
			return db.Template{}, err
		}
	}
	if used, err := s.Store.Q.HostTemplateVMIDInUse(ctx, db.HostTemplateVMIDInUseParams{Host: in.Host, ProxmoxTemplateID: int32(in.VMID)}); err != nil {
		return db.Template{}, err
	} else if used {
		return db.Template{}, invalid("VMID %d is already used by another template", in.VMID)
	}
	t, err := s.Store.Q.InsertTemplate(ctx, db.InsertTemplateParams{Slug: in.Slug, Name: in.Name, ProxmoxTemplateID: int32(in.VMID), CiUser: in.CIUser})
	if err != nil {
		if isUnique(err) {
			return db.Template{}, invalid("VMID %d is already used by another template", in.VMID)
		}
		return db.Template{}, err
	}
	if err := s.Store.Q.SetHostTemplate(ctx, db.SetHostTemplateParams{Host: in.Host, TemplateID: t.ID, ProxmoxTemplateID: int32(in.VMID)}); err != nil {
		return t, err
	}
	s.audit(ctx, by, "template.add", in.Slug, map[string]any{"name": in.Name, "vmid": in.VMID, "ci_user": in.CIUser, "host": in.Host})
	return t, nil
}

// SetTemplateHost says which VMID holds an existing template on a host (each host has its own copy).
func (s *Service) SetTemplateHost(ctx context.Context, slug, host string, vmid int, skipCheck bool, by Actor) error {
	t, err := s.Store.Q.GetTemplateAny(ctx, slug)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if vmid < 100 || vmid > 999999999 {
		return invalid("the Proxmox VMID must be between 100 and 999999999")
	}
	if used, err := s.Store.Q.HostTemplateVMIDInUse(ctx, db.HostTemplateVMIDInUseParams{Host: host, ProxmoxTemplateID: int32(vmid), TemplateID: t.ID}); err != nil {
		return err
	} else if used {
		return invalid("VMID %d is already used by another template on host %s", vmid, host)
	}
	if !skipCheck {
		if err := s.checkTemplate(ctx, host, vmid); err != nil {
			return err
		}
	}
	if err := s.Store.Q.SetHostTemplate(ctx, db.SetHostTemplateParams{Host: host, TemplateID: t.ID, ProxmoxTemplateID: int32(vmid)}); err != nil {
		return invalid("host %q is not known to the control plane yet: start the API or worker with it configured first", host)
	}
	s.audit(ctx, by, "template.host", slug, map[string]any{"host": host, "vmid": vmid})
	return nil
}

// SetTemplateActive enables or disables a template for new VMs and rebuilds.
func (s *Service) SetTemplateActive(ctx context.Context, slug string, active bool, by Actor) error {
	t, err := s.Store.Q.GetTemplateAny(ctx, slug)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if t.Active == active {
		return nil
	}
	if err := s.Store.Q.SetTemplateActive(ctx, db.SetTemplateActiveParams{ID: t.ID, Active: active}); err != nil {
		return err
	}
	s.audit(ctx, by, "template.active", slug, map[string]any{"active": active})
	return nil
}

func (s *Service) audit(ctx context.Context, by Actor, action, target string, detail any) {
	b, err := json.Marshal(detail)
	if err != nil {
		b = []byte("{}")
	}
	arg := db.InsertAuditParams{Source: pgtype.Text{String: by.Source, Valid: by.Source != ""}, Action: action, Target: target, Detail: b}
	if by.AdminID != 0 {
		arg.AdminID = pgtype.Int8{Int64: by.AdminID, Valid: true}
	}
	if err := s.Store.Q.InsertAudit(ctx, arg); err != nil {
		// A catalogue change that cannot be audited is still made; the failure is loud in the logs of the caller.
		fmt.Printf("catalogue: audit write failed for %s %s: %v\n", action, target, err)
	}
}

func isUnique(err error) bool {
	var pe interface{ SQLState() string }
	return errors.As(err, &pe) && pe.SQLState() == "23505"
}
