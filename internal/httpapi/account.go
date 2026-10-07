package httpapi

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/israel-duff/xenos/internal/accounts"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/israel-duff/xenos/internal/auth"
	"github.com/israel-duff/xenos/internal/store/db"
)

// Data export and account closure are for the signed-in person only (requireSession): a leaked API token can
// neither read an account's data in bulk nor close it. Both ask for the password again.

func (s *Server) reauth(w http.ResponseWriter, r *http.Request, password string) bool {
	if ok, _ := auth.CheckPassword(password, principalFrom(r.Context()).User.PasswordHash); !ok {
		writeErr(w, http.StatusUnauthorized, "the password is incorrect")
		return false
	}
	return true
}

// exportAccount streams a zip of everything held about the account (no password hash, no secrets).
func (s *Server) exportAccount(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &in) {
		return
	}
	u := principalFrom(r.Context()).User
	if !s.exportLimit.Allow(strconvID(u.ID)) {
		writeErr(w, http.StatusTooManyRequests, "you can export your data three times a day")
		return
	}
	if !s.reauth(w, r, in.Password) {
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="xenos-data-`+time.Now().UTC().Format("2006-01-02")+`.zip"`)
	if err := accounts.Export(r.Context(), w, s.Store, u); err != nil {
		s.Log.Error("account export", "user_id", u.ID, "err", err) // the download has started; nothing more can be said to the client
	}
}

// closeAccount closes the caller's account, or says what stands in the way.
func (s *Server) closeAccount(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Password  string `json:"password"`
		Email     string `json:"email"`
		DeleteVMs bool   `json:"delete_vms"`
	}
	if !decode(w, r, &in) {
		return
	}
	u := principalFrom(r.Context()).User
	if !s.closeLimit.Allow(strconvID(u.ID)) {
		writeErr(w, http.StatusTooManyRequests, "too many attempts, try again later")
		return
	}
	if !s.reauth(w, r, in.Password) {
		return
	}
	if strings.ToLower(strings.TrimSpace(in.Email)) != strings.ToLower(u.Email) {
		writeErr(w, http.StatusBadRequest, "type your email address to confirm")
		return
	}
	blockers, err := accounts.Close(r.Context(), s.Store, s.Jobs, s.ISpend, u, accounts.CloseOptions{DeleteVMs: in.DeleteVMs})
	if err != nil {
		s.Log.Error("close account", "user_id", u.ID, "err", err)
		writeErr(w, http.StatusServiceUnavailable, "the account could not be closed right now, try again shortly")
		return
	}
	if len(blockers) > 0 {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "the account cannot be closed yet", "blockers": blockers})
		return
	}
	if err := s.Store.Q.InsertAudit(r.Context(), db.InsertAuditParams{Source: pgtype.Text{String: "customer", Valid: true}, Action: "account.close",
		Target: u.Email, Detail: []byte(fmt.Sprintf(`{"delete_vms":%t}`, in.DeleteVMs))}); err != nil {
		s.Log.Error("audit account close", "user_id", u.ID, "err", err)
	}
	purge := time.Now().UTC().AddDate(0, 0, 30)
	_ = s.Mailer.Send(r.Context(), u.Email, "Your Xenos account is closed", fmt.Sprintf(
		"Your Xenos account is closed. You cannot sign in any more.\n\nYour personal data will be removed on %s. Until then, contact support if this was a mistake and we can reopen it. "+
			"Records of past charges are kept, without your personal details, as the law requires.\n", purge.Format("2 January 2006")))
	s.clearCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{"status": "closing", "purge_after": purge})
}
