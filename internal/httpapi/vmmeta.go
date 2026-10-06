package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/israel-duff/xenos/internal/store/db"
)

const (
	maxLabels         = 16
	maxBootScriptSize = 16 << 10 // the guest agent takes 64 KiB on stdin; stay well under
)

var (
	labelKeyRe   = regexp.MustCompile(`^[a-z0-9]([a-z0-9._/-]{0,61}[a-z0-9])?$`)
	labelValueRe = regexp.MustCompile(`^[A-Za-z0-9._/:@+-]{0,63}$`)
)

// validLabels checks a label set and returns its JSON for storage, or the reason it is refused.
func validLabels(in map[string]string) ([]byte, string) {
	if len(in) > maxLabels {
		return nil, fmt.Sprintf("at most %d labels", maxLabels)
	}
	for k, v := range in {
		if !labelKeyRe.MatchString(k) {
			return nil, fmt.Sprintf("label key %q must be 1-63 characters: lowercase letters, digits and . _ / -", k)
		}
		if !labelValueRe.MatchString(v) {
			return nil, fmt.Sprintf("label value for %q must be at most 63 characters: letters, digits and . _ / : @ + -", k)
		}
	}
	if in == nil {
		in = map[string]string{}
	}
	b, _ := json.Marshal(in)
	return b, ""
}

func validBootScript(s string) string {
	switch {
	case len(s) > maxBootScriptSize:
		return fmt.Sprintf("boot_script is at most %d KiB", maxBootScriptSize>>10)
	case !utf8.ValidString(s) || strings.ContainsRune(s, 0):
		return "boot_script must be text"
	}
	return ""
}

// labelFilter reads repeated ?label=key=value parameters; a VM must carry all of them.
func labelFilter(w http.ResponseWriter, r *http.Request) (map[string]string, bool) {
	want := map[string]string{}
	for _, kv := range r.URL.Query()["label"] {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			writeErr(w, http.StatusBadRequest, "label filters look like label=key=value")
			return nil, false
		}
		want[k] = v
	}
	return want, true
}

func labelsMatch(raw []byte, want map[string]string) bool {
	if len(want) == 0 {
		return true
	}
	have := map[string]string{}
	_ = json.Unmarshal(raw, &have)
	for k, v := range want {
		if got, ok := have[k]; !ok || got != v {
			return false
		}
	}
	return true
}

func isUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

// replayCreate answers a repeated create: the same Idempotency-Key with the same plan, template and hostname
// returns the VM already made (200); a different request under that key is refused (422). It reports whether
// it wrote a response.
func (s *Server) replayCreate(w http.ResponseWriter, r *http.Request, userID int64, token, plan, template, hostname string) bool {
	prev, err := s.Store.Q.GetVMByClientToken(r.Context(), db.GetVMByClientTokenParams{UserID: userID, ClientToken: textOf(token)})
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	} else if err != nil {
		s.fail(w, r, err)
		return true
	}
	if prev.PlanSlug != plan || prev.TemplateSlug != template || (hostname != "" && prev.Hostname != strings.ToLower(strings.TrimSpace(hostname))) {
		writeErr(w, http.StatusUnprocessableEntity, "this Idempotency-Key was already used for a different request")
		return true
	}
	s.respondVM(w, r, prev.ID, userID, http.StatusOK)
	return true
}

// patchVM updates a VM's labels (the whole set is replaced).
func (s *Server) patchVM(w http.ResponseWriter, r *http.Request) {
	v, ok := s.ownedVM(w, r)
	if !ok {
		return
	}
	var in struct {
		Labels map[string]string `json:"labels"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Labels == nil {
		writeErr(w, http.StatusBadRequest, "nothing to change: send labels")
		return
	}
	labels, msg := validLabels(in.Labels)
	if msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	uid := principalFrom(r.Context()).User.ID
	if _, err := s.Store.Q.SetVMLabels(r.Context(), db.SetVMLabelsParams{ID: v.ID, UserID: uid, Labels: labels}); err != nil {
		s.fail(w, r, err)
		return
	}
	s.respondVM(w, r, v.ID, uid, http.StatusOK)
}
