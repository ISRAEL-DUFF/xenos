package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/israel-duff/xenos/internal/jobs"
	"github.com/israel-duff/xenos/internal/store/db"
	"github.com/israel-duff/xenos/internal/vm"
)

// Private networks hold /24s from 10.64.0.0/10 (10.64.0.0 to 10.127.255.255). Addresses .2 to .251 go to VMs
// (250); .1 is left free for a future gateway. There is no routing or NAT: the network is a layer-2 segment.
const (
	privateBase    = 0x0A400000 // 10.64.0.0
	privateBlocks  = 1 << 14    // /24s in a /10
	firstHostOctet = 2
	lastHostOctet  = 251
	maxVMNetworks  = 2
)

var networkNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,39}$`)

type networkJSON struct {
	ID        int64        `json:"id"`
	Name      string       `json:"name"`
	CIDR      string       `json:"cidr"`
	VLAN      int32        `json:"vlan_id"`
	Host      *string      `json:"host"`
	CreatedAt time.Time    `json:"created_at"`
	Members   []memberJSON `json:"members"`
}

type memberJSON struct {
	VMID     int64  `json:"vm_id"`
	Hostname string `json:"hostname"`
	Address  string `json:"address"`
	State    string `json:"state"`
}

type privateIPJSON struct {
	NetworkID int64  `json:"network_id"`
	Name      string `json:"name"`
	Address   string `json:"address"`
	State     string `json:"state"`
}

func cidrOfBlock(n int) string {
	v := uint32(privateBase) + uint32(n)<<8
	return fmt.Sprintf("%d.%d.%d.0/24", v>>24, (v>>16)&0xff, (v>>8)&0xff)
}

func (s *Server) networkView(ctx context.Context, id int64, name, cidr string, vlan int32, host pgtype.Text, at time.Time) (networkJSON, error) {
	n := networkJSON{ID: id, Name: name, CIDR: cidr, VLAN: vlan, CreatedAt: at, Members: []memberJSON{}}
	if host.Valid {
		n.Host = &host.String
	}
	ms, err := s.Store.Q.ListNetworkMembers(ctx, id)
	if err != nil {
		return n, err
	}
	for _, m := range ms {
		n.Members = append(n.Members, memberJSON{m.VmID, m.Hostname, m.Address, m.State})
	}
	return n, nil
}

func (s *Server) listNetworks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := s.Store.Q.ListUserNetworks(ctx, principalFrom(ctx).User.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]networkJSON, 0, len(rows))
	for _, n := range rows {
		v, err := s.networkView(ctx, n.ID, n.Name, n.Cidr, n.VlanID, n.Host, n.CreatedAt)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"networks": out, "limit": s.Cfg.PrivateNetworkLimit, "tunnel": s.Cfg.PrivateNetworkTunnel})
}

func (s *Server) ownedNetwork(w http.ResponseWriter, r *http.Request) (db.GetUserNetworkRow, bool) {
	id, ok := pathID(w, r)
	if !ok {
		return db.GetUserNetworkRow{}, false
	}
	n, err := s.Store.Q.GetUserNetwork(r.Context(), db.GetUserNetworkParams{ID: id, UserID: principalFrom(r.Context()).User.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not found")
		return n, false
	} else if err != nil {
		s.fail(w, r, err)
		return n, false
	}
	return n, true
}

func (s *Server) getNetwork(w http.ResponseWriter, r *http.Request) {
	n, ok := s.ownedNetwork(w, r)
	if !ok {
		return
	}
	v, err := s.networkView(r.Context(), n.ID, n.Name, n.Cidr, n.VlanID, n.Host, n.CreatedAt)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// createNetwork allocates the lowest free /24 and VLAN id. Two creates racing for the same ones collide on the
// unique indexes; the loser simply picks again.
func (s *Server) createNetwork(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := principalFrom(ctx).User
	var in struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &in) {
		return
	}
	name := strings.TrimSpace(in.Name)
	if !networkNameRe.MatchString(name) {
		writeErr(w, http.StatusBadRequest, "the name is 1-40 characters: letters, digits, spaces, dots, dashes and underscores")
		return
	}
	var created db.CreatePrivateNetworkRow
	var apiErr *apiError
	for attempt := 0; attempt < 6; attempt++ {
		apiErr = nil
		err := s.Store.InTx(ctx, func(q *db.Queries, tx pgx.Tx) error {
			if status, err := q.LockUserStatus(ctx, user.ID); err != nil {
				return err
			} else if status != "active" {
				apiErr = &apiError{http.StatusForbidden, "your account is suspended or closing; contact support"}
				return errAbort
			}
			if n, err := q.CountUserNetworks(ctx, user.ID); err != nil {
				return err
			} else if n >= int64(s.Cfg.PrivateNetworkLimit) {
				apiErr = &apiError{http.StatusConflict, fmt.Sprintf("network limit reached (%d)", s.Cfg.PrivateNetworkLimit)}
				return errAbort
			}
			used, err := q.ListNetworkCIDRsAndVLANs(ctx)
			if err != nil {
				return err
			}
			cidrs, vlans := map[string]bool{}, map[int32]bool{}
			for _, u := range used {
				cidrs[u.Cidr], vlans[u.VlanID] = true, true
			}
			block := -1
			for i := 0; i < privateBlocks; i++ {
				if !cidrs[cidrOfBlock(i)] {
					block = i
					break
				}
			}
			vlan := int32(-1)
			for v := s.Cfg.PrivateVLANMin; v <= s.Cfg.PrivateVLANMax; v++ {
				if !vlans[int32(v)] {
					vlan = int32(v)
					break
				}
			}
			if block < 0 || vlan < 0 {
				apiErr = &apiError{http.StatusServiceUnavailable, "no private network capacity left (address space or VLAN ids exhausted)"}
				return errAbort
			}
			created, err = q.CreatePrivateNetwork(ctx, db.CreatePrivateNetworkParams{UserID: user.ID, Name: name, Column3: cidrOfBlock(block), VlanID: vlan})
			return err
		})
		if errors.Is(err, errAbort) && apiErr != nil {
			writeErr(w, apiErr.status, apiErr.msg)
			return
		}
		if err != nil && isUniqueViolation(err) {
			if strings.Contains(err.Error(), "user_id_name") || strings.Contains(err.Error(), "private_networks_user_id") {
				writeErr(w, http.StatusConflict, "you already have a network with that name")
				return
			}
			continue // someone took the same /24 or VLAN id: pick again
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, networkJSON{ID: created.ID, Name: created.Name, CIDR: created.Cidr, VLAN: created.VlanID, CreatedAt: created.CreatedAt, Members: []memberJSON{}})
		return
	}
	writeErr(w, http.StatusServiceUnavailable, "could not allocate a network, try again")
}

func (s *Server) deleteNetwork(w http.ResponseWriter, r *http.Request) {
	n, ok := s.ownedNetwork(w, r)
	if !ok {
		return
	}
	rows, err := s.Store.Q.DeletePrivateNetwork(r.Context(), db.DeletePrivateNetworkParams{ID: n.ID, UserID: principalFrom(r.Context()).User.ID})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if rows == 0 {
		writeErr(w, http.StatusConflict, "the network still has VMs: detach them first")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// joinNetwork records a VM's membership inside a transaction: it locks the network, pins it to the VM's host
// (unless the operator has built a tunnel), enforces two networks per VM and picks the lowest free address and slot.
func (s *Server) joinNetwork(ctx context.Context, q *db.Queries, userID, vmID int64, vmHost string, networkID int64, state string) *apiError {
	n, err := q.LockNetwork(ctx, db.LockNetworkParams{ID: networkID, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return &apiError{http.StatusNotFound, "network not found"}
	} else if err != nil {
		return &apiError{http.StatusInternalServerError, "internal error"}
	}
	if !s.Cfg.PrivateNetworkTunnel {
		if n.Host.Valid && n.Host.String != vmHost {
			return &apiError{http.StatusConflict, fmt.Sprintf("the network lives on host %s: VMs on other hosts cannot join it without a tunnel between hosts", n.Host.String)}
		}
		if !n.Host.Valid {
			if err := q.PinNetworkHost(ctx, db.PinNetworkHostParams{ID: n.ID, Host: textOf(vmHost)}); err != nil {
				return &apiError{http.StatusInternalServerError, "internal error"}
			}
		}
	}
	mine, err := q.ListVMPrivateIPs(ctx, vmID)
	if err != nil {
		return &apiError{http.StatusInternalServerError, "internal error"}
	}
	slotUsed := map[int16]bool{}
	for _, m := range mine {
		if m.NetworkID == networkID {
			return &apiError{http.StatusConflict, "the VM is already on this network"}
		}
		slotUsed[m.Slot] = true
	}
	slot := int16(0)
	for c := int16(1); c <= maxVMNetworks; c++ {
		if !slotUsed[c] {
			slot = c
			break
		}
	}
	if slot == 0 {
		return &apiError{http.StatusConflict, fmt.Sprintf("a VM can be on at most %d private networks", maxVMNetworks)}
	}
	pre, err := netip.ParsePrefix(n.Cidr)
	if err != nil {
		return &apiError{http.StatusInternalServerError, "internal error"}
	}
	inUse, err := q.NetworkAddressesInUse(ctx, networkID)
	if err != nil {
		return &apiError{http.StatusInternalServerError, "internal error"}
	}
	taken := map[string]bool{}
	for _, a := range inUse {
		taken[a] = true
	}
	base := pre.Masked().Addr().As4()
	var addr string
	for o := firstHostOctet; o <= lastHostOctet; o++ {
		cand := netip.AddrFrom4([4]byte{base[0], base[1], base[2], byte(o)}).String()
		if !taken[cand] {
			addr = cand
			break
		}
	}
	if addr == "" {
		return &apiError{http.StatusConflict, "the network is full (250 VMs)"}
	}
	if err := q.AddVMPrivateIP(ctx, db.AddVMPrivateIPParams{VmID: vmID, NetworkID: networkID, Slot: slot, Column4: netip.MustParseAddr(addr), State: state}); err != nil {
		return &apiError{http.StatusInternalServerError, "internal error"}
	}
	return nil
}

func (s *Server) attachNetwork(w http.ResponseWriter, r *http.Request) {
	v, ok := s.ownedVM(w, r)
	if !ok {
		return
	}
	s.changeNetwork(w, r, v, vm.NetAttach)
}

func (s *Server) detachNetwork(w http.ResponseWriter, r *http.Request) {
	v, ok := s.ownedVM(w, r)
	if !ok {
		return
	}
	s.changeNetwork(w, r, v, vm.NetDetach)
}

// changeNetwork claims the VM, records the intent and queues the worker (stop, change the NIC, start).
func (s *Server) changeNetwork(w http.ResponseWriter, r *http.Request, v db.GetUserVMRow, action string) {
	ctx := r.Context()
	userID := principalFrom(ctx).User.ID
	nid, err := parseID(chi.URLParam(r, "nid"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if v.Busy.Valid || !operable(v.State) {
		writeErr(w, http.StatusConflict, busyConflict)
		return
	}
	var apiErr *apiError
	err = s.Store.InTx(ctx, func(q *db.Queries, tx pgx.Tx) error {
		if n, err := q.ClaimVMBusy(ctx, db.ClaimVMBusyParams{ID: v.ID, UserID: userID, Busy: textOf("networking")}); err != nil {
			return err
		} else if n == 0 {
			apiErr = &apiError{http.StatusConflict, busyConflict}
			return errAbort
		}
		if action == vm.NetAttach {
			if e := s.joinNetwork(ctx, q, userID, v.ID, v.Host, nid, "attaching"); e != nil {
				apiErr = e
				return errAbort
			}
		} else {
			if _, err := q.GetUserNetwork(ctx, db.GetUserNetworkParams{ID: nid, UserID: userID}); errors.Is(err, pgx.ErrNoRows) {
				apiErr = &apiError{http.StatusNotFound, "network not found"}
				return errAbort
			} else if err != nil {
				return err
			}
			rows, err := q.ListVMPrivateIPs(ctx, v.ID)
			if err != nil {
				return err
			}
			var cur *db.ListVMPrivateIPsRow
			for i := range rows {
				if rows[i].NetworkID == nid {
					cur = &rows[i]
				}
			}
			if cur == nil || cur.State != "attached" {
				apiErr = &apiError{http.StatusConflict, "the VM is not on this network"}
				return errAbort
			}
			if err := q.SetVMPrivateIPState(ctx, db.SetVMPrivateIPStateParams{VmID: v.ID, NetworkID: nid, State: "detaching"}); err != nil {
				return err
			}
		}
		return jobs.EnqueueTx(ctx, tx, vm.JobNetwork, vm.Payload{VMID: v.ID, NetworkID: nid, Action: action})
	})
	if errors.Is(err, errAbort) && apiErr != nil {
		writeErr(w, apiErr.status, apiErr.msg)
		return
	} else if err != nil {
		s.fail(w, r, err)
		return
	}
	s.respondVM(w, r, v.ID, userID, http.StatusAccepted)
}

func operable(state string) bool { return state == "running" || state == "stopped" }

// privateIPsOf lists a VM's private addresses for its JSON.
func (s *Server) privateIPsOf(ctx context.Context, vmID int64) []privateIPJSON {
	rows, err := s.Store.Q.ListVMPrivateIPs(ctx, vmID)
	if err != nil {
		return nil
	}
	out := make([]privateIPJSON, 0, len(rows))
	for _, r := range rows {
		out = append(out, privateIPJSON{r.NetworkID, r.Name, r.Address, r.State})
	}
	return out
}

func parseID(s string) (int64, error) { return strconv.ParseInt(s, 10, 64) }
