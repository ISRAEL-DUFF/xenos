// Command xenosctl is the operator CLI for tasks that have no admin UI yet.
//
//	xenosctl ip add <first>-<last> <gateway> [region]   load addresses into the IP pool
//	xenosctl ip list                                   show the pool and what uses each address
//	xenosctl admin grant <email>                       make a user an admin
//	xenosctl user limit <email> <n>                    set a user's VM limit
//	xenosctl user ban|unban <email>                    ban revokes sessions and suspends their VMs
//	xenosctl flagged                                   VMs flagged for sustained high CPU (possible mining)
//	xenosctl flag clear <vm-id>                        dismiss a flag after review
//	xenosctl port25 allow|block <vm-id>                exempt a reviewed VM from the outbound SMTP block
//	xenosctl firewall nft [bridge]                     print the nftables ruleset to load on the Proxmox host
//	xenosctl preflight [--send-test <email>]           check config, database, iswallet, Proxmox, email and the worker before launch (exit 1 on any FAIL)
//	xenosctl ispend subscribe <https-url>              register our webhook endpoint with iswallet (prints the signing secret ONCE)
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"strconv"
	"strings"

	"github.com/israel-duff/xenos/internal/accounts"
	"github.com/israel-duff/xenos/internal/billing"
	"github.com/israel-duff/xenos/internal/config"
	"github.com/israel-duff/xenos/internal/firewall"
	"github.com/israel-duff/xenos/internal/jobs"
	"github.com/israel-duff/xenos/internal/mail"
	"github.com/israel-duff/xenos/internal/preflight"
	"github.com/israel-duff/xenos/internal/store"
	"github.com/israel-duff/xenos/internal/store/db"
	"github.com/israel-duff/xenos/internal/worker"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: xenosctl ip add|list | admin grant <email> | user limit|ban|unban ... | flagged | flag clear <id> | port25 allow|block <id> | firewall nft")
	}
	if args[0] == "preflight" {
		return preflightCmd(context.Background(), args[1:])
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	switch {
	case len(args) >= 4 && args[0] == "ip" && args[1] == "add":
		region := cfg.Region
		if len(args) > 4 {
			region = args[4]
		}
		return ipAdd(ctx, st, args[2], args[3], region)
	case len(args) == 2 && args[0] == "ip" && args[1] == "list":
		return ipList(ctx, st)
	case len(args) == 3 && args[0] == "admin" && args[1] == "grant":
		return setAdmin(ctx, st, args[2])
	case len(args) == 4 && args[0] == "user" && args[1] == "limit":
		n, err := strconv.Atoi(args[3])
		if err != nil || n < 0 {
			return fmt.Errorf("limit must be a non-negative integer")
		}
		return setLimit(ctx, st, args[2], n)
	case len(args) == 3 && args[0] == "user" && (args[1] == "ban" || args[1] == "unban"):
		return setBan(ctx, st, args[2], args[1] == "ban")
	case len(args) == 1 && args[0] == "flagged":
		return listFlagged(ctx, st)
	case len(args) == 3 && args[0] == "flag" && args[1] == "clear":
		return withVMID(args[2], func(id int64) error { return st.Q.ClearVMFlag(ctx, id) })
	case len(args) == 3 && args[0] == "port25" && (args[1] == "allow" || args[1] == "block"):
		return withVMID(args[2], func(id int64) error {
			n, err := st.Q.SetPort25(ctx, db.SetPort25Params{ID: id, Port25Unblocked: args[1] == "allow"})
			if err == nil && n == 0 {
				err = fmt.Errorf("no such vm")
			}
			if err == nil {
				fmt.Println("ok; regenerate and load the firewall rules: xenosctl firewall nft")
			}
			return err
		})
	case len(args) == 3 && args[0] == "ispend" && args[1] == "subscribe":
		return subscribeWebhook(ctx, cfg, args[2])
	case len(args) >= 2 && args[0] == "firewall" && args[1] == "nft":
		bridge := cfg.PVEBridge
		if len(args) > 2 {
			bridge = args[2]
		}
		return printNft(ctx, st, bridge)
	}
	return fmt.Errorf("unknown command %q", strings.Join(args, " "))
}

func withVMID(arg string, fn func(int64) error) error {
	id, err := strconv.ParseInt(arg, 10, 64)
	if err != nil {
		return fmt.Errorf("vm id must be a number")
	}
	return fn(id)
}

func setBan(ctx context.Context, st *store.Store, email string, ban bool) error {
	email = strings.ToLower(email)
	var uid int64
	if err := st.Pool.QueryRow(ctx, `SELECT id FROM users WHERE email = $1`, email).Scan(&uid); err != nil {
		return fmt.Errorf("no user with email %s", email)
	}
	if !ban {
		if _, err := st.Q.SetUserStatusByID(ctx, db.SetUserStatusByIDParams{ID: uid, Status: "active"}); err != nil {
			return err
		}
		fmt.Println("ok")
		return nil
	}
	n, err := accounts.Ban(ctx, st, jobs.New(st.Pool), uid)
	if err != nil {
		return err
	}
	fmt.Printf("banned; sessions revoked; suspending %d VM(s). Deleting them is a separate decision.\n", n)
	return nil
}

func listFlagged(ctx context.Context, st *store.Store) error {
	rows, err := st.Q.ListFlaggedVMs(ctx)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Println("no flagged VMs")
	}
	for _, r := range rows {
		fmt.Printf("vm %-6d %-20s %-10s %s  %s\n    %s\n", r.ID, r.Hostname, r.State, r.FlaggedAt.Time.Format("2006-01-02 15:04"), r.Email, r.FlagReason)
	}
	return nil
}

func printNft(ctx context.Context, st *store.Store, bridge string) error {
	rows, err := st.Q.ListPort25Unblocked(ctx)
	if err != nil {
		return err
	}
	var v4, v6 []string
	for _, r := range rows {
		v4 = append(v4, r.Ipv4)
		v6 = append(v6, r.Ipv6)
	}
	out, err := firewall.Render(bridge, v4, v6)
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}

func ipAdd(ctx context.Context, st *store.Store, rng, gateway, region string) error {
	first, last, ok := strings.Cut(rng, "-")
	if !ok {
		last = first
	}
	a, err := netip.ParseAddr(first)
	if err != nil {
		return fmt.Errorf("first address: %w", err)
	}
	b, err := netip.ParseAddr(last)
	if err != nil {
		return fmt.Errorf("last address: %w", err)
	}
	gw, err := netip.ParseAddr(gateway)
	if err != nil {
		return fmt.Errorf("gateway: %w", err)
	}
	if !a.Is4() || !b.Is4() || !gw.Is4() || b.Less(a) {
		return fmt.Errorf("need an IPv4 range first<=last and an IPv4 gateway")
	}
	n := 0
	for ip := a; ; ip = ip.Next() {
		if ip == gw {
			return fmt.Errorf("gateway %s is inside the range", gw)
		}
		tag, err := st.Pool.Exec(ctx,
			`INSERT INTO ip_addresses (address, gateway, region) VALUES ($1::inet, $2::inet, $3) ON CONFLICT (address) DO NOTHING`,
			ip.String(), gw.String(), region)
		if err != nil {
			return err
		}
		n += int(tag.RowsAffected())
		if ip == b {
			break
		}
	}
	fmt.Printf("added %d address(es) to %s\n", n, region)
	return nil
}

func ipList(ctx context.Context, st *store.Store) error {
	rows, err := st.Pool.Query(ctx,
		`SELECT host(address), host(gateway), region, COALESCE(vm_id::text, '-') FROM ip_addresses ORDER BY address`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var a, g, r, v string
		if err := rows.Scan(&a, &g, &r, &v); err != nil {
			return err
		}
		fmt.Printf("%-16s gw %-16s %-10s vm %s\n", a, g, r, v)
	}
	return rows.Err()
}

func setAdmin(ctx context.Context, st *store.Store, email string) error {
	tag, err := st.Pool.Exec(ctx, `UPDATE users SET is_admin = TRUE WHERE email = $1`, strings.ToLower(email))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("no user with email %s", email)
	}
	fmt.Println("ok")
	return nil
}

func setLimit(ctx context.Context, st *store.Store, email string, n int) error {
	tag, err := st.Pool.Exec(ctx, `UPDATE users SET vm_limit = $2 WHERE email = $1`, strings.ToLower(email), n)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("no user with email %s", email)
	}
	fmt.Println("ok")
	return nil
}

// subscribeWebhook registers the endpoint URL with iswallet. The signing secret is shown once by
// iswallet, so print it immediately; it belongs in XENOS_ISPEND_WEBHOOK_SECRET.
func subscribeWebhook(ctx context.Context, cfg config.Config, endpoint string) error {
	if !strings.HasPrefix(endpoint, "https://") {
		return fmt.Errorf("the webhook URL must be https://…")
	}
	if cfg.ISpendURL == "" {
		return fmt.Errorf("XENOS_ISPEND_URL is not set (this command talks to the real iswallet API)")
	}
	c, err := billing.NewISWallet(billing.ISWalletConfig{BaseURL: cfg.ISpendURL, APIKey: cfg.ISpendAPIKey,
		MerchantWallet: cfg.ISpendMerchantWallet, OwnerPrefix: cfg.ISpendOwnerPrefix, USDTDecimals: cfg.ISpendUSDTDecimals})
	if err != nil {
		return err
	}
	// One subscription per environment: the key is stable, so re-running returns the same subscription.
	id, secret, err := c.SubscribeWebhook(ctx, "xenos:sub:primary", endpoint, "Xenos inbox")
	if err != nil {
		return err
	}
	fmt.Printf("subscription: %s\n", id)
	if secret == "" {
		fmt.Println("no signing secret was returned: it is shown only when the subscription is first created")
		return nil
	}
	fmt.Printf("signing secret (shown once, store it now):\n  XENOS_ISPEND_WEBHOOK_SECRET=%s\n", secret)
	return nil
}

// preflightCmd runs every deployment check and prints one line each. A configuration that would not even
// start the services is reported as a failed check rather than an abort, so the rest still runs.
func preflightCmd(ctx context.Context, args []string) error {
	var sendTo string
	if len(args) == 2 && args[0] == "--send-test" {
		sendTo = args[1]
	} else if len(args) != 0 {
		return fmt.Errorf("usage: xenosctl preflight [--send-test <email>]")
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg, err := config.Load()
	if err != nil {
		// Keep going with what loaded: the problem is the first line of the report.
		fmt.Printf("FAIL  config                 %v\n\n", err)
		cfg, _ = config.LoadUnchecked()
	}
	d := preflight.Deps{Cfg: cfg}
	if cfg.DatabaseURL != "" {
		if st, err := store.Open(ctx, cfg.DatabaseURL); err == nil {
			defer st.Close()
			d.Store = st
		}
	}
	if is, err := billing.FromConfig(cfg, log); err == nil {
		d.ISpend = is
	}
	d.PVE = worker.NewProxmox(cfg, log)
	d.Mailer, _ = mail.New(cfg, log)
	if sendTo != "" {
		if d.Mailer == nil {
			return fmt.Errorf("no mailer is configured")
		}
		if err := d.Mailer.Send(ctx, sendTo, "Xenos preflight test", "If you can read this, Xenos can send email."); err != nil {
			fmt.Printf("FAIL  email test send        %v\n", err)
		} else {
			fmt.Printf("ok    email test send        delivered to the provider for %s: check that it arrives\n", sendTo)
		}
	}

	results := preflight.Run(ctx, d)
	group := ""
	for _, r := range results {
		if r.Group != group {
			group = r.Group
			fmt.Printf("\n%s\n", strings.ToUpper(group))
		}
		fmt.Printf("  %-5s %-26s %s\n", r.Status, r.Name, r.Detail)
	}
	ok, warn, fail := preflight.Summary(results)
	fmt.Printf("\n%d ok, %d warnings, %d failures\n", ok, warn, fail)
	if fail > 0 {
		os.Exit(1)
	}
	return nil
}
