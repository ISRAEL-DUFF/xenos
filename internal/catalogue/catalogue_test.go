package catalogue

import (
	"context"
	"errors"
	"testing"

	"github.com/israel-duff/xenos/internal/proxmox"
	"github.com/israel-duff/xenos/internal/testutil"
)

func newSvc(t *testing.T) (*Service, *proxmox.Fake) {
	st := testutil.DB(t)
	pve := proxmox.NewFake()
	return &Service{Store: st, PVE: pve}, pve
}

var cli = Actor{Source: "xenosctl:test"}

func TestAddPlanValidation(t *testing.T) {
	s, _ := newSvc(t)
	ctx := context.Background()
	ok := PlanInput{Slug: "d-small", VCPU: 2, RAMMB: 4096, DiskGB: 40, HourlyUUSDT: 48_000}
	for name, mut := range map[string]func(*PlanInput){
		"bad slug":       func(p *PlanInput) { p.Slug = "D Small" },
		"zero cpu":       func(p *PlanInput) { p.VCPU = 0 },
		"tiny memory":    func(p *PlanInput) { p.RAMMB = 64 },
		"huge disk":      func(p *PlanInput) { p.DiskGB = 100000 },
		"free":           func(p *PlanInput) { p.HourlyUUSDT = 0 },
		"absurd price":   func(p *PlanInput) { p.HourlyUUSDT = 1_000_000_000 },
		"cap under hour": func(p *PlanInput) { p.CapUUSDT = 100 },
		"cap over month": func(p *PlanInput) { p.CapUUSDT = p.HourlyUUSDT * 800 },
		"duplicate slug": func(p *PlanInput) { p.Slug = "nano" },
	} {
		in := ok
		mut(&in)
		if _, err := s.AddPlan(ctx, in, cli); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: got %v, want an invalid-input error", name, err)
		}
	}
	p, err := s.AddPlan(ctx, ok, cli)
	if err != nil {
		t.Fatal(err)
	}
	if p.PriceUusdtMonthlyCap != 48_000*730 || !p.Active {
		t.Fatalf("plan: %+v (the default cap is a full month)", p)
	}
	// The new plan is offered to customers.
	if _, err := s.Store.Q.GetActivePlanBySlug(ctx, "d-small"); err != nil {
		t.Fatal(err)
	}
}

func TestPriceChangeShowsImpactAndNeedsConfirmation(t *testing.T) {
	s, _ := newSvc(t)
	ctx := context.Background()
	// Two VMs bill on nano, one has ended billing.
	for _, q := range []string{
		`INSERT INTO users (id, email, password_hash) VALUES (1, 'a@x.co', 'x')`,
		`INSERT INTO vms (user_id, region, plan_id, template_id, proxmox_vmid, hostname, state, billing_from)
		 SELECT 1, 'r', p.id, t.id, 100 + g, 'h' || g, 'running', now() FROM plans p, templates t, generate_series(1,2) g WHERE p.slug='nano' AND t.slug='ubuntu-24.04'`,
		`INSERT INTO vms (user_id, region, plan_id, template_id, proxmox_vmid, hostname, state, billing_from, billing_until)
		 SELECT 1, 'r', p.id, t.id, 110, 'old', 'deleted', now(), now() FROM plans p, templates t WHERE p.slug='nano' AND t.slug='ubuntu-24.04'`,
	} {
		if _, err := s.Store.Pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	old, _ := s.Store.Q.GetPlanAny(ctx, "nano")

	im, err := s.SetPrice(ctx, "nano", old.PriceUusdtHourly+1000, 0, false, cli)
	if err != nil {
		t.Fatal(err)
	}
	if im.Applied || im.BillingVMs != 2 || im.MonthlyDeltaUUSDT != 1000*730*2 {
		t.Fatalf("impact: %+v", im)
	}
	if cur, _ := s.Store.Q.GetPlanAny(ctx, "nano"); cur.PriceUusdtHourly != old.PriceUusdtHourly {
		t.Fatal("without confirmation nothing may change")
	}
	im, err = s.SetPrice(ctx, "nano", old.PriceUusdtHourly+1000, 0, true, cli)
	if err != nil || !im.Applied {
		t.Fatalf("confirmed: %+v %v", im, err)
	}
	cur, _ := s.Store.Q.GetPlanAny(ctx, "nano")
	if cur.PriceUusdtHourly != old.PriceUusdtHourly+1000 {
		t.Fatalf("price %d", cur.PriceUusdtHourly)
	}
	if cur.PriceUusdtMonthlyCap != cur.PriceUusdtHourly*730 { // the cap kept its 730-hour proportion
		t.Fatalf("cap %d", cur.PriceUusdtMonthlyCap)
	}
	if _, err := s.SetPrice(ctx, "nano", 0, 0, true, cli); !errors.Is(err, ErrInvalid) {
		t.Errorf("a zero price: %v", err)
	}
	if _, err := s.SetPrice(ctx, "ghost", 100, 0, true, cli); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown plan: %v", err)
	}
	var n int
	if err := s.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM admin_audit WHERE source='xenosctl:test' AND action='plan.price'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("audit rows: %d %v", n, err)
	}
}

func TestDisabledPlanIsHiddenButExistingVMsStillBill(t *testing.T) {
	s, _ := newSvc(t)
	ctx := context.Background()
	if err := s.SetPlanActive(ctx, "small", false, cli); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.Q.GetActivePlanBySlug(ctx, "small"); err == nil {
		t.Fatal("a disabled plan must not be offered")
	}
	plans, _ := s.Store.Q.ListActivePlans(ctx)
	for _, p := range plans {
		if p.Slug == "small" {
			t.Fatal("a disabled plan must not be listed to customers")
		}
	}
	all, _ := s.Store.Q.AdminListPlans(ctx)
	found := false
	for _, p := range all {
		if p.Slug == "small" && !p.Active {
			found = true
		}
	}
	if !found {
		t.Fatal("the operator still sees it")
	}
	if err := s.SetPlanActive(ctx, "small", true, cli); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPlanActive(ctx, "ghost", true, cli); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown plan: %v", err)
	}
}

func TestTemplates(t *testing.T) {
	s, pve := newSvc(t)
	ctx := context.Background()
	in := TemplateInput{Slug: "alma-9", Name: "AlmaLinux 9", VMID: 9002, CIUser: "almalinux"}

	if _, err := s.AddTemplate(ctx, in, cli); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a VMID missing on the host: %v", err)
	}
	pve.VMs[9002] = &proxmox.FakeVM{ID: 9002}
	tpl, err := s.AddTemplate(ctx, in, cli)
	if err != nil || tpl.CiUser != "almalinux" || !tpl.Active {
		t.Fatalf("add: %+v %v", tpl, err)
	}
	for name, bad := range map[string]TemplateInput{
		"duplicate slug": {Slug: "alma-9", Name: "x", VMID: 9003, SkipCheck: true},
		"duplicate vmid": {Slug: "other", Name: "x", VMID: 9002, SkipCheck: true},
		"bad slug":       {Slug: "Alma 9", Name: "x", VMID: 9004, SkipCheck: true},
		"bad user":       {Slug: "u", Name: "x", VMID: 9005, CIUser: "root; rm -rf /", SkipCheck: true},
		"vmid below 100": {Slug: "low", Name: "x", VMID: 5, SkipCheck: true},
	} {
		if _, err := s.AddTemplate(ctx, bad, cli); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := s.AddTemplate(ctx, TemplateInput{Slug: "skipped", Name: "Skipped", VMID: 9006, SkipCheck: true}, cli); err != nil {
		t.Errorf("skipping the host check: %v", err)
	}
	if err := s.SetTemplateActive(ctx, "alma-9", false, cli); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.Q.GetActiveTemplateBySlug(ctx, "alma-9"); err == nil {
		t.Fatal("a disabled template must not be offered")
	}
}
