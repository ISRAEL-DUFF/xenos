package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/israel-duff/xenos/internal/hosts"
	"github.com/israel-duff/xenos/internal/store/db"
)

const hostHealthTTL = 30 * time.Second

var spreadGroupRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)

type hostHealth struct {
	at       time.Time
	ok       bool
	memTotal int64
}

// healthCache remembers for a short while whether a host answered, so creating VMs does not call every host
// on every request.
type healthCache struct {
	mu sync.Mutex
	m  map[string]hostHealth
}

func (c *healthCache) get(ctx context.Context, h *hosts.Host, now time.Time) hostHealth {
	c.mu.Lock()
	cur, ok := c.m[h.Name]
	c.mu.Unlock()
	if ok && now.Sub(cur.at) < hostHealthTTL {
		return cur
	}
	info, err := h.API.NodeInfo(ctx)
	cur = hostHealth{at: now, ok: err == nil, memTotal: info.MemTotal}
	c.mu.Lock()
	if c.m == nil {
		c.m = map[string]hostHealth{}
	}
	c.m[h.Name] = cur
	c.mu.Unlock()
	return cur
}

// placeRequest is what a new VM needs from a host.
type placeRequest struct {
	UserID       int64
	TemplateID   int64
	PlanRAMMB    int64
	SpreadGroup  string
	SpreadPrefer bool
	// OnlyHost restricts placement to one host (a private network the VM must join lives there).
	OnlyHost string
}

// placeVM picks the host for a new VM. Eligible hosts are in this region, active, reachable, hold the template, have
// a free IP and have RAM left (committed plus this plan within XENOS_RAM_COMMIT_LIMIT). Among them the one with the
// lowest committed-RAM fraction wins, ties broken by name. VMs of one spread group go on different hosts.
func (s *Server) placeVM(ctx context.Context, req placeRequest) (string, *apiError) {
	if s.Hosts == nil {
		return hosts.Default, nil // single-host installs without a hosts set (tests)
	}
	q := s.Store.Q
	status := map[string]string{}
	rows, err := q.ListHosts(ctx)
	if err != nil {
		return "", &apiError{http.StatusInternalServerError, "internal error"}
	}
	for _, r := range rows {
		status[r.Name] = r.Status
	}
	committed := map[string]int64{}
	crow, err := q.HostCommittedRAM(ctx)
	if err != nil {
		return "", &apiError{http.StatusInternalServerError, "internal error"}
	}
	for _, r := range crow {
		committed[r.Host] = r.RamMb
	}
	free := map[string]int64{}
	frow, err := q.HostFreeIPs(ctx)
	if err != nil {
		return "", &apiError{http.StatusInternalServerError, "internal error"}
	}
	for _, r := range frow {
		free[r.Host] = int64(r.Free)
	}
	used := map[string]bool{}
	if req.SpreadGroup != "" {
		hs, err := q.SpreadGroupHosts(ctx, db.SpreadGroupHostsParams{UserID: req.UserID, SpreadGroup: textOf(req.SpreadGroup)})
		if err != nil {
			return "", &apiError{http.StatusInternalServerError, "internal error"}
		}
		for _, h := range hs {
			used[h] = true
		}
	}

	type cand struct {
		name string
		frac float64
	}
	var all, apart []cand
	limit := s.Cfg.RAMCommitLimit
	if limit <= 0 {
		limit = 1
	}
	now := time.Now()
	for _, h := range s.Hosts.All() {
		if req.OnlyHost != "" && h.Name != req.OnlyHost {
			continue
		}
		if h.Region != s.Cfg.Region || status[h.Name] != "active" || free[h.Name] < 1 {
			continue
		}
		if has, err := q.HostHasTemplate(ctx, db.HostHasTemplateParams{Host: h.Name, TemplateID: req.TemplateID}); err != nil || !has {
			continue
		}
		health := s.health.get(ctx, h, now)
		if !health.ok || health.memTotal <= 0 {
			continue
		}
		after := float64((committed[h.Name]+req.PlanRAMMB)<<20) / float64(max(health.memTotal, 1))
		if health.memTotal > 0 && after > limit {
			continue
		}
		c := cand{h.Name, float64(committed[h.Name]<<20) / float64(max(health.memTotal, 1))}
		all = append(all, c)
		if !used[h.Name] {
			apart = append(apart, c)
		}
	}
	pool := all
	if req.SpreadGroup != "" {
		pool = apart
		if len(pool) == 0 && req.SpreadPrefer {
			pool = all
		}
	}
	if len(pool) == 0 {
		if req.SpreadGroup != "" && len(all) > 0 {
			return "", &apiError{http.StatusConflict, fmt.Sprintf(
				"no host left that does not already hold a VM of spread group %q; delete one, or send \"spread\": \"prefer\" to allow sharing a host", req.SpreadGroup)}
		}
		return "", &apiError{http.StatusServiceUnavailable, "no capacity available right now, try again later"}
	}
	sort.Slice(pool, func(i, j int) bool {
		if pool[i].frac != pool[j].frac {
			return pool[i].frac < pool[j].frac
		}
		return pool[i].name < pool[j].name
	})
	return pool[0].name, nil
}

// spreadGroupOf reads the group from the request, or from the spread.group label.
func spreadGroupOf(group string, labels map[string]string) (string, string) {
	group = strings.TrimSpace(group)
	if group == "" {
		group = labels["spread.group"]
	}
	if group != "" && !spreadGroupRe.MatchString(group) {
		return "", "the spread group is 1-64 characters: letters, digits, dots, colons, dashes and underscores"
	}
	return group, ""
}
