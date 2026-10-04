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

	"github.com/israel-duff/xenos/internal/billing"
	"github.com/israel-duff/xenos/internal/config"
	"github.com/israel-duff/xenos/internal/jobs"
	"github.com/israel-duff/xenos/internal/store"
)

type Server struct {
	Cfg     config.Config
	Store   *store.Store
	Jobs    *jobs.Queue
	ISpend  billing.ISpend
	Log     *slog.Logger
	WebRoot fs.FS
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.RealIP, middleware.Recoverer)
	r.Use(s.logRequests)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })

	r.Route("/v1", func(r chi.Router) {
		// Public catalogue.
		r.Get("/plans", s.listPlans)
		r.Get("/templates", s.listTemplates)

		// Not yet implemented; see "VPS V1 Weekend Build Plan.md" Phase 2/3.
		for _, route := range []struct{ method, path string }{
			{"POST", "/auth/signup"}, {"POST", "/auth/login"}, {"POST", "/auth/verify"},
			{"GET", "/ssh-keys"}, {"POST", "/ssh-keys"}, {"DELETE", "/ssh-keys/{id}"},
			{"POST", "/vms"}, {"GET", "/vms"}, {"GET", "/vms/{id}"}, {"DELETE", "/vms/{id}"},
			{"POST", "/vms/{id}/reboot"}, {"POST", "/vms/{id}/stop"}, {"POST", "/vms/{id}/start"},
			{"GET", "/wallet"}, {"POST", "/wallet/convert"}, {"POST", "/webhooks/ispend"},
		} {
			r.MethodFunc(route.method, route.path, notImplemented)
		}
	})

	r.NotFound(s.spa)
	return r
}

func notImplemented(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "not implemented"})
}

func (s *Server) listPlans(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Store.Pool.Query(r.Context(),
		`SELECT id, slug, vcpu, ram_mb, disk_gb, price_uusdt_hourly, price_uusdt_monthly_cap
		 FROM plans WHERE active ORDER BY price_uusdt_hourly`)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer rows.Close()
	type plan struct {
		ID           int64  `json:"id"`
		Slug         string `json:"slug"`
		VCPU         int    `json:"vcpu"`
		RAMMB        int    `json:"ram_mb"`
		DiskGB       int    `json:"disk_gb"`
		HourlyUUSDT  int64  `json:"price_uusdt_hourly"`
		MonthlyUUSDT int64  `json:"price_uusdt_monthly_cap"`
	}
	out := []plan{}
	for rows.Next() {
		var p plan
		if err := rows.Scan(&p.ID, &p.Slug, &p.VCPU, &p.RAMMB, &p.DiskGB, &p.HourlyUUSDT, &p.MonthlyUUSDT); err != nil {
			s.fail(w, r, err)
			return
		}
		out = append(out, p)
	}
	writeJSON(w, 200, out)
}

func (s *Server) listTemplates(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Store.Pool.Query(r.Context(), `SELECT id, slug, name FROM templates WHERE active ORDER BY id`)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer rows.Close()
	type tpl struct {
		ID   int64  `json:"id"`
		Slug string `json:"slug"`
		Name string `json:"name"`
	}
	out := []tpl{}
	for rows.Next() {
		var t tpl
		if err := rows.Scan(&t.ID, &t.Slug, &t.Name); err != nil {
			s.fail(w, r, err)
			return
		}
		out = append(out, t)
	}
	writeJSON(w, 200, out)
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
