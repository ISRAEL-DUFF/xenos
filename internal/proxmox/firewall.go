package proxmox

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
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

	var sets []struct {
		Name string `json:"name"`
	}
	if err := c.do(ctx, http.MethodGet, base+"/firewall/ipset", nil, &sets); err != nil {
		return err
	}
	exists := false
	for _, s := range sets {
		exists = exists || s.Name == ipsetName
	}
	if !exists {
		if err := c.do(ctx, http.MethodPost, base+"/firewall/ipset", url.Values{"name": {ipsetName}}, nil); err != nil {
			return err
		}
	}
	var have []struct {
		CIDR string `json:"cidr"`
	}
	if err := c.do(ctx, http.MethodGet, base+"/firewall/ipset/"+ipsetName, nil, &have); err != nil {
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
			if err := c.do(ctx, http.MethodDelete, base+"/firewall/ipset/"+ipsetName+"/"+url.PathEscape(h.CIDR), nil, nil); err != nil {
				return err
			}
		}
	}
	for _, a := range allowed {
		if present[plainAddr(a)] {
			continue
		}
		if err := c.do(ctx, http.MethodPost, base+"/firewall/ipset/"+ipsetName, url.Values{"cidr": {plainAddr(a)}}, nil); err != nil {
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
