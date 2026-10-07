package hosts

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/israel-duff/xenos/internal/config"
	"github.com/israel-duff/xenos/internal/proxmox"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func writeFile(t *testing.T, body string) string {
	p := filepath.Join(t.TempDir(), "hosts.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNoFileMeansOneDefaultHost(t *testing.T) {
	s, err := Load(config.Config{Region: "r", PVEStorage: "vmdata", PVEDisk: "scsi0"}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Names(); len(got) != 1 || got[0] != Default {
		t.Fatalf("hosts = %v", got)
	}
	h, _ := s.Get(Default)
	if h.Storage != "vmdata" || h.Region != "r" {
		t.Fatalf("host = %+v", h)
	}
}

func TestHostsFile(t *testing.T) {
	p := writeFile(t, `
hosts:
  - name: pve1
    url: https://10.0.0.1:8006
    node: pve1
    token_id: xenos@pve!c
    token_secret: s1
    storage: fast
    ipv6_prefix: 2001:db8:1::/64
  - name: pve2
    region: lagos-2
    url: https://10.0.0.2:8006
    node: pve2
    token_id: xenos@pve!c
    token_secret: s2
`)
	s, err := Load(config.Config{HostsFile: p, Region: "lagos-1", PVEStorage: "vmdata", PVEDisk: "scsi0"}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(s.Names(), ","); got != "pve1,pve2" {
		t.Fatalf("hosts = %s", got)
	}
	a, _ := s.Get("pve1")
	b, _ := s.Get("pve2")
	if a.Storage != "fast" || a.Region != "lagos-1" || !a.IPv6Prefix.IsValid() || b.Region != "lagos-2" || b.Storage != "vmdata" {
		t.Fatalf("settings: %+v %+v", a, b)
	}
}

func TestHostsFileMistakesAreRefused(t *testing.T) {
	for name, body := range map[string]string{
		"missing secret": "hosts:\n  - {name: a, url: u, node: n, token_id: t}\n",
		"bad name":       "hosts:\n  - {name: A_B, url: u, node: n, token_id: t, token_secret: s}\n",
		"duplicate":      "hosts:\n  - {name: a, url: u, node: n, token_id: t, token_secret: s}\n  - {name: a, url: u, node: n, token_id: t, token_secret: s}\n",
		"unknown key":    "hosts:\n  - {name: a, url: u, node: n, token_id: t, token_secret: s, stoarge: x}\n",
		"empty":          "hosts: []\n",
	} {
		if _, err := Load(config.Config{HostsFile: writeFile(t, body)}, quiet); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestCallsGoToTheHostHoldingTheVMID(t *testing.T) {
	f1, f2 := proxmox.NewFake(), proxmox.NewFake()
	f1.VMs[101] = &proxmox.FakeVM{ID: 101}
	f2.VMs[202] = &proxmox.FakeVM{ID: 202}
	s := NewSet(&Host{Name: "a", API: f1, Node: "na"}, &Host{Name: "b", API: f2, Node: "nb"})
	s.Resolve = func(_ context.Context, vmid int) (string, error) {
		if vmid == 202 {
			return "b", nil
		}
		return "a", nil
	}
	ctx := context.Background()
	if st, err := s.Status(ctx, 202); err != nil || !st.Exists {
		t.Fatalf("202: %+v %v", st, err)
	}
	if st, _ := s.Status(ctx, 101); !st.Exists {
		t.Fatal("101 should be found on a")
	}
	if _, err := s.Power(ctx, 202, "start"); err != nil || !f2.VMs[202].Running || f1.VMs[101].Running {
		t.Fatal("start must reach host b only")
	}
	// A host this process does not know is an error, not a guess.
	s.Resolve = func(context.Context, int) (string, error) { return "gone", nil }
	if _, err := s.Status(ctx, 303); err == nil {
		t.Fatal("an unknown host must fail")
	}
	// Task ids route by node.
	if s.byUPID("UPID:nb:0001:x") != proxmox.API(f2) || s.byUPID("UPID:start") != proxmox.API(f1) {
		t.Fatal("UPID routing")
	}
}
