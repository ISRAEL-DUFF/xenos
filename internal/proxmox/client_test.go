package proxmox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type recorded struct {
	method, path, auth string
	form               url.Values
}

func testClient(t *testing.T, respond func(r recorded) string) (*Client, *[]recorded) {
	var got []recorded
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		rec := recorded{r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"), r.Form}
		got = append(got, rec)
		_, _ = w.Write([]byte(`{"data":` + respond(rec) + `}`))
	}))
	t.Cleanup(ts.Close)
	return New(ts.URL, "pve1", "xenos@pve!control", "s3cret", false), &got
}

func TestRequestShapes(t *testing.T) {
	c, got := testClient(t, func(r recorded) string {
		if strings.HasSuffix(r.path, "/qemu") {
			return `[{"vmid":101,"status":"running"},{"vmid":102,"status":"stopped"}]`
		}
		return `"UPID:pve1:abc"`
	})
	ctx := context.Background()

	upid, err := c.Clone(ctx, CloneParams{TemplateID: 9000, NewID: 105, Name: "web", Storage: "vmdata"})
	if err != nil || upid != "UPID:pve1:abc" {
		t.Fatalf("clone: %q %v", upid, err)
	}
	r := (*got)[0]
	if r.method != "POST" || r.path != "/api2/json/nodes/pve1/qemu/9000/clone" || r.auth != "PVEAPIToken=xenos@pve!control=s3cret" ||
		r.form.Get("newid") != "105" || r.form.Get("full") != "1" || r.form.Get("storage") != "vmdata" || r.form.Get("name") != "web" {
		t.Fatalf("clone request: %+v", r)
	}

	keys := "ssh-ed25519 AAAA one\nssh-ed25519 BBBB two"
	if err := c.Configure(ctx, 105, ConfigParams{Cores: 2, MemoryMB: 2048, CIUser: "root", SSHKeys: keys,
		IPConfig0: "ip=1.2.3.4/32,gw=1.2.3.1", Nameserver: "1.1.1.1", DisableKVM: true}); err != nil {
		t.Fatal(err)
	}
	r = (*got)[1]
	if r.method != "PUT" || r.path != "/api2/json/nodes/pve1/qemu/105/config" || r.form.Get("cores") != "2" ||
		r.form.Get("kvm") != "0" || r.form.Get("cpu") != "qemu64" || r.form.Get("ipconfig0") != "ip=1.2.3.4/32,gw=1.2.3.1" {
		t.Fatalf("configure request: %+v", r)
	}
	// Proxmox wants sshkeys URL-encoded inside the form value (spaces as %20, not '+').
	if enc := r.form.Get("sshkeys"); enc != "ssh-ed25519%20AAAA%20one%0Assh-ed25519%20BBBB%20two" {
		t.Fatalf("sshkeys = %q", enc)
	}

	if err := c.Configure(ctx, 106, ConfigParams{Cores: 1, MemoryMB: 1024}); err != nil {
		t.Fatal(err)
	}
	if f := (*got)[2].form; f.Has("kvm") || f.Has("cpu") {
		t.Fatalf("kvm/cpu must be left alone when KVM is enabled: %v", f)
	}

	if err := c.ResizeDisk(ctx, 105, "scsi0", 40); err != nil || (*got)[3].form.Get("size") != "40G" {
		t.Fatalf("resize: %v %+v", err, (*got)[3])
	}
	if _, err := c.Power(ctx, 105, "shutdown"); err != nil {
		t.Fatal(err)
	}
	if f := (*got)[4].form; f.Get("forceStop") != "1" || f.Get("timeout") != "60" {
		t.Fatalf("shutdown must force-stop after a timeout: %v", f)
	}
	if _, err := c.Destroy(ctx, 105); err != nil {
		t.Fatal(err)
	}
	if r := (*got)[5]; r.method != "DELETE" || !strings.Contains(r.path, "purge=1") {
		t.Fatalf("destroy request: %+v", r)
	}

	for vmid, want := range map[int]VMStatus{101: {true, true}, 102: {true, false}, 103: {false, false}} {
		if st, err := c.Status(ctx, vmid); err != nil || st != want {
			t.Fatalf("status(%d) = %+v %v, want %+v", vmid, st, err, want)
		}
	}
}

func TestWaitTask(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		wantErr    bool
	}{
		{"ok", `{"status":"stopped","exitstatus":"OK"}`, false},
		{"failed", `{"status":"stopped","exitstatus":"clone failed"}`, true},
	} {
		c, _ := testClient(t, func(recorded) string { return tc.body })
		err := c.WaitTask(context.Background(), "UPID:pve1:abc")
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err = %v", tc.name, err)
		}
	}
}

func TestHTTPErrorSurfaces(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "permission check failed", http.StatusForbidden)
	}))
	defer ts.Close()
	c := New(ts.URL, "pve1", "t", "s", false)
	if _, err := c.Nodes(context.Background()); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v", err)
	}
}

func TestMonitoringEndpoints(t *testing.T) {
	c, got := testClient(t, func(r recorded) string {
		switch {
		case strings.HasSuffix(r.path, "/qemu"):
			return `[{"vmid":101,"status":"running","cpu":0.97},{"vmid":102,"status":"stopped","cpu":0}]`
		case strings.Contains(r.path, "/storage/"):
			return `{"used":850,"total":1000,"avail":150}`
		default:
			return `{"cpuinfo":{"cpus":32},"memory":{"total":68719476736,"used":1,"free":2}}`
		}
	})
	ctx := context.Background()

	guests, err := c.Guests(ctx)
	if err != nil || len(guests) != 2 || !guests[0].Running || guests[0].CPU != 0.97 || guests[1].Running {
		t.Fatalf("guests: %+v %v", guests, err)
	}
	pool, err := c.StoragePool(ctx, "vmdata")
	if err != nil || pool.Fraction() != 0.85 {
		t.Fatalf("pool: %+v %v", pool, err)
	}
	if (*got)[1].path != "/api2/json/nodes/pve1/storage/vmdata/status" {
		t.Fatalf("storage path: %s", (*got)[1].path)
	}
	node, err := c.NodeInfo(ctx)
	if err != nil || node.MemTotal != 68719476736 || node.CPUs != 32 {
		t.Fatalf("node: %+v %v", node, err)
	}
	if (Usage{}).Fraction() != 0 {
		t.Fatal("empty usage must not divide by zero")
	}
}
