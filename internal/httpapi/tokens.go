package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/israel-duff/xenos/internal/auth"
	"github.com/israel-duff/xenos/internal/store/db"
)

// API tokens are for programs (for example PGDock provisioning nodes). A token acts as its owner for VMs, SSH
// keys and reading the wallet; requireSession keeps everything else for a signed-in person.
const (
	apiTokenPrefix = "xt_"
	maxAPITokens   = 10
	maxTokenDays   = 365
)

func newAPIToken() (token string, hash []byte, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return
	}
	token = apiTokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	return token, auth.HashToken(token), nil
}

// authenticateAPIToken resolves a token. Like authenticate it never rejects by itself: an unknown, expired
// or revoked token leaves the request anonymous.
func (s *Server) authenticateAPIToken(w http.ResponseWriter, r *http.Request, next http.Handler, token string) {
	row, err := s.Store.Q.GetAPITokenUser(r.Context(), auth.HashToken(token))
	if errors.Is(err, pgx.ErrNoRows) {
		next.ServeHTTP(w, r)
		return
	} else if err != nil {
		s.fail(w, r, err)
		return
	}
	if cannotSignIn(row.Status) {
		next.ServeHTTP(w, r)
		return
	}
	if !s.tokenLimit.Allow(strconvID(row.TokenID)) {
		writeErr(w, http.StatusTooManyRequests, "too many requests for this token")
		return
	}
	if err := s.Store.Q.TouchAPIToken(r.Context(), row.TokenID); err != nil {
		s.Log.Error("touch api token", "err", err)
	}
	p := &Principal{User: userFromTokenRow(row), TokenHash: row.TokenHash, Kind: "token"}
	next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
}

type tokenJSON struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Store.Q.ListAPITokens(r.Context(), principalFrom(r.Context()).User.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]tokenJSON, 0, len(rows))
	for _, t := range rows {
		j := tokenJSON{ID: t.ID, Name: t.Name, Prefix: t.Prefix, CreatedAt: t.CreatedAt}
		if t.LastUsedAt.Valid {
			j.LastUsedAt = &t.LastUsedAt.Time
		}
		if t.ExpiresAt.Valid {
			j.ExpiresAt = &t.ExpiresAt.Time
		}
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, out)
}

// createToken mints a token and shows it once; only its hash is kept.
func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name          string `json:"name"`
		ExpiresInDays int    `json:"expires_in_days"`
	}
	if !decode(w, r, &in) {
		return
	}
	name := strings.TrimSpace(in.Name)
	if l := len([]rune(name)); l < 1 || l > 60 {
		writeErr(w, http.StatusBadRequest, "give the token a name of 1-60 characters")
		return
	}
	if in.ExpiresInDays < 0 || in.ExpiresInDays > maxTokenDays {
		writeErr(w, http.StatusBadRequest, "expires_in_days must be between 1 and 365, or omitted for a token that does not expire")
		return
	}
	ctx := r.Context()
	user := principalFrom(ctx).User
	if n, err := s.Store.Q.CountAPITokens(ctx, user.ID); err != nil {
		s.fail(w, r, err)
		return
	} else if n >= maxAPITokens {
		writeErr(w, http.StatusConflict, "you can have up to 10 API tokens: revoke one first")
		return
	}
	token, hash, err := newAPIToken()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var expires pgtype.Timestamptz
	if in.ExpiresInDays > 0 {
		expires = pgtype.Timestamptz{Time: time.Now().Add(time.Duration(in.ExpiresInDays) * 24 * time.Hour), Valid: true}
	}
	id, err := s.Store.Q.CreateAPIToken(ctx, db.CreateAPITokenParams{UserID: user.ID, Name: name, Prefix: token[:len(apiTokenPrefix)+6], TokenHash: hash, ExpiresAt: expires})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := map[string]any{"id": id, "name": name, "token": token, "prefix": token[:len(apiTokenPrefix)+6]}
	if expires.Valid {
		out["expires_at"] = expires.Time
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) deleteToken(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	n, err := s.Store.Q.RevokeAPIToken(r.Context(), db.RevokeAPITokenParams{ID: id, UserID: principalFrom(r.Context()).User.ID})
	if err != nil {
		s.fail(w, r, err)
		return
	} else if n == 0 {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func userFromTokenRow(r db.GetAPITokenUserRow) db.User {
	return db.User{ID: r.ID, Email: r.Email, PasswordHash: r.PasswordHash, IspendCustomerID: r.IspendCustomerID,
		EmailVerifiedAt: r.EmailVerifiedAt, Phone: r.Phone, Status: r.Status, IsAdmin: r.IsAdmin,
		VmLimit: r.VmLimit, AutoConvert: r.AutoConvert, CreatedAt: r.CreatedAt}
}
