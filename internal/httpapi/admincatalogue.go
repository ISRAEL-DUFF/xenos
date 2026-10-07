package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/israel-duff/xenos/internal/catalogue"
)

func (s *Server) catalogue() *catalogue.Service {
	return &catalogue.Service{Store: s.Store, PVE: s.PVE}
}

func adminActor(r *http.Request) catalogue.Actor {
	return catalogue.Actor{AdminID: principalFrom(r.Context()).User.ID}
}

// catalogueErr turns the service's refusals into 400/404 and everything else into a 500.
func (s *Server) catalogueErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, catalogue.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not found")
	case errors.Is(err, catalogue.ErrInvalid):
		writeErr(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), "invalid: "))
	default:
		s.fail(w, r, err)
	}
}

func (s *Server) adminListPlans(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Store.Q.AdminListPlans(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) adminAddPlan(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Slug        string `json:"slug"`
		VCPU        int    `json:"vcpu"`
		RAMMB       int    `json:"ram_mb"`
		DiskGB      int    `json:"disk_gb"`
		HourlyUUSDT int64  `json:"price_uusdt_hourly"`
		CapUUSDT    int64  `json:"price_uusdt_monthly_cap"`
	}
	if !decode(w, r, &in) {
		return
	}
	p, err := s.catalogue().AddPlan(r.Context(), catalogue.PlanInput{Slug: in.Slug, VCPU: in.VCPU, RAMMB: in.RAMMB, DiskGB: in.DiskGB,
		HourlyUUSDT: in.HourlyUUSDT, CapUUSDT: in.CapUUSDT}, adminActor(r))
	if err != nil {
		s.catalogueErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

// adminSetPlanPrice reports the impact of a price change and applies it only when confirm is true.
func (s *Server) adminSetPlanPrice(w http.ResponseWriter, r *http.Request) {
	var in struct {
		HourlyUUSDT int64 `json:"price_uusdt_hourly"`
		CapUUSDT    int64 `json:"price_uusdt_monthly_cap"`
		Confirm     bool  `json:"confirm"`
	}
	if !decode(w, r, &in) {
		return
	}
	im, err := s.catalogue().SetPrice(r.Context(), chi.URLParam(r, "slug"), in.HourlyUUSDT, in.CapUUSDT, in.Confirm, adminActor(r))
	if err != nil {
		s.catalogueErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, im)
}

func (s *Server) adminSetPlanActive(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Active bool `json:"active"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := s.catalogue().SetPlanActive(r.Context(), chi.URLParam(r, "slug"), in.Active, adminActor(r)); err != nil {
		s.catalogueErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"active": in.Active})
}

func (s *Server) adminListTemplates(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Store.Q.AdminListTemplates(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) adminAddTemplate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Slug      string `json:"slug"`
		Name      string `json:"name"`
		VMID      int    `json:"proxmox_template_id"`
		CIUser    string `json:"ci_user"`
		SkipCheck bool   `json:"skip_host_check"`
	}
	if !decode(w, r, &in) {
		return
	}
	t, err := s.catalogue().AddTemplate(r.Context(), catalogue.TemplateInput{Slug: in.Slug, Name: in.Name, VMID: in.VMID, CIUser: in.CIUser, SkipCheck: in.SkipCheck}, adminActor(r))
	if err != nil {
		s.catalogueErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) adminSetTemplateActive(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Active bool `json:"active"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := s.catalogue().SetTemplateActive(r.Context(), chi.URLParam(r, "slug"), in.Active, adminActor(r)); err != nil {
		s.catalogueErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"active": in.Active})
}
