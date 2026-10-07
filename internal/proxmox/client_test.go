package proxmox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
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

func TestResizeAndSnapshotRequestShapes(t *testing.T) {
	c, got := testClient(t, func(r recorded) string {
		switch {
		case r.method == "GET" && strings.HasSuffix(r.path, "/config"):
			return `{"scsi0":"vmdata:vm-105-disk-0,size=20G","cores":1}`
		case r.method == "GET" && strings.HasSuffix(r.path, "/snapshot"):
			return `[{"name":"xs1"},{"name":"current"}]`
		}
		return `"UPID:pve1:abc"`
	})
	ctx := context.Background()

	if err := c.SetResources(ctx, 105, 2, 2048); err != nil {
		t.Fatal(err)
	}
	if r := (*got)[0]; r.method != "PUT" || r.path != "/api2/json/nodes/pve1/qemu/105/config" || r.form.Get("cores") != "2" || r.form.Get("memory") != "2048" {
		t.Fatalf("set resources: %+v", r)
	}
	if n, err := c.DiskSizeGB(ctx, 105, "scsi0"); err != nil || n != 20 {
		t.Fatalf("disk size = %d %v", n, err)
	}
	if _, err := c.SnapshotCreate(ctx, 105, "xs2"); err != nil {
		t.Fatal(err)
	}
	if r := (*got)[2]; r.method != "POST" || r.path != "/api2/json/nodes/pve1/qemu/105/snapshot" || r.form.Get("snapname") != "xs2" {
		t.Fatalf("snapshot create: %+v", r)
	}
	if names, err := c.SnapshotList(ctx, 105); err != nil || len(names) != 1 || names[0] != "xs1" {
		t.Fatalf("snapshot list = %v %v (the pseudo entry \"current\" must be hidden)", names, err)
	}
	if _, err := c.SnapshotRollback(ctx, 105, "xs1"); err != nil {
		t.Fatal(err)
	}
	if r := (*got)[4]; r.method != "POST" || r.path != "/api2/json/nodes/pve1/qemu/105/snapshot/xs1/rollback" {
		t.Fatalf("rollback: %+v", r)
	}
	if _, err := c.SnapshotDelete(ctx, 105, "xs1"); err != nil {
		t.Fatal(err)
	}
	if r := (*got)[5]; r.method != "DELETE" || r.path != "/api2/json/nodes/pve1/qemu/105/snapshot/xs1" {
		t.Fatalf("delete: %+v", r)
	}
}

func TestParseDiskSize(t *testing.T) {
	for in, want := range map[string]int{
		"vmdata:vm-1-disk-0,size=20G":                  20,
		"vmdata:vm-1-disk-0,discard=on,size=40G,ssd=1": 40,
		"vmdata:vm-1-disk-0,size=512M":                 1,
		"vmdata:vm-1-disk-0,size=1T":                   1024,
		"vmdata:vm-1-disk-0,size=20.5G":                21,
	} {
		if got, err := parseDiskSize(in); err != nil || got != want {
			t.Errorf("%q = %d %v, want %d", in, got, err, want)
		}
	}
	if _, err := parseDiskSize(""); err == nil {
		t.Error("a config without that disk must be an error")
	}
}

// The console opens a VNC session with websocket=1, then dials the host's websocket with the API token.
func TestConsoleTicketAndWebsocket(t *testing.T) {
	var auth, query string
	up := websocket.Upgrader{Subprotocols: []string{"binary"}}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api2/json/nodes/pve1/qemu/105/vncproxy":
			_ = r.ParseForm()
			if r.Form.Get("websocket") != "1" {
				t.Errorf("vncproxy form: %v", r.Form)
			}
			_, _ = w.Write([]byte(`{"data":{"port":"5901","ticket":"PVEVNC:abc","upid":"x"}}`))
		case r.URL.Path == "/api2/json/nodes/pve1/qemu/105/vncwebsocket":
			auth, query = r.Header.Get("Authorization"), r.URL.RawQuery
			conn, err := up.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			_ = conn.WriteMessage(websocket.BinaryMessage, []byte("RFB 003.008\n"))
		}
	}))
	defer ts.Close()
	c := New(ts.URL, "pve1", "xenos@pve!control", "s3cret", false)
	ctx := context.Background()

	tk, err := c.Console(ctx, 105)
	if err != nil || tk.Port != 5901 || tk.Ticket != "PVEVNC:abc" {
		t.Fatalf("ticket %+v %v", tk, err)
	}
	conn, err := c.DialConsole(ctx, 105, tk)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if msg, err := conn.ReadMessage(); err != nil || string(msg) != "RFB 003.008\n" {
		t.Fatalf("message %q %v", msg, err)
	}
	if auth != "PVEAPIToken=xenos@pve!control=s3cret" || !strings.Contains(query, "port=5901") || !strings.Contains(query, "vncticket=PVEVNC%3Aabc") {
		t.Fatalf("websocket request: auth=%q query=%q", auth, query)
	}
}

func TestAgentExecRequestShapes(t *testing.T) {
	c, got := testClient(t, func(r recorded) string {
		if strings.Contains(r.path, "exec-status") {
			return `{"exited":1,"exitcode":3,"out-data":"hello\n","err-data":"oops\n"}`
		}
		return `{"pid":4242}`
	})
	ctx := context.Background()
	pid, err := c.AgentExec(ctx, 105, []string{"/bin/bash", "-s"}, "echo hi")
	if err != nil || pid != 4242 {
		t.Fatalf("exec = %d %v", pid, err)
	}
	r := (*got)[0]
	if r.method != "POST" || r.path != "/api2/json/nodes/pve1/qemu/105/agent/exec" || strings.Join(r.form["command"], " ") != "/bin/bash -s" || r.form.Get("input-data") != "echo hi" {
		t.Fatalf("exec request: %+v", r)
	}
	st, err := c.AgentExecStatus(ctx, 105, 4242)
	if err != nil || !st.Exited || st.ExitCode != 3 || st.Output != "hello\noops\n" {
		t.Fatalf("status = %+v %v", st, err)
	}
	if r := (*got)[1]; r.method != "GET" || r.path != "/api2/json/nodes/pve1/qemu/105/agent/exec-status?pid=4242" {
		t.Fatalf("status request: %+v", r)
	}
}

func TestIsolateSetsFirewallAndSyncsTheIPSet(t *testing.T) {
	c, got := testClient(t, func(r recorded) string {
		switch {
		case r.method == "GET" && strings.HasSuffix(r.path, "/config"):
			return `{"net0":"virtio=BC:24:11:AA:BB:CC,bridge=vmbr0"}`
		case r.method == "GET" && strings.HasSuffix(r.path, "/firewall/ipset"):
			return `[{"name":"ipfilter-net0"}]`
		case r.method == "GET" && strings.HasSuffix(r.path, "/firewall/ipset/ipfilter-net0"):
			return `[{"cidr":"203.0.113.9"},{"cidr":"203.0.113.10/32"}]`
		}
		return `null`
	})
	if err := c.Isolate(context.Background(), 105, []string{"203.0.113.10", "2001:db8::10"}); err != nil {
		t.Fatal(err)
	}
	var net0, opts url.Values
	var deleted, added []string
	for _, r := range *got {
		switch {
		case r.method == "PUT" && strings.HasSuffix(r.path, "/qemu/105/config"):
			net0 = r.form
		case r.method == "PUT" && strings.HasSuffix(r.path, "/firewall/options"):
			opts = r.form
		case r.method == "DELETE":
			deleted = append(deleted, r.path)
		case r.method == "POST" && strings.HasSuffix(r.path, "/ipfilter-net0"):
			added = append(added, r.form.Get("cidr"))
		}
	}
	if net0.Get("net0") != "virtio=BC:24:11:AA:BB:CC,bridge=vmbr0,firewall=1" {
		t.Fatalf("net0 = %q", net0.Get("net0"))
	}
	if opts.Get("enable") != "1" || opts.Get("ipfilter") != "1" || opts.Get("macfilter") != "1" || opts.Get("policy_in") != "ACCEPT" {
		t.Fatalf("options = %v", opts)
	}
	if len(deleted) != 1 || !strings.HasSuffix(deleted[0], "/ipfilter-net0/203.0.113.9") {
		t.Fatalf("stale entry not removed: %v", deleted)
	}
	if len(added) != 1 || added[0] != "2001:db8::10" {
		t.Fatalf("added = %v (an address already in the set must not be added twice)", added)
	}
}

func TestWithFirewallKeepsTheNIC(t *testing.T) {
	for in, want := range map[string]string{
		"virtio=AA,bridge=vmbr0":                  "virtio=AA,bridge=vmbr0,firewall=1",
		"virtio=AA,bridge=vmbr0,firewall=0":       "virtio=AA,bridge=vmbr0,firewall=1",
		"virtio=AA,firewall=1,bridge=vmbr0,tag=5": "virtio=AA,firewall=1,bridge=vmbr0,tag=5",
	} {
		if got := withFirewall(in); got != want {
			t.Errorf("withFirewall(%q) = %q, want %q", in, got, want)
		}
	}
}
