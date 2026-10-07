package proxmox

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// ipsetName is the set Proxmox consults for the first NIC when ipfilter is on.
const ipsetName = "ipfilter-net0"

var firewallOpt = regexp.MustCompile(`(^|,)firewall=[01]`)

// withFirewall returns a NIC definition ("virtio=MAC,bridge=vmbr0,...") with firewall=1.
func withFirewall(net string) string {
	if firewallOpt.MatchString(net) {
		return firewallOpt.ReplaceAllString(net, "${1}firewall=1")
	}
	return net + ",firewall=1"
}

// plainAddr drops a host-route suffix so "10.0.0.1/32" and "10.0.0.1" compare equal.
func plainAddr(s string) string {
	s = strings.TrimSuffix(s, "/32")
	return strings.TrimSuffix(s, "/128")
}

func (c *Client) Isolate(ctx context.Context, vmid int, allowed []string) error {
	base := fmt.Sprintf("/nodes/%s/qemu/%d", c.node, vmid)

	var cfg map[string]any
	if err := c.do(ctx, http.MethodGet, base+"/config", nil, &cfg); err != nil {
		return err
	}
	net0, _ := cfg["net0"].(string)
	if net0 == "" {
		return fmt.Errorf("vm %d has no net0", vmid)
	}
	if err := c.do(ctx, http.MethodPut, base+"/config", url.Values{"net0": {withFirewall(net0)}}, nil); err != nil {
		return err
	}
	// The guest firewall defaults to dropping inbound traffic once enabled; filtering here is about spoofing,
	// so let everything else through (the host's own rules decide what reaches the guests).
	opts := url.Values{"enable": {"1"}, "macfilter": {"1"}, "ipfilter": {"1"}, "policy_in": {"ACCEPT"}, "policy_out": {"ACCEPT"}, "dhcp": {"0"}, "ndp": {"1"}}
	if err := c.do(ctx, http.MethodPut, base+"/firewall/options", opts, nil); err != nil {
		return err
	}

	return c.syncIPSet(ctx, base, ipsetName, allowed)
}

// syncIPSet makes the named firewall IP set hold exactly the allowed addresses (creating it if needed).
func (c *Client) syncIPSet(ctx context.Context, base, set string, allowed []string) error {
	var sets []struct {
		Name string `json:"name"`
	}
	if err := c.do(ctx, http.MethodGet, base+"/firewall/ipset", nil, &sets); err != nil {
		return err
	}
	exists := false
	for _, s := range sets {
		exists = exists || s.Name == set
	}
	if !exists {
		if err := c.do(ctx, http.MethodPost, base+"/firewall/ipset", url.Values{"name": {set}}, nil); err != nil {
			return err
		}
	}
	var have []struct {
		CIDR string `json:"cidr"`
	}
	if err := c.do(ctx, http.MethodGet, base+"/firewall/ipset/"+set, nil, &have); err != nil {
		return err
	}
	want := map[string]bool{}
	for _, a := range allowed {
		want[plainAddr(a)] = true
	}
	present := map[string]bool{}
	for _, h := range have {
		a := plainAddr(h.CIDR)
		present[a] = true
		if !want[a] {
			if err := c.do(ctx, http.MethodDelete, base+"/firewall/ipset/"+set+"/"+url.PathEscape(h.CIDR), nil, nil); err != nil {
				return err
			}
		}
	}
	for _, a := range allowed {
		if present[plainAddr(a)] {
			continue
		}
		if err := c.do(ctx, http.MethodPost, base+"/firewall/ipset/"+set, url.Values{"cidr": {plainAddr(a)}}, nil); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) HostFirewall(ctx context.Context) (HostFirewallState, error) {
	enabled := func(path string) (bool, error) {
		var o struct {
			Enable pveBool `json:"enable"`
		}
		if err := c.do(ctx, http.MethodGet, path, nil, &o); err != nil {
			return false, err
		}
		return bool(o.Enable), nil
	}
	var s HostFirewallState
	var err error
	if s.Cluster, err = enabled("/cluster/firewall/options"); err != nil {
		return s, err
	}
	s.Node, err = enabled("/nodes/" + c.node + "/firewall/options")
	return s, err
}

// nicOpt reads one NIC option ("virtio=MAC,bridge=vmbr1,tag=5" -> "bridge" -> "vmbr1").
func nicMAC(net string) string {
	first, _, _ := strings.Cut(net, ",")
	if _, mac, ok := strings.Cut(first, "="); ok {
		return mac
	}
	return ""
}

func (c *Client) SetNIC(ctx context.Context, vmid int, p NICParams) error {
	base := fmt.Sprintf("/nodes/%s/qemu/%d", c.node, vmid)
	var cfg map[string]any
	if err := c.do(ctx, http.MethodGet, base+"/config", nil, &cfg); err != nil {
		return err
	}
	def := "virtio"
	if cur, _ := cfg["net"+strconv.Itoa(p.Slot)].(string); cur != "" {
		if mac := nicMAC(cur); mac != "" {
			def = "virtio=" + mac // keep the address the guest already knows
		}
	}
	net := fmt.Sprintf("%s,bridge=%s,tag=%d,firewall=1", def, p.Bridge, p.VLAN)
	f := url.Values{fmt.Sprintf("net%d", p.Slot): {net}, fmt.Sprintf("ipconfig%d", p.Slot): {p.IPConfig}}
	return c.do(ctx, http.MethodPut, base+"/config", f, nil)
}

func (c *Client) RemoveNIC(ctx context.Context, vmid int, slot int) error {
	base := fmt.Sprintf("/nodes/%s/qemu/%d", c.node, vmid)
	var cfg map[string]any
	if err := c.do(ctx, http.MethodGet, base+"/config", nil, &cfg); err != nil {
		return err
	}
	var del []string // Proxmox refuses to delete an option the VM does not have
	for _, k := range []string{fmt.Sprintf("net%d", slot), fmt.Sprintf("ipconfig%d", slot)} {
		if _, ok := cfg[k]; ok {
			del = append(del, k)
		}
	}
	if len(del) == 0 {
		return nil
	}
	return c.do(ctx, http.MethodPut, base+"/config", url.Values{"delete": {strings.Join(del, ",")}}, nil)
}

func (c *Client) IsolateNIC(ctx context.Context, vmid int, slot int, allowed []string) error {
	base := fmt.Sprintf("/nodes/%s/qemu/%d", c.node, vmid)
	opts := url.Values{"enable": {"1"}, "macfilter": {"1"}, "ipfilter": {"1"}, "policy_in": {"ACCEPT"}, "policy_out": {"ACCEPT"}, "dhcp": {"0"}, "ndp": {"1"}}
	if err := c.do(ctx, http.MethodPut, base+"/firewall/options", opts, nil); err != nil {
		return err
	}
	return c.syncIPSet(ctx, base, fmt.Sprintf("ipfilter-net%d", slot), allowed)
}

func (c *Client) Bridges(ctx context.Context) ([]string, error) {
	var nets []struct {
		Iface string `json:"iface"`
		Type  string `json:"type"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/nodes/%s/network", c.node), nil, &nets); err != nil {
		return nil, err
	}
	var out []string
	for _, n := range nets {
		if n.Type == "bridge" || n.Type == "OVSBridge" {
			out = append(out, n.Iface)
		}
	}
	return out, nil
}
