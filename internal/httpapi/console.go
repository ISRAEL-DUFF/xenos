package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenos/internal/proxmox"
	"github.com/israel-duff/xenos/internal/store/db"
)

// Browser console: the dashboard opens a VNC session to the customer's own running VM through this server.
// The Proxmox host is never exposed: this process holds the host token, asks for a one-time ticket for the
// customer's VM, and bridges the browser's websocket to the host's. A browser needs two steps: a POST (CSRF
// protected) that returns a one-time session id, then the websocket that presents it.
var (
	consoleSessionTTL = 30 * time.Second
	consoleIdleLimit  = 15 * time.Minute
	consoleMaxLength  = 4 * time.Hour
	consoleRecheck    = 30 * time.Second
)

const maxConsolesPerUser = 2

type consoleSession struct {
	userID  int64
	vmID    int64
	vmid    int
	ticket  proxmox.ConsoleTicket
	expires time.Time
}

type consoleStore struct {
	mu       sync.Mutex
	sessions map[string]consoleSession
	active   map[int64]int // open consoles per user
}

func (c *consoleStore) put(cs consoleSession) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sessions == nil {
		c.sessions = map[string]consoleSession{}
	}
	now := time.Now()
	for k, v := range c.sessions { // drop what was never used
		if now.After(v.expires) {
			delete(c.sessions, k)
		}
	}
	c.sessions[id] = cs
	return id, nil
}

// take consumes a session id: it works once, for the user and VM it was issued to, within its lifetime.
func (c *consoleStore) take(id string, userID, vmID int64) (consoleSession, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cs, ok := c.sessions[id]
	if !ok {
		return consoleSession{}, false
	}
	delete(c.sessions, id)
	if time.Now().After(cs.expires) || cs.userID != userID || cs.vmID != vmID {
		return consoleSession{}, false
	}
	return cs, true
}

func (c *consoleStore) open(userID int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active == nil {
		c.active = map[int64]int{}
	}
	if c.active[userID] >= maxConsolesPerUser {
		return false
	}
	c.active[userID]++
	return true
}

func (c *consoleStore) closed(userID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active[userID]--; c.active[userID] <= 0 {
		delete(c.active, userID)
	}
}

// createConsole prepares a console for a running VM the caller owns and returns the one-time session id
// and the VNC password the browser's VNC client must present.
func (s *Server) createConsole(w http.ResponseWriter, r *http.Request) {
	v, ok := s.ownedVM(w, r)
	if !ok {
		return
	}
	if s.PVE == nil {
		writeErr(w, http.StatusServiceUnavailable, "the console is not available")
		return
	}
	ctx := r.Context()
	user := principalFrom(ctx).User
	if !s.consoleLimit.Allow(strconvID(user.ID)) {
		writeErr(w, http.StatusTooManyRequests, "too many console requests, try again in a minute")
		return
	}
	if v.State != "running" {
		writeErr(w, http.StatusConflict, "the console is available while the VM is running")
		return
	}
	if v.Busy.Valid {
		writeErr(w, http.StatusConflict, "the VM is busy: try again in a moment")
		return
	}
	work, err := s.Store.Q.GetVMForWork(ctx, v.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if work.UserID != user.ID {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	ticket, err := s.PVE.Console(ctx, int(work.ProxmoxVmid.Int32))
	if err != nil {
		s.Log.Error("console ticket", "vm_id", v.ID, "err", err)
		writeErr(w, http.StatusBadGateway, "could not open a console on the host right now")
		return
	}
	id, err := s.consoles.put(consoleSession{userID: user.ID, vmID: v.ID, vmid: int(work.ProxmoxVmid.Int32), ticket: ticket, expires: time.Now().Add(consoleSessionTTL)})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": id, "password": ticket.Ticket})
}

// consoleSocket bridges the browser to the host's VNC websocket until either side closes, the console sits
// idle or runs too long, or the account or VM stops qualifying.
func (s *Server) consoleSocket(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	user := principalFrom(ctx).User
	cs, ok := s.consoles.take(r.URL.Query().Get("session"), user.ID, id)
	if !ok {
		writeErr(w, http.StatusForbidden, "the console session is missing, used or expired")
		return
	}
	if s.PVE == nil {
		writeErr(w, http.StatusServiceUnavailable, "the console is not available")
		return
	}
	if !s.consoles.open(user.ID) {
		writeErr(w, http.StatusTooManyRequests, "too many consoles are open")
		return
	}
	defer s.consoles.closed(user.ID)
	s.Metrics.ConsoleOpened()
	defer s.Metrics.ConsoleClosed()

	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	upstream, err := s.PVE.DialConsole(dctx, cs.vmid, cs.ticket)
	cancel()
	if err != nil {
		s.Log.Error("console dial", "vm_id", id, "err", err)
		writeErr(w, http.StatusBadGateway, "could not reach the VM console")
		return
	}
	defer upstream.Close()

	up := websocket.Upgrader{
		Subprotocols: []string{"binary"},
		CheckOrigin:  s.sameOrigin,
	}
	browser, err := up.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade has already replied
	}
	defer browser.Close()
	browser.SetReadLimit(1 << 20)
	s.Log.Info("console opened", "user_id", user.ID, "vm_id", id)
	defer s.Log.Info("console closed", "user_id", user.ID, "vm_id", id)

	var lastActive atomic.Int64
	lastActive.Store(time.Now().UnixNano())
	done := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(done); browser.Close(); upstream.Close() }) }

	go func() { // browser -> guest
		defer stop()
		for {
			mt, msg, err := browser.ReadMessage()
			if err != nil {
				return
			}
			if mt != websocket.BinaryMessage {
				continue
			}
			lastActive.Store(time.Now().UnixNano())
			if upstream.WriteMessage(msg) != nil {
				return
			}
		}
	}()
	go func() { // guest -> browser
		defer stop()
		for {
			msg, err := upstream.ReadMessage()
			if err != nil {
				return
			}
			lastActive.Store(time.Now().UnixNano())
			if browser.WriteMessage(websocket.BinaryMessage, msg) != nil {
				return
			}
		}
	}()

	started := time.Now()
	tick := time.NewTicker(consoleRecheck)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-tick.C:
			idle := time.Since(time.Unix(0, lastActive.Load()))
			if idle > consoleIdleLimit || time.Since(started) > consoleMaxLength || !s.stillEntitled(user.ID, id) {
				stop()
				return
			}
		}
	}
}

// stillEntitled re-checks that the account is active and the VM is still the owner's and running.
func (s *Server) stillEntitled(userID, vmID int64) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	u, err := s.Store.Q.GetUserByID(ctx, userID)
	if err != nil || u.Status != "active" {
		return false
	}
	v, err := s.Store.Q.GetUserVM(ctx, db.GetUserVMParams{ID: vmID, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) || err != nil {
		return false
	}
	return v.State == "running"
}

// sameOrigin allows a websocket only from our own site, so another website cannot open a customer's console
// with the customer's cookies. A missing Origin (not a browser) is refused too.
func (s *Server) sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return false
	}
	u, err := url.Parse(o)
	if err != nil {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	pub, err := url.Parse(s.Cfg.PublicURL)
	return err == nil && strings.EqualFold(u.Host, pub.Host)
}

func strconvID(id int64) string { return strconv.FormatInt(id, 10) }
