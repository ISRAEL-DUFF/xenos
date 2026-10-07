// Package httpapi wires the HTTP surface: /v1 JSON API, webhooks and the embedded dashboard.
package httpapi

import (
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/israel-duff/xenos/internal/alert"
	"github.com/israel-duff/xenos/internal/auth"
	"github.com/israel-duff/xenos/internal/billing"
	"github.com/israel-duff/xenos/internal/config"
	"github.com/israel-duff/xenos/internal/hosts"
	"github.com/israel-duff/xenos/internal/jobs"
	"github.com/israel-duff/xenos/internal/mail"
	"github.com/israel-duff/xenos/internal/metrics"
	"github.com/israel-duff/xenos/internal/proxmox"
	"github.com/israel-duff/xenos/internal/store"
	"github.com/israel-duff/xenos/internal/wallet"
)

type Server struct {
	Cfg     config.Config
	Store   *store.Store
	Jobs    *jobs.Queue
	ISpend  billing.ISpend
	Log     *slog.Logger
	WebRoot fs.FS
	Mailer  mail.Mailer
	Wallet  *wallet.Service
	Cache   *billing.BalanceCache
	Metrics *metrics.Metrics // optional; nil records nothing
	health  healthCache
	Hosts   *hosts.Set  // the Proxmox hosts (placement, capacity); nil in tests that only need PVE
	PVE     proxmox.API // optional; the admin capacity view reports the host as unreachable without it

	consoles     consoleStore
	consoleLimit *auth.Limiter
	rebuildLimit *auth.Limiter
	tokenLimit   *auth.Limiter
	exportLimit  *auth.Limiter
	closeLimit   *auth.Limiter

	signupLimit, loginIPLimit, loginAcctLimit, resendLimit, resetLimit, resetTokenLimit, webhookFailLimit *auth.Limiter
}

// NewServer builds a Server with its rate limiters (signup 3/hour per IP per the plan).
func NewServer(cfg config.Config, st *store.Store, q *jobs.Queue, is billing.ISpend, m mail.Mailer, log *slog.Logger, webRoot fs.FS) *Server {
	cache := billing.NewBalanceCache(is, 60*time.Second)
	s := &Server{Cfg: cfg, Store: st, Jobs: q, ISpend: is, Mailer: m, Log: log, WebRoot: webRoot, Cache: cache,
		Wallet: &wallet.Service{Store: st, ISpend: is, Cache: cache, Log: log, Alerter: &alert.Notifier{Store: st, Log: log,
			TelegramToken: cfg.TelegramBotToken, TelegramChat: cfg.TelegramChatID, Mailer: m, ToEmail: cfg.AlertEmail}}}
	// Each limiter counts its refusals under its own name; s.Metrics may be set after construction.
	lim := func(name string, max int, window time.Duration) *auth.Limiter {
		l := auth.NewLimiter(max, window)
		l.OnDeny = func() { s.Metrics.RateLimited(name) }
		return l
	}
	s.signupLimit = lim("signup", 3, time.Hour)
	s.loginIPLimit = lim("login_ip", 30, 15*time.Minute)
	s.loginAcctLimit = lim("login_account", 8, 15*time.Minute)
	s.resendLimit = lim("resend_verification", 3, time.Hour)
	s.webhookFailLimit = lim("webhook_failures", 30, time.Minute)
	s.consoleLimit = lim("console", 10, time.Minute)
	s.rebuildLimit = lim("rebuild", 5, time.Hour)
	s.tokenLimit = lim("api_token", 600, time.Minute)
	s.exportLimit = lim("export", 3, 24*time.Hour)
	s.closeLimit = lim("close_account", 5, time.Hour)
	s.resetLimit = lim("forgot_password", 5, time.Hour)
	s.resetTokenLimit = lim("reset_password", 20, 15*time.Minute)
	return s
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Recoverer, s.Metrics.HTTP)
	r.Use(s.logRequests, s.securityHeaders)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })

	// Liveness: the process is up. Readiness: it can reach Postgres and the worker is ticking.
	r.Get("/readyz", s.readyz)

	r.Route("/v1", func(r chi.Router) {
		r.Use(s.authenticate)

		// Public.
		r.Get("/plans", s.listPlans)
		r.Get("/templates", s.listTemplates)
		r.Post("/auth/signup", s.signup)
		r.Post("/auth/login", s.login)
		r.Post("/auth/verify", s.verifyEmail)
		r.Post("/auth/forgot-password", s.forgotPassword)
		r.Post("/auth/reset-password", s.resetPassword)

		// Authenticated (cookie sessions also need X-CSRF-Token on unsafe methods).
		r.Group(func(r chi.Router) {
			r.Use(s.requireAuth)
			r.Get("/auth/me", s.me)
			r.With(s.requireSession).Post("/auth/logout", s.logout)
			r.With(s.requireSession).Post("/auth/resend-verification", s.resendVerification)
			r.With(s.requireSession).Post("/auth/change-password", s.changePassword)

			// Export and closure need a person: not an API token, and the password again.
			r.With(s.requireSession).Post("/account/export", s.exportAccount)
			r.With(s.requireSession).Post("/account/close", s.closeAccount)

			// API tokens are managed by a signed-in person only.
			r.With(s.requireSession).Get("/tokens", s.listTokens)
			r.With(s.requireSession, s.requireVerified).Post("/tokens", s.createToken)
			r.With(s.requireSession).Delete("/tokens/{id}", s.deleteToken)

			r.Get("/ssh-keys", s.listSSHKeys)
			r.With(s.requireActive).Post("/ssh-keys", s.createSSHKey)
			r.Delete("/ssh-keys/{id}", s.deleteSSHKey)

			r.Post("/vms", s.createVM)
			r.Get("/vms", s.listVMs)
			r.Get("/vms/{id}", s.getVM)
			r.Patch("/vms/{id}", s.patchVM)
			r.Delete("/vms/{id}", s.deleteVM)
			r.With(s.requireActive).Post("/vms/{id}/start", s.powerAction("start", "stopped"))
			r.Post("/vms/{id}/stop", s.powerAction("stop", "running"))
			r.With(s.requireActive).Post("/vms/{id}/reboot", s.powerAction("reboot", "running"))

			r.With(s.requireSession, s.requireActive).Post("/vms/{id}/console", s.createConsole)
			r.With(s.requireSession).Get("/vms/{id}/console/ws", s.consoleSocket)
			r.With(s.requireActive).Post("/vms/{id}/resize", s.resizeVM)
			r.With(s.requireActive).Post("/vms/{id}/rebuild", s.rebuildVM)
			r.Get("/vms/{id}/snapshots", s.listSnapshots)
			r.With(s.requireActive).Post("/vms/{id}/snapshots", s.createSnapshot)
			r.Delete("/vms/{id}/snapshots/{sid}", s.deleteSnapshot)
			r.With(s.requireActive).Post("/vms/{id}/snapshots/{sid}/restore", s.restoreSnapshot)

			r.Get("/networks", s.listNetworks)
			r.With(s.requireActive, s.requireVerified).Post("/networks", s.createNetwork)
			r.Get("/networks/{id}", s.getNetwork)
			r.Delete("/networks/{id}", s.deleteNetwork)
			r.With(s.requireActive).Post("/vms/{id}/networks/{nid}", s.attachNetwork)
			r.With(s.requireActive).Delete("/vms/{id}/networks/{nid}", s.detachNetwork)

			r.Get("/floating-ips", s.listFloatingIPs)
			r.With(s.requireActive).Post("/floating-ips", s.allocateFloatingIP)
			r.Get("/floating-ips/{id}", s.getFloatingIP)
			r.With(s.requireActive).Post("/floating-ips/{id}/attach", s.attachFloatingIP)
			r.With(s.requireActive).Post("/floating-ips/{id}/detach", s.detachFloatingIP)
			r.Delete("/floating-ips/{id}", s.releaseFloatingIP)

			r.Get("/statements", s.listStatements)
			r.Get("/statements/{month}", s.getStatement)

			r.Get("/wallet", s.getWallet)
			r.With(s.requireSession).Patch("/wallet/settings", s.walletSettings)
			// Starting a top-up or conversion needs a verified email.
			r.With(s.requireSession, s.requireActive, s.requireVerified).Post("/wallet/convert", s.convert)
		})

		// Operator area: every route requires an admin account.
		r.Route("/admin", func(r chi.Router) {
			r.Use(s.requireAuth, s.requireSession, s.requireAdmin)
			r.Get("/users", s.adminListUsers)
			r.Get("/users/{id}", s.adminGetUser)
			r.Patch("/users/{id}", s.adminUpdateUser)
			r.Post("/users/{id}/adjustments", s.adminAdjust)
			r.Get("/vms", s.adminListVMs)
			r.Post("/vms/{id}/stop", s.adminVMAction("stop"))
			r.Delete("/vms/{id}", s.adminVMAction("delete"))
			r.Post("/vms/{id}/port25", s.adminPort25)
			r.Get("/capacity", s.adminCapacity)
			r.Get("/jobs", s.adminListJobs)
			r.Post("/jobs/{id}/retry", s.adminRetryJob)
			r.Get("/revenue", s.adminRevenue)
			r.Get("/plans", s.adminListPlans)
			r.Post("/plans", s.adminAddPlan)
			r.Post("/plans/{slug}/price", s.adminSetPlanPrice)
			r.Post("/plans/{slug}/active", s.adminSetPlanActive)
			r.Get("/templates", s.adminListTemplates)
			r.Post("/templates", s.adminAddTemplate)
			r.Post("/templates/{slug}/active", s.adminSetTemplateActive)
		})

		// Authenticated by webhook signature instead of a session (Phase 3).
		r.Post("/webhooks/ispend", s.ispendWebhook)
	})

	r.NotFound(s.spa)
	return r
}

func (s *Server) listPlans(w http.ResponseWriter, r *http.Request) {
	plans, err := s.Store.Q.ListActivePlans(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, 200, plans)
}

func (s *Server) listTemplates(w http.ResponseWriter, r *http.Request) {
	tpls, err := s.Store.Q.ListActiveTemplates(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, 200, tpls)
}

// spa serves the embedded dashboard, falling back to index.html for client-side routes.
func (s *Server) spa(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/v1/") {
		writeJSON(w, 404, map[string]string{"error": "not found"})
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/")
	if _, err := fs.Stat(s.WebRoot, p); p == "" || err != nil {
		p = "index.html"
	}
	f, err := s.WebRoot.Open(p)
	if err != nil {
		http.Error(w, "dashboard not built: run `make web`", http.StatusNotFound)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	if rs, ok := f.(interface {
		Read([]byte) (int, error)
		Seek(int64, int) (int64, error)
	}); ok {
		http.ServeContent(w, r, st.Name(), st.ModTime(), rs)
		return
	}
	http.ServeFileFS(w, r, s.WebRoot, p)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		start := time.Now()
		next.ServeHTTP(ww, r)
		s.Log.Info("http", "request_id", middleware.GetReqID(r.Context()), "method", r.Method,
			"path", r.URL.Path, "status", ww.Status(), "dur", time.Since(start))
	})
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	s.Log.Error("request failed", "request_id", middleware.GetReqID(r.Context()), "err", err)
	writeJSON(w, 500, map[string]string{"error": "internal error"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// workerStaleAfter is how long the worker may go without a heartbeat before /readyz fails.
const workerStaleAfter = 3 * time.Minute

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := s.Store.Pool.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "database unreachable"})
		return
	}
	at, err := s.Store.Q.GetHeartbeat(ctx, "worker")
	if err != nil || time.Since(at) > workerStaleAfter {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "worker not running"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// securityHeaders sets conservative defaults. The dashboard is a same-origin
// bundle with no inline scripts or styles, so a strict CSP fits.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	// The browser console is a same-origin websocket. Some browsers do not treat 'self' as covering ws:
	// and wss:, so name our own origin explicitly.
	connect := "'self'"
	if u, err := url.Parse(s.Cfg.PublicURL); err == nil && u.Host != "" {
		connect += " " + map[string]string{"https": "wss", "http": "ws"}[u.Scheme] + "://" + u.Host
	}
	csp := "default-src 'self'; connect-src " + connect + "; frame-ancestors 'none'; base-uri 'self'; form-action 'self'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Content-Security-Policy", csp)
		next.ServeHTTP(w, r)
	})
}
