package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/user"
	"strconv"
	"strings"

	"github.com/israel-duff/xenos/internal/catalogue"
	"github.com/israel-duff/xenos/internal/config"
	"github.com/israel-duff/xenos/internal/hosts"
	"github.com/israel-duff/xenos/internal/store"
)

// catalogueCmd handles "plan ..." and "template ..." and reports whether the arguments were one of them.
func catalogueCmd(ctx context.Context, cfg config.Config, st *store.Store, args []string) (bool, error) {
	if len(args) == 0 || (args[0] != "plan" && args[0] != "template") {
		return false, nil
	}
	svc := &catalogue.Service{Store: st}
	if cfg.PVEURL != "" || cfg.HostsFile != "" { // check template VMIDs against the real hosts when they are configured
		hs, err := hosts.Load(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			return true, err
		}
		svc.Hosts = hs
	}
	by := catalogue.Actor{Source: "xenosctl:" + osUser()}
	if args[0] == "plan" {
		return true, planCmd(ctx, svc, st, by, args[1:])
	}
	return true, templateCmd(ctx, svc, st, by, args[1:])
}

func osUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "unknown"
}

const catalogueUsage = `usage:
  xenosctl plan list
  xenosctl plan add <slug> <vcpu> <ram_mb> <disk_gb> <hourly_usdt> [monthly_cap_usdt]
  xenosctl plan price <slug> <hourly_usdt> [monthly_cap_usdt] [--yes]
  xenosctl plan disable|enable <slug>
  xenosctl template list
  xenosctl template add <slug> <name> <proxmox_vmid> [--host <host>] [--ci-user <user>] [--skip-host-check]
  xenosctl template host <slug> <host> <proxmox_vmid> [--skip-host-check]   (the same template's VMID on another host)
  xenosctl template disable|enable <slug>`

func planCmd(ctx context.Context, svc *catalogue.Service, st *store.Store, by catalogue.Actor, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf(catalogueUsage)
	}
	switch args[0] {
	case "list":
		rows, err := st.Q.AdminListPlans(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("%-14s %5s %8s %7s %14s %14s %6s %s\n", "SLUG", "VCPU", "RAM_MB", "DISK_GB", "USDT/HOUR", "MONTHLY_CAP", "VMS", "STATE")
		for _, p := range rows {
			state := "active"
			if !p.Active {
				state = "disabled"
			}
			fmt.Printf("%-14s %5d %8d %7d %14s %14s %6d %s\n", p.Slug, p.Vcpu, p.RamMb, p.DiskGb, formatUSDT(p.PriceUusdtHourly),
				formatUSDT(p.PriceUusdtMonthlyCap), p.VmCount, state)
		}
		return nil
	case "add":
		if len(args) < 6 || len(args) > 7 {
			return fmt.Errorf(catalogueUsage)
		}
		in := catalogue.PlanInput{Slug: args[1]}
		var err error
		if in.VCPU, err = strconv.Atoi(args[2]); err != nil {
			return fmt.Errorf("vcpu must be a number")
		}
		if in.RAMMB, err = strconv.Atoi(args[3]); err != nil {
			return fmt.Errorf("ram_mb must be a number")
		}
		if in.DiskGB, err = strconv.Atoi(args[4]); err != nil {
			return fmt.Errorf("disk_gb must be a number")
		}
		if in.HourlyUUSDT, err = parseUSDT(args[5]); err != nil {
			return err
		}
		if len(args) == 7 {
			if in.CapUUSDT, err = parseUSDT(args[6]); err != nil {
				return err
			}
		}
		p, err := svc.AddPlan(ctx, in, by)
		if err != nil {
			return err
		}
		fmt.Printf("added plan %s: %d vCPU, %d MB, %d GB at %s USDT/hour (monthly cap %s)\n", p.Slug, p.Vcpu, p.RamMb, p.DiskGb,
			formatUSDT(p.PriceUusdtHourly), formatUSDT(p.PriceUusdtMonthlyCap))
		return nil
	case "price":
		rest, yes := takeFlag(args[1:], "--yes")
		if len(rest) < 2 || len(rest) > 3 {
			return fmt.Errorf(catalogueUsage)
		}
		hourly, err := parseUSDT(rest[1])
		if err != nil {
			return err
		}
		var cap int64
		if len(rest) == 3 {
			if cap, err = parseUSDT(rest[2]); err != nil {
				return err
			}
		}
		im, err := svc.SetPrice(ctx, rest[0], hourly, cap, yes, by)
		if err != nil {
			return err
		}
		fmt.Printf("plan %s: %s -> %s USDT/hour (monthly cap %s -> %s)\n", im.Slug, formatUSDT(im.OldHourlyUUSDT), formatUSDT(im.NewHourlyUUSDT),
			formatUSDT(im.OldCapUUSDT), formatUSDT(im.NewCapUUSDT))
		fmt.Printf("%d VM(s) are being billed on this plan; their next hourly charge uses the new price.\n", im.BillingVMs)
		fmt.Printf("If they all ran a full month the change is %s USDT in total.\n", formatSignedUSDT(im.MonthlyDeltaUUSDT))
		if im.Applied {
			fmt.Println("applied")
		} else {
			fmt.Println("NOT applied: run again with --yes to apply it (tell affected customers first)")
		}
		return nil
	case "host": // template host <slug> <host> <vmid>: this template's VMID on another host
		rest, skip := takeFlag(args[1:], "--skip-host-check")
		if len(rest) != 3 {
			return fmt.Errorf(catalogueUsage)
		}
		vmid, err := strconv.Atoi(rest[2])
		if err != nil {
			return fmt.Errorf("proxmox_vmid must be a number")
		}
		if err := svc.SetTemplateHost(ctx, rest[0], rest[1], vmid, skip, by); err != nil {
			return err
		}
		fmt.Println("ok")
		return nil
	case "disable", "enable":
		if len(args) != 2 {
			return fmt.Errorf(catalogueUsage)
		}
		if err := svc.SetPlanActive(ctx, args[1], args[0] == "enable", by); err != nil {
			return err
		}
		fmt.Println("ok")
		return nil
	}
	return fmt.Errorf(catalogueUsage)
}

func templateCmd(ctx context.Context, svc *catalogue.Service, st *store.Store, by catalogue.Actor, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf(catalogueUsage)
	}
	switch args[0] {
	case "list":
		rows, err := st.Q.AdminListTemplates(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("%-16s %-24s %8s %-12s %6s %s\n", "SLUG", "NAME", "VMID", "LOGIN USER", "VMS", "STATE")
		for _, t := range rows {
			state := "active"
			if !t.Active {
				state = "disabled"
			}
			fmt.Printf("%-16s %-24s %8d %-12s %6d %s\n", t.Slug, t.Name, t.ProxmoxTemplateID, t.CiUser, t.VmCount, state)
		}
		return nil
	case "add":
		rest, skip := takeFlag(args[1:], "--skip-host-check")
		rest, ciUser := takeValue(rest, "--ci-user")
		rest, host := takeValue(rest, "--host")
		if len(rest) != 3 {
			return fmt.Errorf(catalogueUsage)
		}
		vmid, err := strconv.Atoi(rest[2])
		if err != nil {
			return fmt.Errorf("proxmox_vmid must be a number")
		}
		t, err := svc.AddTemplate(ctx, catalogue.TemplateInput{Slug: rest[0], Name: rest[1], VMID: vmid, CIUser: ciUser, SkipCheck: skip, Host: host}, by)
		if err != nil {
			return err
		}
		fmt.Printf("added template %s (%s), VMID %d, login user %s\n", t.Slug, t.Name, t.ProxmoxTemplateID, t.CiUser)
		return nil
	case "disable", "enable":
		if len(args) != 2 {
			return fmt.Errorf(catalogueUsage)
		}
		if err := svc.SetTemplateActive(ctx, args[1], args[0] == "enable", by); err != nil {
			return err
		}
		fmt.Println("ok")
		return nil
	}
	return fmt.Errorf(catalogueUsage)
}

func takeFlag(args []string, flag string) ([]string, bool) {
	var out []string
	found := false
	for _, a := range args {
		if a == flag {
			found = true
			continue
		}
		out = append(out, a)
	}
	return out, found
}

func takeValue(args []string, flag string) ([]string, string) {
	var out []string
	val := ""
	for i := 0; i < len(args); i++ {
		if args[i] == flag && i+1 < len(args) {
			val = args[i+1]
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out, val
}

// parseUSDT reads a decimal amount such as "0.048" into micro-USDT, with at most six decimals.
func parseUSDT(s string) (int64, error) {
	whole, frac, _ := strings.Cut(strings.TrimSpace(s), ".")
	if whole == "" && frac == "" || len(frac) > 6 || strings.HasPrefix(whole, "-") {
		return 0, fmt.Errorf("%q is not an amount in USDT (up to 6 decimals)", s)
	}
	frac += strings.Repeat("0", 6-len(frac))
	w, err1 := strconv.ParseInt(orZero(whole), 10, 64)
	f, err2 := strconv.ParseInt(frac, 10, 64)
	if err1 != nil || err2 != nil || w > 1_000_000_000 {
		return 0, fmt.Errorf("%q is not an amount in USDT (up to 6 decimals)", s)
	}
	return w*1_000_000 + f, nil
}

func orZero(s string) string {
	if s == "" {
		return "0"
	}
	return s
}

func formatUSDT(micro int64) string {
	return fmt.Sprintf("%d.%06d", micro/1_000_000, micro%1_000_000)
}

func formatSignedUSDT(micro int64) string {
	if micro < 0 {
		return "-" + formatUSDT(-micro)
	}
	return "+" + formatUSDT(micro)
}
