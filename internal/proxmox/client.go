// Package proxmox is a thin client over the Proxmox VE REST API using an API token.
// The control plane is the only component that talks to Proxmox.
package proxmox

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	base  string
	token string // "USER@REALM!TOKENID=SECRET"
	node  string
	http  *http.Client
}

// New builds a client. Proxmox ships a self-signed cert by default; pass
// insecureTLS only until a proper cert is installed on the host.
func New(baseURL, node, tokenID, tokenSecret string, insecureTLS bool) *Client {
	return &Client{
		base:  strings.TrimRight(baseURL, "/") + "/api2/json",
		token: fmt.Sprintf("%s=%s", tokenID, tokenSecret),
		node:  node,
		http: &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: insecureTLS}},
		},
	}
}

func (c *Client) do(ctx context.Context, method, path string, form url.Values, out any) error {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "PVEAPIToken="+c.token)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("proxmox %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(raw)))
	}
	if out == nil {
		return nil
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return err
	}
	return json.Unmarshal(env.Data, out)
}

// Nodes lists cluster nodes; used as the connectivity smoke test.
func (c *Client) Nodes(ctx context.Context) ([]map[string]any, error) {
	var out []map[string]any
	return out, c.do(ctx, http.MethodGet, "/nodes", nil, &out)
}

// CloneParams describes a full clone of a template.
type CloneParams struct {
	TemplateID int
	NewID      int
	Name       string
	Storage    string
}

// Clone starts a full clone and returns the task UPID.
func (c *Client) Clone(ctx context.Context, p CloneParams) (string, error) {
	f := url.Values{"newid": {fmt.Sprint(p.NewID)}, "name": {p.Name}, "full": {"1"}, "storage": {p.Storage}}
	var upid string
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/qemu/%d/clone", c.node, p.TemplateID), f, &upid)
	return upid, err
}

// ConfigParams is the post-clone VM configuration (cores, memory, cloud-init).
type ConfigParams struct {
	Cores      int
	MemoryMB   int
	CIUser     string
	SSHKeys    string // newline-separated public keys
	IPConfig0  string // e.g. "ip=203.0.113.10/32,gw=203.0.113.1,ip6=2001:db8::10/64,gw6=2001:db8::1"
	Nameserver string
	// DisableKVM runs the guest under software emulation (kvm=0, cpu=qemu64).
	// For test hosts without hardware virtualisation only; it is very slow.
	DisableKVM bool
}

func (c *Client) Configure(ctx context.Context, vmid int, p ConfigParams) error {
	f := url.Values{
		"cores":      {fmt.Sprint(p.Cores)},
		"memory":     {fmt.Sprint(p.MemoryMB)},
		"ciuser":     {p.CIUser},
		"sshkeys":    {url.PathEscape(p.SSHKeys)}, // PVE expects URL-encoded keys
		"ipconfig0":  {p.IPConfig0},
		"nameserver": {p.Nameserver},
	}
	if p.DisableKVM {
		f.Set("kvm", "0")
		f.Set("cpu", "qemu64")
	}
	return c.do(ctx, http.MethodPut, fmt.Sprintf("/nodes/%s/qemu/%d/config", c.node, vmid), f, nil)
}

func (c *Client) ResizeDisk(ctx context.Context, vmid int, disk string, sizeGB int) error {
	f := url.Values{"disk": {disk}, "size": {fmt.Sprintf("%dG", sizeGB)}}
	return c.do(ctx, http.MethodPut, fmt.Sprintf("/nodes/%s/qemu/%d/resize", c.node, vmid), f, nil)
}

// Power performs start, stop, shutdown or reboot and returns the task UPID.
func (c *Client) Power(ctx context.Context, vmid int, action string) (string, error) {
	f := url.Values{}
	if action == "shutdown" {
		f.Set("timeout", "60")
		f.Set("forceStop", "1") // hard-stop if the guest ignores ACPI shutdown
	}
	var upid string
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/qemu/%d/status/%s", c.node, vmid, action), f, &upid)
	return upid, err
}

// Status reports whether a QEMU guest with this VMID exists and is running.
func (c *Client) Status(ctx context.Context, vmid int) (VMStatus, error) {
	var vms []struct {
		VMID   int    `json:"vmid"`
		Status string `json:"status"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/nodes/%s/qemu", c.node), nil, &vms); err != nil {
		return VMStatus{}, err
	}
	for _, v := range vms {
		if v.VMID == vmid {
			return VMStatus{Exists: true, Running: v.Status == "running"}, nil
		}
	}
	return VMStatus{}, nil
}

// Guests lists every QEMU guest on the node with its power state and current CPU use.
func (c *Client) Guests(ctx context.Context) ([]Guest, error) {
	var raw []struct {
		VMID   int     `json:"vmid"`
		Status string  `json:"status"`
		CPU    float64 `json:"cpu"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/nodes/%s/qemu", c.node), nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Guest, 0, len(raw))
	for _, g := range raw {
		out = append(out, Guest{VMID: g.VMID, Running: g.Status == "running", CPU: g.CPU})
	}
	return out, nil
}

// StoragePool reports a storage pool's usage (the thin pool VM disks live on).
func (c *Client) StoragePool(ctx context.Context, storage string) (Usage, error) {
	var st struct {
		Used  int64 `json:"used"`
		Total int64 `json:"total"`
	}
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/nodes/%s/storage/%s/status", c.node, url.PathEscape(storage)), nil, &st)
	return Usage{Used: st.Used, Total: st.Total}, err
}

// NodeInfo reports the host's logical CPU count and physical RAM.
func (c *Client) NodeInfo(ctx context.Context) (NodeInfo, error) {
	var st struct {
		CPUInfo struct {
			CPUs int `json:"cpus"`
		} `json:"cpuinfo"`
		Memory struct {
			Total int64 `json:"total"`
		} `json:"memory"`
	}
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/nodes/%s/status", c.node), nil, &st)
	return NodeInfo{CPUs: st.CPUInfo.CPUs, MemTotal: st.Memory.Total}, err
}

func (c *Client) Destroy(ctx context.Context, vmid int) (string, error) {
	var upid string
	err := c.do(ctx, http.MethodDelete, fmt.Sprintf("/nodes/%s/qemu/%d?purge=1&destroy-unreferenced-disks=1", c.node, vmid), nil, &upid)
	return upid, err
}

// AgentPing succeeds once the QEMU guest agent responds, i.e. the guest booted.
func (c *Client) AgentPing(ctx context.Context, vmid int) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/qemu/%d/agent/ping", c.node, vmid), url.Values{}, nil)
}

// WaitTask polls a task until it stops and errors if it did not finish OK.
func (c *Client) WaitTask(ctx context.Context, upid string) error {
	for {
		var st struct {
			Status     string `json:"status"`
			ExitStatus string `json:"exitstatus"`
		}
		path := fmt.Sprintf("/nodes/%s/tasks/%s/status", c.node, url.PathEscape(upid))
		if err := c.do(ctx, http.MethodGet, path, nil, &st); err != nil {
			return err
		}
		if st.Status == "stopped" {
			if st.ExitStatus != "OK" {
				return fmt.Errorf("proxmox task %s: %s", upid, st.ExitStatus)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// SetResources changes cores and memory. The new values apply after the guest is fully stopped and started.
func (c *Client) SetResources(ctx context.Context, vmid int, cores, memoryMB int) error {
	f := url.Values{"cores": {fmt.Sprint(cores)}, "memory": {fmt.Sprint(memoryMB)}}
	return c.do(ctx, http.MethodPut, fmt.Sprintf("/nodes/%s/qemu/%d/config", c.node, vmid), f, nil)
}

// DiskSizeGB reads the size of a disk (e.g. scsi0) from the VM config, where it appears as
// "vmdata:vm-100-disk-0,size=20G". Sizes are rounded up to whole GiB.
func (c *Client) DiskSizeGB(ctx context.Context, vmid int, disk string) (int, error) {
	var cfg map[string]any
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/nodes/%s/qemu/%d/config", c.node, vmid), nil, &cfg); err != nil {
		return 0, err
	}
	v, _ := cfg[disk].(string)
	return parseDiskSize(v)
}

func parseDiskSize(v string) (int, error) {
	for _, part := range strings.Split(v, ",") {
		val, ok := strings.CutPrefix(part, "size=")
		if !ok || val == "" {
			continue
		}
		unit := val[len(val)-1]
		n, err := strconv.ParseFloat(val[:len(val)-1], 64)
		if err != nil {
			break
		}
		switch unit {
		case 'T':
			n *= 1024
		case 'G':
		case 'M':
			n /= 1024
		default:
			return 0, fmt.Errorf("proxmox: unknown disk size unit in %q", v)
		}
		return int(math.Ceil(n)), nil
	}
	return 0, fmt.Errorf("proxmox: no disk size in %q", v)
}

func (c *Client) SnapshotCreate(ctx context.Context, vmid int, name string) (string, error) {
	var upid string
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/qemu/%d/snapshot", c.node, vmid),
		url.Values{"snapname": {name}, "description": {"xenos customer snapshot"}}, &upid)
	return upid, err
}

func (c *Client) SnapshotList(ctx context.Context, vmid int) ([]string, error) {
	var raw []struct {
		Name string `json:"name"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/nodes/%s/qemu/%d/snapshot", c.node, vmid), nil, &raw); err != nil {
		return nil, err
	}
	var out []string
	for _, s := range raw {
		if s.Name != "current" {
			out = append(out, s.Name)
		}
	}
	return out, nil
}

func (c *Client) SnapshotRollback(ctx context.Context, vmid int, name string) (string, error) {
	var upid string
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/qemu/%d/snapshot/%s/rollback", c.node, vmid, url.PathEscape(name)), url.Values{}, &upid)
	return upid, err
}

func (c *Client) SnapshotDelete(ctx context.Context, vmid int, name string) (string, error) {
	var upid string
	err := c.do(ctx, http.MethodDelete, fmt.Sprintf("/nodes/%s/qemu/%d/snapshot/%s", c.node, vmid, url.PathEscape(name)), nil, &upid)
	return upid, err
}
