package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/israel-duff/xenos/internal/sshkey"
	"github.com/israel-duff/xenos/internal/store/db"
)

const (
	maxSSHKeysPerUser = 20
	maxKeyNameLen     = 64
)

type sshKeyJSON struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	PublicKey   string    `json:"public_key"`
	Fingerprint string    `json:"fingerprint"`
	CreatedAt   time.Time `json:"created_at"`
}

func toSSHKeyJSON(k db.SshKey) sshKeyJSON {
	return sshKeyJSON{ID: k.ID, Name: k.Name, PublicKey: k.PublicKey, Fingerprint: k.Fingerprint, CreatedAt: k.CreatedAt}
}

func (s *Server) listSSHKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.Store.Q.ListSSHKeys(r.Context(), principalFrom(r.Context()).User.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]sshKeyJSON, 0, len(keys))
	for _, k := range keys {
		out = append(out, toSSHKeyJSON(k))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createSSHKey(w http.ResponseWriter, r *http.Request) {
	uid := principalFrom(r.Context()).User.ID
	var in struct {
		Name      string `json:"name"`
		PublicKey string `json:"public_key"`
	}
	if !decode(w, r, &in) {
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || utf8.RuneCountInString(name) > maxKeyNameLen || strings.ContainsFunc(name, func(r rune) bool { return r < ' ' || r == 0x7f }) {
		writeErr(w, http.StatusBadRequest, "name must be 1-64 printable characters")
		return
	}
	parsed, err := sshkey.Parse(in.PublicKey)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	n, err := s.Store.Q.CountSSHKeys(r.Context(), uid)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if n >= maxSSHKeysPerUser {
		writeErr(w, http.StatusConflict, "SSH key limit reached")
		return
	}
	k, err := s.Store.Q.CreateSSHKey(r.Context(), db.CreateSSHKeyParams{
		UserID: uid, Name: name, PublicKey: parsed.Normalized, Fingerprint: parsed.Fingerprint})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		writeErr(w, http.StatusConflict, "this key is already on your account")
		return
	} else if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toSSHKeyJSON(k))
}

func (s *Server) deleteSSHKey(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	// Scoped by user_id: another user's key id reports 404, same as a missing one.
	n, err := s.Store.Q.DeleteSSHKey(r.Context(), db.DeleteSSHKeyParams{ID: id, UserID: principalFrom(r.Context()).User.ID})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if n == 0 {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
