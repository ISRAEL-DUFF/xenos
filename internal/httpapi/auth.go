package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/israel-duff/xenos/internal/auth"
	"github.com/israel-duff/xenos/internal/billing"
	"github.com/israel-duff/xenos/internal/store/db"
)

const (
	cookieSessionTTL = 30 * 24 * time.Hour
	bearerSessionTTL = 90 * 24 * time.Hour
	verifyTTL        = 24 * time.Hour
	resetTTL         = time.Hour
	minPassword      = 10
	maxPassword      = 128
)

var phoneRe = regexp.MustCompile(`^\+?[0-9]{7,15}$`)

type userJSON struct {
	ID            int64  `json:"id"`
	Email         string `json:"email"`
	Phone         string `json:"phone"`
	EmailVerified bool   `json:"email_verified"`
	IsAdmin       bool   `json:"is_admin"`
	Status        string `json:"status"`
	VMLimit       int32  `json:"vm_limit"`
	AutoConvert   bool   `json:"auto_convert"`
}

func toUserJSON(u db.User) userJSON {
	return userJSON{ID: u.ID, Email: u.Email, Phone: u.Phone, EmailVerified: u.EmailVerifiedAt.Valid,
		IsAdmin: u.IsAdmin, Status: u.Status, VMLimit: u.VmLimit, AutoConvert: u.AutoConvert}
}

type sessionResponse struct {
	User      userJSON `json:"user"`
	CSRFToken string   `json:"csrf_token,omitempty"`
	Token     string   `json:"token,omitempty"` // bearer sessions only
}

func normalizeEmail(raw string) (string, bool) {
	e := strings.ToLower(strings.TrimSpace(raw))
	if len(e) > 254 {
		return "", false
	}
	a, err := mail.ParseAddress(e)
	if err != nil || a.Address != e || !strings.Contains(e[strings.LastIndex(e, "@"):], ".") {
		return "", false
	}
	return e, true
}

// startSession creates a session row and, for cookie sessions, sets the cookie.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u db.User, bearer bool) (sessionResponse, error) {
	token, hash, err := auth.NewToken()
	if err != nil {
		return sessionResponse{}, err
	}
	kind, ttl, csrf := "cookie", cookieSessionTTL, ""
	if bearer {
		kind, ttl = "bearer", bearerSessionTTL
	} else if csrf, _, err = auth.NewToken(); err != nil {
		return sessionResponse{}, err
	}
	err = s.Store.Q.CreateSession(r.Context(), db.CreateSessionParams{
		TokenHash: hash, UserID: u.ID, Kind: kind, CsrfToken: csrf, ExpiresAt: time.Now().Add(ttl)})
	if err != nil {
		return sessionResponse{}, err
	}
	resp := sessionResponse{User: toUserJSON(u), CSRFToken: csrf}
	if bearer {
		resp.Token = token
		return resp, nil
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", MaxAge: int(ttl.Seconds()),
		HttpOnly: true, Secure: s.Cfg.CookieSecure, SameSite: http.SameSiteLaxMode})
	return resp, nil
}

func (s *Server) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.Cfg.CookieSecure, SameSite: http.SameSiteLaxMode})
}

func (s *Server) signup(w http.ResponseWriter, r *http.Request) {
	if !s.signupLimit.Allow(s.clientIP(r)) {
		writeErr(w, http.StatusTooManyRequests, "too many signups from this address, try again later")
		return
	}
	var in struct {
		Email, Password, Phone string
		AcceptAUP              bool `json:"accept_aup"` // acceptable-use policy; required
		Token                  bool // true: return a bearer token instead of setting a cookie
	}
	if !decode(w, r, &in) {
		return
	}
	email, ok := normalizeEmail(in.Email)
	switch {
	case !in.AcceptAUP:
		writeErr(w, http.StatusBadRequest, "you must accept the acceptable-use policy")
		return
	case !ok:
		writeErr(w, http.StatusBadRequest, "invalid email address")
		return
	case len(in.Password) < minPassword || len(in.Password) > maxPassword:
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("password must be %d-%d characters", minPassword, maxPassword))
		return
	case !phoneRe.MatchString(strings.ReplaceAll(in.Phone, " ", "")):
		writeErr(w, http.StatusBadRequest, "invalid phone number")
		return
	}
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	u, err := s.Store.Q.CreateUser(r.Context(), db.CreateUserParams{
		Email: email, PasswordHash: hash, Phone: strings.ReplaceAll(in.Phone, " ", "")})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		writeErr(w, http.StatusConflict, "an account with this email already exists")
		return
	} else if err != nil {
		s.fail(w, r, err)
		return
	}

	if err := s.Store.Q.MarkAUP(r.Context(), db.MarkAUPParams{ID: u.ID, AupAcceptedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}}); err != nil {
		s.Log.Error("record aup acceptance", "user_id", u.ID, "err", err)
	}
	s.linkISpend(r.Context(), &u)
	if err := s.sendVerification(r.Context(), u); err != nil {
		s.Log.Error("send verification", "user_id", u.ID, "err", err)
	}
	resp, err := s.startSession(w, r, u, in.Token)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, resp)
}

// linkISpend creates the customer's iswallet wallet (and virtual account) and remembers them. A
// failure is logged and left for a later retry rather than blocking signup; the idempotency key
// and owner ref make retries safe.
func (s *Server) linkISpend(ctx context.Context, u *db.User) {
	c, err := s.ISpend.CreateCustomer(ctx, "signup:"+u.Email, "user:"+strconv.FormatInt(u.ID, 10), u.Email, u.Phone)
	if err != nil {
		s.Log.Error("iswallet create customer", "user_id", u.ID, "err", err)
		return
	}
	if err := s.Store.Q.SetISpendCustomer(ctx, db.SetISpendCustomerParams{ID: u.ID, IspendCustomerID: textOf(c.ID)}); err != nil {
		s.Log.Error("store iswallet wallet id", "user_id", u.ID, "err", err)
		return
	}
	u.IspendCustomerID = textOf(c.ID)
	s.saveVirtualAccount(ctx, u, c)
}

// saveVirtualAccount keeps the account number locally: iswallet can issue it (idempotently) but
// has no call to read it back.
func (s *Server) saveVirtualAccount(ctx context.Context, u *db.User, c billing.Customer) {
	if c.VirtualAcct == "" {
		return
	}
	if err := s.Store.Q.SetVirtualAccount(ctx, db.SetVirtualAccountParams{ID: u.ID,
		VaBank: textOf(c.VirtualBank), VaAccountNumber: textOf(c.VirtualAcct), VaAccountName: textOf(c.VirtualName)}); err != nil {
		s.Log.Error("store virtual account", "user_id", u.ID, "err", err)
		return
	}
	u.VaBank, u.VaAccountNumber, u.VaAccountName = textOf(c.VirtualBank), textOf(c.VirtualAcct), textOf(c.VirtualName)
}

func (s *Server) sendVerification(ctx context.Context, u db.User) error {
	if err := s.Store.Q.DeleteUserEmailVerifications(ctx, u.ID); err != nil {
		return err
	}
	token, hash, err := auth.NewToken()
	if err != nil {
		return err
	}
	if err := s.Store.Q.CreateEmailVerification(ctx, db.CreateEmailVerificationParams{
		TokenHash: hash, UserID: u.ID, ExpiresAt: time.Now().Add(verifyTTL)}); err != nil {
		return err
	}
	link := fmt.Sprintf("%s/verify-email#token=%s", s.Cfg.PublicURL, token)
	return s.Mailer.Send(ctx, u.Email, "Verify your Xenos email", "Confirm your email address:\n\n"+link+"\n\nThis link expires in 24 hours.")
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if !s.loginIPLimit.Allow(ip) {
		writeErr(w, http.StatusTooManyRequests, "too many attempts, try again later")
		return
	}
	var in struct {
		Email, Password string
		Token           bool
	}
	if !decode(w, r, &in) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if len(email) > 254 { // no real address is longer; do not let junk keys fill the limiter
		writeErr(w, http.StatusUnauthorized, "invalid email or password")
		return
	}
	if !s.loginAcctLimit.Allow(email) {
		writeErr(w, http.StatusTooManyRequests, "too many attempts for this account, try again later")
		return
	}
	u, err := s.Store.Q.GetUserByEmail(r.Context(), email)
	hash := auth.DummyHash
	if err == nil {
		hash = u.PasswordHash
	} else if !errors.Is(err, pgx.ErrNoRows) {
		s.fail(w, r, err)
		return
	}
	ok, _ := auth.CheckPassword(in.Password, hash) // always verify, even for unknown emails
	if err != nil || !ok {
		writeErr(w, http.StatusUnauthorized, "invalid email or password")
		return
	}
	if u.Status == "banned" {
		writeErr(w, http.StatusForbidden, "account disabled")
		return
	}
	s.loginAcctLimit.Reset(email)
	resp, err := s.startSession(w, r, u, in.Token)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if err := s.Store.Q.DeleteSession(r.Context(), p.TokenHash); err != nil {
		s.fail(w, r, err)
		return
	}
	s.clearCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	writeJSON(w, http.StatusOK, sessionResponse{User: toUserJSON(p.User), CSRFToken: p.CSRFToken})
}

func (s *Server) verifyEmail(w http.ResponseWriter, r *http.Request) {
	var in struct{ Token string }
	if !decode(w, r, &in) {
		return
	}
	uid, err := s.Store.Q.ConsumeEmailVerification(r.Context(), auth.HashToken(in.Token))
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusBadRequest, "invalid or expired token")
		return
	} else if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Store.Q.MarkEmailVerified(r.Context(), uid); err != nil {
		s.fail(w, r, err)
		return
	}
	// Naira that arrived before verification was held; convert it now.
	if err := s.Wallet.ConvertHeldOnVerify(r.Context(), uid); err != nil {
		s.Log.Error("convert held deposits", "user_id", uid, "err", err)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "verified"})
}

func (s *Server) resendVerification(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p.User.EmailVerifiedAt.Valid {
		writeErr(w, http.StatusConflict, "email already verified")
		return
	}
	if !s.resendLimit.Allow(fmt.Sprint(p.User.ID)) {
		writeErr(w, http.StatusTooManyRequests, "too many requests, try again later")
		return
	}
	if err := s.sendVerification(r.Context(), p.User); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// forgotPassword always answers 202 so it cannot be used to discover accounts.
func (s *Server) forgotPassword(w http.ResponseWriter, r *http.Request) {
	var in struct{ Email string }
	if !decode(w, r, &in) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if s.resetLimit.Allow(s.clientIP(r)) && s.resetLimit.Allow("acct:"+email) {
		if u, err := s.Store.Q.GetUserByEmail(r.Context(), email); err == nil {
			if err := s.sendReset(r.Context(), u); err != nil {
				s.Log.Error("send reset", "user_id", u.ID, "err", err)
			}
		}
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) sendReset(ctx context.Context, u db.User) error {
	if err := s.Store.Q.DeleteUserPasswordResets(ctx, u.ID); err != nil {
		return err
	}
	token, hash, err := auth.NewToken()
	if err != nil {
		return err
	}
	if err := s.Store.Q.CreatePasswordReset(ctx, db.CreatePasswordResetParams{
		TokenHash: hash, UserID: u.ID, ExpiresAt: time.Now().Add(resetTTL)}); err != nil {
		return err
	}
	link := fmt.Sprintf("%s/reset-password#token=%s", s.Cfg.PublicURL, token)
	return s.Mailer.Send(ctx, u.Email, "Reset your Xenos password", "Reset your password:\n\n"+link+"\n\nThis link expires in 1 hour. If you did not ask for this, ignore this email.")
}

func (s *Server) resetPassword(w http.ResponseWriter, r *http.Request) {
	if !s.resetTokenLimit.Allow(s.clientIP(r)) {
		writeErr(w, http.StatusTooManyRequests, "too many attempts, try again later")
		return
	}
	var in struct{ Token, Password string }
	if !decode(w, r, &in) {
		return
	}
	if len(in.Password) < minPassword || len(in.Password) > maxPassword {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("password must be %d-%d characters", minPassword, maxPassword))
		return
	}
	// Check the token before the expensive hash: a request with a bad token must cost almost nothing.
	uid, err := s.Store.Q.ConsumePasswordReset(r.Context(), auth.HashToken(in.Token))
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusBadRequest, "invalid or expired token")
		return
	} else if err != nil {
		s.fail(w, r, err)
		return
	}
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.setPassword(r.Context(), uid, hash); err != nil {
		s.fail(w, r, err)
		return
	}
	// A reset is how an account is recovered after a compromise, so API tokens do not survive it.
	if err := s.Store.Q.RevokeUserAPITokens(r.Context(), uid); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "password updated"})
}

// setPassword stores the new hash and signs the user out everywhere.
func (s *Server) setPassword(ctx context.Context, uid int64, hash string) error {
	if err := s.Store.Q.UpdatePasswordHash(ctx, db.UpdatePasswordHashParams{ID: uid, PasswordHash: hash}); err != nil {
		return err
	}
	return s.Store.Q.DeleteUserSessions(ctx, uid)
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	var in struct{ Current, New string }
	if !decode(w, r, &in) {
		return
	}
	if len(in.New) < minPassword || len(in.New) > maxPassword {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("password must be %d-%d characters", minPassword, maxPassword))
		return
	}
	if ok, _ := auth.CheckPassword(in.Current, p.User.PasswordHash); !ok {
		writeErr(w, http.StatusUnauthorized, "current password is incorrect")
		return
	}
	hash, err := auth.HashPassword(in.New)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.setPassword(r.Context(), p.User.ID, hash); err != nil {
		s.fail(w, r, err)
		return
	}
	// All sessions (including this one) were revoked; issue a fresh one.
	resp, err := s.startSession(w, r, p.User, p.Kind == "bearer")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
