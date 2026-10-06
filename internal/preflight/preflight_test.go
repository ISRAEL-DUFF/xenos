package preflight

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/israel-duff/xenos/internal/billing"
	"github.com/israel-duff/xenos/internal/config"
	"github.com/israel-duff/xenos/internal/mail"
	"github.com/israel-duff/xenos/internal/proxmox"
	"github.com/israel-duff/xenos/internal/store/db"
	"github.com/israel-duff/xenos/internal/testutil"
)

func find(rs []Result, group, name string) Result {
	for _, r := range rs {
		if r.Group == group && r.Name == name {
			return r
		}
	}
	return Result{Status: "missing"}
}

func goodConfig() config.Config {
	return config.Config{Env: "production", PublicURL: "https://app.example.com", CookieSecure: true, HTTPAddr: "127.0.0.1:8080",
		TrustProxy: true, ISpendURL: "https://i", ISpendAPIKey: "k", ISpendWebhookSecret: "whsec", ISpendOwnerPrefix: "xenos-prod",
		PVEURL: "https://pve:8006", PVETokenID: "t", PVETokenSecret: "s", PVENode: "pve", PVEStorage: "vmdata",
		IPv6Prefix: "2001:db8::/64", IPv6Gateway: "2001:db8::1", AlertEmail: "ops@example.com", MeterSpreadMinutes: 40}
}

func TestConfigChecksFlagUnsafeSettings(t *testing.T) {
	good := configChecks(context.Background(), Deps{Cfg: goodConfig()})
	for _, r := range good {
		if r.Status == Fail {
			t.Errorf("a complete config should not fail: %+v", r)
		}
	}
	bad := goodConfig()
	bad.Env = "development"
	bad.PublicURL = "http://localhost:8080"
	bad.CookieSecure = false
	bad.HTTPAddr = ":8080"
	bad.ISpendURL = ""
	bad.ISpendWebhookSecret = ""
	bad.PVEDisableKVM = true
	bad.ISpendOwnerPrefix = "xenos-sbx-1"
	rs := configChecks(context.Background(), Deps{Cfg: bad})
	for name, want := range map[string]Status{
		"environment": Warn, "public URL": Fail, "secure cookies": Fail, "listen address": Warn,
		"iswallet URL and key": Fail, "webhook secret": Fail, "KVM": Fail, "owner prefix": Warn,
	} {
		if got := find(rs, "config", name).Status; got != want {
			t.Errorf("%s: got %s, want %s", name, got, want)
		}
	}
}

func TestFakesAreReportedAsFailures(t *testing.T) {
	d := Deps{Cfg: goodConfig(), ISpend: billing.NewFake(150_000), PVE: proxmox.NewFake(), Mailer: mail.LogMailer{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	rs := Run(context.Background(), d)
	for _, g := range []string{"iswallet", "proxmox", "email"} {
		var failed bool
		for _, r := range rs {
			if r.Group == g && r.Status == Fail {
				failed = true
			}
		}
		if !failed {
			t.Errorf("%s: the fake/log implementation must be a failure in a real preflight", g)
		}
	}
	if !Failed(rs) {
		t.Error("Failed must be true")
	}
}

func TestDatabaseAndProxmoxChecks(t *testing.T) {
	st := testutil.DB(t)
	ctx := context.Background()
	pve := proxmox.NewFake()
	d := Deps{Cfg: goodConfig(), Store: st, PVE: pve, ISpend: billing.NewFake(150_000), AllowFakes: true,
		Mailer: mail.LogMailer{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}

	rs := Run(ctx, d)
	if r := find(rs, "database", "IP pool"); r.Status != Fail || !strings.Contains(r.Detail, "no addresses") {
		t.Errorf("empty pool: %+v", r)
	}
	if r := find(rs, "database", "plan prices"); r.Status != Warn { // seeds are placeholders: ask the operator to confirm
		t.Errorf("seed prices: %+v", r)
	}
	if r := find(rs, "proxmox", "template ubuntu-24.04"); r.Status != Fail || !strings.Contains(r.Detail, "does not exist") {
		t.Errorf("a template missing on the host: %+v", r)
	}
	if r := find(rs, "worker", "heartbeat"); r.Status != Warn {
		t.Errorf("a worker that never reported: %+v", r)
	}

	// Provide what the checks look for.
	if _, err := st.Pool.Exec(ctx, `INSERT INTO ip_addresses (address, gateway, region) SELECT ('10.0.0.'||g)::inet, '10.0.0.1'::inet, 'test-1' FROM generate_series(10, 30) g`); err != nil {
		t.Fatal(err)
	}
	pve.VMs[9000] = &proxmox.FakeVM{}
	pve.VMs[9001] = &proxmox.FakeVM{}
	if err := st.Q.Heartbeat(ctx, db.HeartbeatParams{Name: "worker", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	rs = Run(ctx, d)
	for _, want := range []struct{ g, n string }{{"database", "IP pool"}, {"proxmox", "template ubuntu-24.04"}, {"proxmox", "template debian-12"}, {"worker", "heartbeat"}} {
		if r := find(rs, want.g, want.n); r.Status != OK {
			t.Errorf("%s/%s: %+v", want.g, want.n, r)
		}
	}

	// A stale heartbeat means the worker is down.
	d.Now = func() time.Time { return time.Now().Add(10 * time.Minute) }
	if r := find(Run(ctx, d), "worker", "heartbeat"); r.Status != Fail {
		t.Errorf("stale heartbeat: %+v", r)
	}

	// A plan priced above a full month is refused.
	if _, err := st.Pool.Exec(ctx, `UPDATE plans SET price_uusdt_monthly_cap = price_uusdt_hourly * 2000 WHERE slug = 'nano'`); err != nil {
		t.Fatal(err)
	}
	if r := find(Run(ctx, d), "database", "plan prices"); r.Status != Fail {
		t.Errorf("a monthly cap above a full month: %+v", r)
	}
}
