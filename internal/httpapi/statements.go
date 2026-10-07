package httpapi

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/israel-duff/xenos/internal/statements"
)

var statementLabelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9._/-]{0,61}[a-z0-9])?$`)

// listStatements lists the months that have activity. Read-only, so API tokens may use it.
func (s *Server) listStatements(w http.ResponseWriter, r *http.Request) {
	months, err := s.Store.Q.StatementMonths(r.Context(), principalFrom(r.Context()).User.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if months == nil {
		months = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"months": months})
}

// getStatement returns one UTC month as JSON, or as CSV (one row per charged hour) with ?format=csv.
// ?group_by=label:<key> also totals the VMs by that label's value (cost per PGDock node, role, and so on).
func (s *Server) getStatement(w http.ResponseWriter, r *http.Request) {
	month := chi.URLParam(r, "month")
	if _, _, err := statements.ParseMonth(month); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	uid := principalFrom(r.Context()).User.ID
	if r.URL.Query().Get("format") == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="xenos-statement-`+month+`.csv"`)
		if err := statements.WriteCSV(r.Context(), w, s.Store, uid, month); err != nil {
			s.Log.Error("statement csv", "user_id", uid, "err", err) // headers are sent: nothing more can be said to the client
		}
		return
	}
	label := ""
	if g := r.URL.Query().Get("group_by"); g != "" {
		k, ok := strings.CutPrefix(g, "label:")
		if !ok || !statementLabelRe.MatchString(k) {
			writeErr(w, http.StatusBadRequest, "group_by looks like label:<key>")
			return
		}
		label = k
	}
	st, err := statements.Build(r.Context(), s.Store, uid, month, label)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}
