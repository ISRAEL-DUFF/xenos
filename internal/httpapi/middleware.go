package httpapi

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenos/internal/auth"
	"github.com/israel-duff/xenos/internal/store/db"
)

const sessionCookie = "xenos_session"

type ctxKey int

const principalKey ctxKey = 0

// Principal is the authenticated caller attached to the request context.
type Principal struct {
	User      db.User
	TokenHash []byte
	Kind      string // "cookie" or "bearer"
	CSRFToken string
}

func principalFrom(ctx context.Context) *Principal {
	p, _ := ctx.Value(principalKey).(*Principal)
	return p
}

// authenticate resolves the session (bearer header or cookie) if present.
// It never rejects; requireAuth does. Unauthenticated requests pass through.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var token, kind string
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			token, kind = strings.TrimSpace(strings.TrimPrefix(h, "Bearer ")), "bearer"
		} else if c, err := r.Cookie(sessionCookie); err == nil {
			token, kind = c.Value, "cookie"
		}
		if token == "" {
			next.ServeHTTP(w, r)
			return
		}
		row, err := s.Store.Q.GetSessionUser(r.Context(), auth.HashToken(token))
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				s.fail(w, r, err)
				return
			}
			next.ServeHTTP(w, r) // unknown or expired token: treated as anonymous
			return
		}
		// A token is only valid on the transport it was issued for, so a
		// cookie value pasted into a header (or the reverse) is rejected.
		if row.Kind != kind || row.Status == "banned" {
			next.ServeHTTP(w, r)
			return
		}
		p := &Principal{User: userFromRow(row), TokenHash: row.TokenHash, Kind: row.Kind, CSRFToken: row.CsrfToken}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
	})
}

// requireAuth rejects anonymous callers and enforces CSRF on cookie sessions.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := principalFrom(r.Context())
		if p == nil {
			writeErr(w, http.StatusUnauthorized, "authentication required")
			return
		}
		if p.Kind == "cookie" && !safeMethod(r.Method) {
			got := r.Header.Get("X-CSRF-Token")
			if subtle.ConstantTimeCompare([]byte(got), []byte(p.CSRFToken)) != 1 {
				writeErr(w, http.StatusForbidden, "invalid CSRF token")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// requireActive blocks actions that start or spend something for accounts an operator has suspended.
// Reading, stopping and deleting stay allowed so a suspended customer can still see why and stop paying.
func (s *Server) requireActive(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := principalFrom(r.Context()); p != nil && p.User.Status != "active" {
			writeErr(w, http.StatusForbidden, "this account is suspended")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireAdmin must run after requireAuth.
func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := principalFrom(r.Context()); p == nil || !p.User.IsAdmin {
			writeErr(w, http.StatusForbidden, "admin only")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireVerified blocks actions (e.g. first top-up) until the email is verified.
func (s *Server) requireVerified(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := principalFrom(r.Context()); p == nil || !p.User.EmailVerifiedAt.Valid {
			writeErr(w, http.StatusForbidden, "email verification required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

func userFromRow(r db.GetSessionUserRow) db.User {
	return db.User{ID: r.ID, Email: r.Email, PasswordHash: r.PasswordHash, IspendCustomerID: r.IspendCustomerID,
		EmailVerifiedAt: r.EmailVerifiedAt, Phone: r.Phone, Status: r.Status, IsAdmin: r.IsAdmin,
		VmLimit: r.VmLimit, AutoConvert: r.AutoConvert, CreatedAt: r.CreatedAt}
}
