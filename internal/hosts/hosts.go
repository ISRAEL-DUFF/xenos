// Package hosts holds the Proxmox hosts behind one control plane. They are standalone nodes (no cluster, no
// shared storage). A VM lives on one host for life, and VMIDs are unique across hosts, so a VMID identifies its
// host: Set implements proxmox.API by sending every VMID-keyed call to the right host.
package hosts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/israel-duff/xenos/internal/config"
	"github.com/israel-duff/xenos/internal/proxmox"
	"github.com/israel-duff/xenos/internal/store"
)

// Default is the name of the host described by the XENOS_PVE_* variables.
const Default = "default"

// Host is one Proxmox host and how guests are built on it.
type Host struct {
	Name, Region string
	API          proxmox.API
	Node         string
	Storage      string
	Disk         string
	Bridge       string
	DisableKVM   bool
	IPv6Prefix   netip.Prefix
	IPv6Gateway  string
	Nameservers  string
}

// fileEntry is one host in the YAML file.
type fileEntry struct {
	Name        string `yaml:"name"`
	Region      string `yaml:"region"`
	URL         string `yaml:"url"`
	Node        string `yaml:"node"`
	TokenID     string `yaml:"token_id"`
	TokenSecret string `yaml:"token_secret"`
	InsecureTLS bool   `yaml:"insecure_tls"`
	Storage     string `yaml:"storage"`
	Disk        string `yaml:"disk"`
	Bridge      string `yaml:"bridge"`
	DisableKVM  bool   `yaml:"disable_kvm"`
	IPv6Prefix  string `yaml:"ipv6_prefix"`
	IPv6Gateway string `yaml:"ipv6_gateway"`
	Nameservers string `yaml:"nameservers"`
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)

// Set is every configured host. Resolve maps a VMID to a host name (normally a database lookup); results are
// cached because a VM never changes host.
type Set struct {
	byName  map[string]*Host
	order   []string
	Resolve func(ctx context.Context, vmid int) (string, error)

	mu    sync.Mutex
	cache map[int]string
}

var _ proxmox.API = (*Set)(nil)

// NewSet builds a set from ready-made hosts (tests, or Load).
func NewSet(hs ...*Host) *Set {
	s := &Set{byName: map[string]*Host{}, cache: map[int]string{}}
	for _, h := range hs {
		s.byName[h.Name] = h
		s.order = append(s.order, h.Name)
	}
	sort.Strings(s.order)
	return s
}

// Single wraps one API as the host "default", for tests and single-host setups.
func Single(api proxmox.API, h Host) *Set {
	h.Name, h.API = Default, api
	return NewSet(&h)
}

// Load reads XENOS_HOSTS_FILE, or builds the one host "default" from the XENOS_PVE_* variables.
func Load(cfg config.Config, log *slog.Logger) (*Set, error) {
	var v6 netip.Prefix
	if cfg.IPv6Prefix != "" {
		var err error
		if v6, err = netip.ParsePrefix(cfg.IPv6Prefix); err != nil {
			return nil, fmt.Errorf("XENOS_IPV6_PREFIX: %w", err)
		}
	}
	legacy := &Host{Name: Default, Region: cfg.Region, Node: cfg.PVENode, Storage: cfg.PVEStorage, Disk: cfg.PVEDisk, Bridge: cfg.PVEBridge,
		DisableKVM: cfg.PVEDisableKVM, IPv6Prefix: v6, IPv6Gateway: cfg.IPv6Gateway, Nameservers: cfg.Nameservers}
	if cfg.HostsFile == "" {
		if cfg.PVEURL == "" {
			log.Warn("XENOS_PVE_URL unset: using in-memory fake Proxmox, no real VMs will be created")
			legacy.API = proxmox.NewFake()
		} else {
			legacy.API = proxmox.New(cfg.PVEURL, cfg.PVENode, cfg.PVETokenID, cfg.PVETokenSecret, cfg.PVEInsecureTLS)
		}
		return NewSet(legacy), nil
	}
	raw, err := os.ReadFile(cfg.HostsFile)
	if err != nil {
		return nil, fmt.Errorf("XENOS_HOSTS_FILE: %w", err)
	}
	var f struct {
		Hosts []fileEntry `yaml:"hosts"`
	}
	dec := yaml.NewDecoder(bytesReader(raw))
	dec.KnownFields(true) // a misspelt key must not silently fall back to a default
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("XENOS_HOSTS_FILE: %w", err)
	}
	if len(f.Hosts) == 0 {
		return nil, errors.New("XENOS_HOSTS_FILE lists no hosts")
	}
	var out []*Host
	seen := map[string]bool{}
	for i, e := range f.Hosts {
		if !nameRe.MatchString(e.Name) {
			return nil, fmt.Errorf("hosts[%d]: name %q must be lowercase letters, digits and hyphens", i, e.Name)
		}
		if seen[e.Name] {
			return nil, fmt.Errorf("host %q is listed twice", e.Name)
		}
		seen[e.Name] = true
		if e.URL == "" || e.Node == "" || e.TokenID == "" || e.TokenSecret == "" {
			return nil, fmt.Errorf("host %q: url, node, token_id and token_secret are required", e.Name)
		}
		h := &Host{Name: e.Name, Region: orDefault(e.Region, cfg.Region), Node: e.Node, Storage: orDefault(e.Storage, cfg.PVEStorage),
			Disk: orDefault(e.Disk, cfg.PVEDisk), Bridge: orDefault(e.Bridge, cfg.PVEBridge), DisableKVM: e.DisableKVM,
			IPv6Gateway: e.IPv6Gateway, Nameservers: orDefault(e.Nameservers, cfg.Nameservers)}
		if e.IPv6Prefix != "" {
			if h.IPv6Prefix, err = netip.ParsePrefix(e.IPv6Prefix); err != nil {
				return nil, fmt.Errorf("host %q: ipv6_prefix: %w", e.Name, err)
			}
		}
		h.API = proxmox.New(e.URL, e.Node, e.TokenID, e.TokenSecret, e.InsecureTLS)
		out = append(out, h)
	}
	return NewSet(out...), nil
}

func orDefault(v, d string) string {
	if v != "" {
		return v
	}
	return d
}

// Names lists the hosts in name order.
func (s *Set) Names() []string { return append([]string(nil), s.order...) }

// Get returns a host by name.
func (s *Set) Get(name string) (*Host, bool) { h, ok := s.byName[name]; return h, ok }

// All returns the hosts in name order.
func (s *Set) All() []*Host {
	out := make([]*Host, 0, len(s.order))
	for _, n := range s.order {
		out = append(out, s.byName[n])
	}
	return out
}

// Primary is the first host in name order, used by the few calls that have no VMID.
func (s *Set) Primary() *Host { return s.byName[s.order[0]] }

// Remember records which host holds a VMID (called when a VM is placed, before its first operation).
func (s *Set) Remember(vmid int, host string) {
	s.mu.Lock()
	s.cache[vmid] = host
	s.mu.Unlock()
}

// For returns the API of the host holding vmid.
func (s *Set) For(ctx context.Context, vmid int) (proxmox.API, error) {
	if len(s.order) == 1 {
		return s.byName[s.order[0]].API, nil
	}
	s.mu.Lock()
	name, ok := s.cache[vmid]
	s.mu.Unlock()
	if !ok {
		if s.Resolve == nil {
			return nil, fmt.Errorf("no host is known for vmid %d", vmid)
		}
		var err error
		if name, err = s.Resolve(ctx, vmid); err != nil {
			return nil, fmt.Errorf("find the host of vmid %d: %w", vmid, err)
		}
		s.Remember(vmid, name)
	}
	h, ok := s.byName[name]
	if !ok {
		return nil, fmt.Errorf("vmid %d is on host %q, which is not in the hosts file", vmid, name)
	}
	return h.API, nil
}

// byUPID picks the host whose node name is inside a Proxmox task id ("UPID:<node>:<pid>:..."); anything else
// (the fakes' ids, a node this file does not know) falls back to the primary host.
func (s *Set) byUPID(upid string) proxmox.API {
	parts := strings.SplitN(upid, ":", 3)
	if len(parts) >= 2 && parts[0] == "UPID" {
		for _, n := range s.order {
			if h := s.byName[n]; h.Node == parts[1] && h.Node != "" {
				return h.API
			}
		}
	}
	return s.Primary().API
}

// Attach connects the set to the database: every configured host gets a row (its status is operator state and
// is never overwritten), and a VMID is looked up from the VM that owns it.
func (s *Set) Attach(ctx context.Context, st *store.Store) error {
	for _, n := range s.order {
		if err := st.Q.SyncHost(ctx, n); err != nil {
			return err
		}
	}
	s.Resolve = func(ctx context.Context, vmid int) (string, error) {
		return st.Q.HostOfVMID(ctx, pgtype.Int4{Int32: int32(vmid), Valid: true})
	}
	return nil
}
