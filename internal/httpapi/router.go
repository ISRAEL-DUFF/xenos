// Package httpapi wires the HTTP surface: /v1 JSON API, webhooks and the embedded dashboard.
package httpapi

import (
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/israel-duff/xenos/internal/auth"
	"github.com/israel-duff/xenos/internal/billing"
	"github.com/israel-duff/xenos/internal/config"
	"github.com/israel-duff/xenos/internal/jobs"
	"github.com/israel-duff/xenos/internal/mail"
	"github.com/israel-duff/xenos/internal/store"
)

type Server struct {
	Cfg     config.Config
	Store   *store.Store
	Jobs    *jobs.Queue
	ISpend  billing.ISpend
	Log     *slog.Logger
	WebRoot fs.FS
	Mailer  mail.Mailer

	signupLimit, loginIPLimit, loginAcctLimit, resendLimit, resetLimit *auth.Limiter
}

// NewServer builds a Server with its rate limiters (signup 3/hour per IP per the plan).
func NewServer(cfg config.Config, st *store.Store, q *jobs.Queue, is billing.ISpend, m mail.Mailer, log *slog.Logger, webRoot fs.FS) *Server {
	return &Server{Cfg: cfg, Store: st, Jobs: q, ISpend: is, Mailer: m, Log: log, WebRoot: webRoot,
		signupLimit:    auth.NewLimiter(3, time.Hour),
		loginIPLimit:   auth.NewLimiter(30, 15*time.Minute),
		loginAcctLimit: auth.NewLimiter(8, 15*time.Minute),
		resendLimit:    auth.NewLimiter(3, time.Hour),
		resetLimit:     auth.NewLimiter(5, time.Hour),
	}
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Recoverer)
	r.Use(s.logRequests)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })

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
			r.Post("/auth/logout", s.logout)
			r.Post("/auth/resend-verification", s.resendVerification)
			r.Post("/auth/change-password", s.changePassword)

			// Not yet implemented; see "VPS V1 Weekend Build Plan.md" Phase 2/3.
			for _, route := range []struct{ method, path string }{
				{"GET", "/ssh-keys"}, {"POST", "/ssh-keys"}, {"DELETE", "/ssh-keys/{id}"},
				{"POST", "/vms"}, {"GET", "/vms"}, {"GET", "/vms/{id}"}, {"DELETE", "/vms/{id}"},
				{"POST", "/vms/{id}/reboot"}, {"POST", "/vms/{id}/stop"}, {"POST", "/vms/{id}/start"},
				{"GET", "/wallet"}, {"POST", "/wallet/convert"},
			} {
				r.MethodFunc(route.method, route.path, notImplemented)
			}
		})

		// Authenticated by webhook signature instead of a session (Phase 3).
		r.Post("/webhooks/ispend", notImplemented)
	})

	r.NotFound(s.spa)
	return r
}

func notImplemented(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "not implemented"})
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
