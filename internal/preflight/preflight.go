// Package preflight checks a deployment before launch: configuration, database, iswallet, Proxmox, email and
// the worker. Each check reports OK, WARN (probably wrong, not fatal) or FAIL (will not work), so one command
// replaces working through the manual checklist by hand.
package preflight

import (
	"context"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/israel-duff/xenos/internal/billing"
	"github.com/israel-duff/xenos/internal/config"
	"github.com/israel-duff/xenos/internal/hosts"
	"github.com/israel-duff/xenos/internal/mail"
	"github.com/israel-duff/xenos/internal/proxmox"
	"github.com/israel-duff/xenos/internal/store"
)

type Status string

const (
	OK   Status = "ok"
	Warn Status = "warn"
	Fail Status = "FAIL"
)

type Result struct {
	Group  string
	Name   string
	Status Status
	Detail string
}

// Deps are the live services to check. A nil service is reported as a failure, not skipped.
type Deps struct {
	Cfg    config.Config
	Store  *store.Store
	ISpend billing.ISpend
	PVE    proxmox.API
	// Hosts, when set, is checked host by host; without it PVE is the one host.
	Hosts  *hosts.Set
	Mailer mail.Mailer
	Now    func() time.Time

	// Thresholds.
	MinFreeIPs int // default 5
	// AllowFakes lets tests run the checks against the in-memory fakes; a real run reports them as failures.
	AllowFakes bool
}

type verifier interface{ Verify(context.Context) error }

// Failed reports whether any check failed.
func Failed(rs []Result) bool {
	for _, r := range rs {
		if r.Status == Fail {
			return true
		}
	}
	return false
}

// Run executes every check. Checks never panic the run: an error becomes a FAIL line.
func Run(ctx context.Context, d Deps) []Result {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.MinFreeIPs == 0 {
		d.MinFreeIPs = 5
	}
	var out []Result
	for _, g := range []func(context.Context, Deps) []Result{configChecks, databaseChecks, iswalletChecks, proxmoxChecks, mailChecks, workerChecks} {
		out = append(out, g(ctx, d)...)
	}
	return out
}

func res(group, name string, s Status, format string, a ...any) Result {
	return Result{Group: group, Name: name, Status: s, Detail: fmt.Sprintf(format, a...)}
}

func step(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 20*time.Second)
}

// ---- configuration ----

func configChecks(_ context.Context, d Deps) []Result {
	const g = "config"
	c := d.Cfg
	var r []Result
	add := func(name string, ok bool, bad Status, good, problem string) {
		if ok {
			r = append(r, res(g, name, OK, "%s", good))
		} else {
			r = append(r, res(g, name, bad, "%s", problem))
		}
	}
	add("environment", c.Env == "production", Warn, "production", "XENOS_ENV is "+c.Env+": the production safety checks are off")
	u, err := url.Parse(c.PublicURL)
	add("public URL", err == nil && u.Scheme == "https" && u.Host != "" && !strings.Contains(u.Host, "localhost"), Fail,
		c.PublicURL, "XENOS_PUBLIC_URL must be the public https URL (emailed links and the webhook use it), got "+c.PublicURL)
	add("secure cookies", c.CookieSecure, Fail, "on", "XENOS_COOKIE_SECURE is false: sessions would travel over plain http")
	add("listen address", loopback(c.HTTPAddr), Warn, c.HTTPAddr, c.HTTPAddr+" is reachable from other hosts: bind 127.0.0.1 behind Caddy and firewall the port")
	add("reverse proxy trust", c.TrustProxy, Warn, "X-Forwarded-For trusted from the proxy", "XENOS_TRUST_PROXY is false: behind Caddy every user shares one IP, so rate limits apply to everyone together")
	add("iswallet URL and key", c.ISpendURL != "" && c.ISpendAPIKey != "", Fail, "set", "XENOS_ISPEND_URL / XENOS_ISPEND_API_KEY missing: the in-memory fake would run and hand out credit")
	add("webhook secret", c.ISpendWebhookSecret != "", Fail, "set", "XENOS_ISPEND_WEBHOOK_SECRET is empty: every iswallet webhook would be rejected (run `xenosctl ispend subscribe`)")
	add("owner prefix", c.ISpendOwnerPrefix != "" && !strings.Contains(c.ISpendOwnerPrefix, "sbx") && c.ISpendOwnerPrefix != "xenos", Warn, c.ISpendOwnerPrefix,
		"XENOS_ISPEND_OWNER_PREFIX is empty, the default or a sandbox value: use a distinct production prefix (owner_ref is unique across all iswallet tenants)")
	add("Proxmox host", c.PVEURL != "" && c.PVETokenID != "" && c.PVETokenSecret != "", Fail, c.PVEURL, "XENOS_PVE_URL / token missing: no real VMs can be created")
	add("Proxmox TLS", !c.PVEInsecureTLS, Warn, "certificate verified", "XENOS_PVE_INSECURE_TLS is true: install a real certificate on the host")
	add("KVM", !c.PVEDisableKVM, Fail, "hardware virtualisation on", "XENOS_PVE_DISABLE_KVM is true: guests run under slow software emulation (test hosts only)")
	add("IPv6", c.IPv6Prefix != "" && c.IPv6Gateway != "", Warn, c.IPv6Prefix, "no IPv6 prefix/gateway configured: VMs will be IPv4 only")
	add("operator alerts", (c.TelegramBotToken != "" && c.TelegramChatID != "") || c.AlertEmail != "", Warn, "a channel is configured", "no Telegram or alert email configured: failures would only reach the log")
	add("spread of hourly charges", c.MeterSpreadMinutes > 0, Warn, fmt.Sprintf("%d minutes", c.MeterSpreadMinutes), "XENOS_METER_SPREAD_MINUTES is 0: every hourly charge fires at once and may exceed iswallet's 100 calls/minute")
	if c.RunWorker {
		r = append(r, res(g, "worker process", Warn, "XENOS_RUN_WORKER is true: in production run xenos-worker as its own single instance"))
	}
	return r
}

func loopback(addr string) bool {
	host := addr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host = addr[:i]
	}
	if host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(strings.Trim(host, "[]"))
	return err == nil && ip.IsLoopback()
}

// ---- database ----

func databaseChecks(ctx context.Context, d Deps) []Result {
	const g = "database"
	if d.Store == nil {
		return []Result{res(g, "connection", Fail, "no database connection")}
	}
	ctx, cancel := step(ctx)
	defer cancel()
	var r []Result
	if err := d.Store.Pool.Ping(ctx); err != nil {
		return []Result{res(g, "connection", Fail, "%v", err)}
	}
	r = append(r, res(g, "connection", OK, "reachable"))
	if strings.Contains(d.Cfg.DatabaseURL, "sslmode=disable") && !strings.Contains(d.Cfg.DatabaseURL, "localhost") && !strings.Contains(d.Cfg.DatabaseURL, "127.0.0.1") {
		r = append(r, res(g, "TLS", Warn, "a remote database is used with sslmode=disable"))
	}

	rows, err := d.Store.Pool.Query(ctx, `SELECT slug, price_uusdt_hourly, price_uusdt_monthly_cap FROM plans WHERE active ORDER BY price_uusdt_hourly`)
	if err != nil {
		return append(r, res(g, "plans", Fail, "%v", err))
	}
	n := 0
	var bad []string
	var prices []string
	for rows.Next() {
		var slug string
		var hourly, cap int64
		if err := rows.Scan(&slug, &hourly, &cap); err != nil {
			rows.Close()
			return append(r, res(g, "plans", Fail, "%v", err))
		}
		n++
		prices = append(prices, fmt.Sprintf("%s %.6f USDT/h", slug, float64(hourly)/1e6))
		if hourly <= 0 || cap <= 0 || cap > hourly*744 {
			bad = append(bad, slug)
		}
	}
	rows.Close()
	switch {
	case n == 0:
		r = append(r, res(g, "plans", Fail, "no active plan: nobody can create a VM"))
	case len(bad) > 0:
		r = append(r, res(g, "plan prices", Fail, "price or monthly cap is zero or above a full month for: %s", strings.Join(bad, ", ")))
	default:
		r = append(r, res(g, "plan prices", Warn, "confirm these are your real prices (seed values are placeholders): %s", strings.Join(prices, "; ")))
	}

	trows, err := d.Store.Pool.Query(ctx, `SELECT slug, proxmox_template_id FROM templates WHERE active ORDER BY id`)
	if err != nil {
		return append(r, res(g, "templates", Fail, "%v", err))
	}
	var tpl []tplRow
	for trows.Next() {
		var t tplRow
		if err := trows.Scan(&t.slug, &t.vmid); err != nil {
			trows.Close()
			return append(r, res(g, "templates", Fail, "%v", err))
		}
		tpl = append(tpl, t)
	}
	trows.Close()
	if len(tpl) == 0 {
		r = append(r, res(g, "templates", Fail, "no active template: nobody can create a VM"))
	} else {
		r = append(r, res(g, "templates", OK, "%d active", len(tpl)))
	}

	var free, total int64
	if err := d.Store.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE vm_id IS NULL), count(*) FROM ip_addresses`).Scan(&free, &total); err != nil {
		return append(r, res(g, "IP pool", Fail, "%v", err))
	}
	switch {
	case total == 0:
		r = append(r, res(g, "IP pool", Fail, "no addresses loaded (xenosctl ip add)"))
	case int(free) < d.MinFreeIPs:
		r = append(r, res(g, "IP pool", Warn, "only %d of %d addresses free", free, total))
	default:
		r = append(r, res(g, "IP pool", OK, "%d of %d free", free, total))
	}

	var admins int
	if err := d.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE is_admin AND status = 'active'`).Scan(&admins); err == nil && admins == 0 {
		r = append(r, res(g, "admin account", Warn, "no admin exists yet (xenosctl admin grant <email>)"))
	}
	return r
}

type tplRow struct {
	slug string
	vmid int
}

// ---- iswallet ----

func iswalletChecks(ctx context.Context, d Deps) []Result {
	const g = "iswallet"
	if d.ISpend == nil {
		return []Result{res(g, "client", Fail, "not configured")}
	}
	if _, fake := d.ISpend.(*billing.Fake); fake && !d.AllowFakes {
		return []Result{res(g, "client", Fail, "the in-memory fake is in use, not iswallet")}
	}
	ctx, cancel := step(ctx)
	defer cancel()
	var r []Result
	if k, err := d.ISpend.Rate(ctx); err != nil {
		r = append(r, res(g, "FX rate", Fail, "%v", err))
	} else if k <= 0 {
		r = append(r, res(g, "FX rate", Fail, "rate is %d", k))
	} else {
		r = append(r, res(g, "FX rate", OK, "₦%d.%02d per USDT", k/100, k%100))
	}
	bal, err := d.ISpend.MerchantBalance(ctx)
	switch {
	case err != nil:
		r = append(r, res(g, "operating wallet", Fail, "%v", err))
	case bal == 0:
		r = append(r, res(g, "operating wallet", Warn, "reachable, holds 0 USDT: admin credits cannot be paid until it is funded"))
	default:
		r = append(r, res(g, "operating wallet", OK, "reachable, holds %.6f USDT", float64(bal)/1e6))
	}
	return r
}

// ---- Proxmox ----

func proxmoxChecks(ctx context.Context, d Deps) []Result {
	if d.Hosts == nil {
		return hostChecks(ctx, d, "proxmox", "default", d.PVE, d.Cfg.PVEStorage)
	}
	var r []Result
	for _, h := range d.Hosts.All() {
		g := "proxmox"
		if len(d.Hosts.Names()) > 1 {
			g = "proxmox " + h.Name
		}
		r = append(r, hostChecks(ctx, d, g, h.Name, h.API, h.Storage)...)
	}
	return r
}

// hostChecks looks at one Proxmox host: reachable, storage, firewall, and every active template present on it.
func hostChecks(ctx context.Context, d Deps, g, host string, api proxmox.API, storage string) []Result {
	if api == nil {
		return []Result{res(g, "connection", Fail, "not configured")}
	}
	if _, fake := api.(*proxmox.Fake); fake && !d.AllowFakes {
		return []Result{res(g, "connection", Fail, "the in-memory fake is in use, not a Proxmox host")}
	}
	ctx, cancel := step(ctx)
	defer cancel()
	var r []Result
	info, err := api.NodeInfo(ctx)
	if err != nil {
		return []Result{res(g, "connection", Fail, "%v (URL, node name and API token)", err)}
	}
	r = append(r, res(g, "connection", OK, "%d CPUs, %.0f GiB RAM", info.CPUs, float64(info.MemTotal)/(1<<30)))

	if u, err := api.StoragePool(ctx, storage); err != nil {
		r = append(r, res(g, "storage", Fail, "pool %q: %v", storage, err))
	} else if f := u.Fraction(); f > 0.85 {
		r = append(r, res(g, "storage", Warn, "pool %q is %.0f%% full", storage, f*100))
	} else {
		r = append(r, res(g, "storage", OK, "pool %q %.0f%% used of %.0f GiB", storage, f*100, float64(u.Total)/(1<<30)))
	}
	if fw, err := api.HostFirewall(ctx); err != nil {
		r = append(r, res(g, "firewall", Warn, "cannot read the firewall state (the API token needs Sys.Audit): %v", err))
	} else if !fw.Cluster || !fw.Node {
		r = append(r, res(g, "firewall", Fail, "the Proxmox firewall is off at datacenter=%t node=%t: anti-spoofing filters on guests do nothing until both are on", fw.Cluster, fw.Node))
	} else {
		r = append(r, res(g, "firewall", OK, "on at datacenter and node level: guests are MAC and IP filtered"))
	}

	if d.Store != nil {
		rows, err := d.Store.Pool.Query(ctx, `SELECT t.slug, COALESCE(ht.proxmox_template_id, 0) FROM templates t
			LEFT JOIN host_templates ht ON ht.template_id = t.id AND ht.host = $1 WHERE t.active ORDER BY t.id`, host)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var t tplRow
				if rows.Scan(&t.slug, &t.vmid) != nil {
					continue
				}
				if t.vmid == 0 {
					r = append(r, res(g, "template "+t.slug, Fail, "no VMID is set for this host: xenosctl template host %s %s <vmid>", t.slug, host))
				} else if st, err := api.Status(ctx, t.vmid); err != nil {
					r = append(r, res(g, "template "+t.slug, Fail, "VMID %d: %v", t.vmid, err))
				} else if !st.Exists {
					r = append(r, res(g, "template "+t.slug, Fail, "VMID %d does not exist on the host", t.vmid))
				} else {
					r = append(r, res(g, "template "+t.slug, OK, "VMID %d present", t.vmid))
				}
			}
		}
	}
	return r
}

// ---- email ----

func mailChecks(ctx context.Context, d Deps) []Result {
	const g = "email"
	v, ok := d.Mailer.(verifier)
	if !ok {
		if d.Mailer == nil {
			return []Result{res(g, "SMTP", Fail, "no mailer")}
		}
		return []Result{res(g, "SMTP", Fail, "emails go to the log, not to customers: verification and password reset cannot work")}
	}
	ctx, cancel := step(ctx)
	defer cancel()
	if err := v.Verify(ctx); err != nil {
		return []Result{res(g, "SMTP", Fail, "%v", err)}
	}
	return []Result{res(g, "SMTP", OK, "%s:%s accepts TLS and the login", d.Cfg.SMTPHost, d.Cfg.SMTPPort)}
}

// ---- worker ----

func workerChecks(ctx context.Context, d Deps) []Result {
	const g = "worker"
	if d.Store == nil {
		return nil
	}
	ctx, cancel := step(ctx)
	defer cancel()
	at, err := d.Store.Q.GetHeartbeat(ctx, "worker")
	if err != nil {
		return []Result{res(g, "heartbeat", Warn, "the worker has never reported: start xenos-worker (exactly one instance)")}
	}
	if age := d.Now().Sub(at); age > 3*time.Minute {
		return []Result{res(g, "heartbeat", Fail, "last seen %s ago: the worker is not running", age.Round(time.Second))}
	}
	return []Result{res(g, "heartbeat", OK, "running")}
}

// Summary counts results by status.
func Summary(rs []Result) (ok, warn, fail int) {
	for _, r := range rs {
		switch r.Status {
		case OK:
			ok++
		case Warn:
			warn++
		default:
			fail++
		}
	}
	return
}
