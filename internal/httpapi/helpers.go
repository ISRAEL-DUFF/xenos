package httpapi

import (
	"encoding/json"
	"github.com/jackc/pgx/v5/pgtype"
	"net"
	"net/http"
	"strings"
)

const maxBody = 64 << 10

// decode reads a JSON body of at most 64 KiB into v.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// clientIP returns the caller's address. Behind a trusted reverse proxy it
// uses the rightmost X-Forwarded-For entry (the one the proxy appended);
// otherwise forwarded headers are ignored because clients can forge them.
func (s *Server) clientIP(r *http.Request) string {
	if s.Cfg.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func textOf(v string) pgtype.Text { return pgtype.Text{String: v, Valid: true} }
