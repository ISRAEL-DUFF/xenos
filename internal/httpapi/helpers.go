package httpapi

import (
	"encoding/json"
	"github.com/jackc/pgx/v5/pgtype"
	"net"
	"net/http"
	"net/netip"
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
	if s.Cfg.TrustProxy && s.fromTrustedProxy(r) {
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

// fromTrustedProxy reports whether the TCP peer is the reverse proxy: loopback (Caddy on the same host)
// or an address in XENOS_TRUSTED_PROXIES. Anything else could be a client forging X-Forwarded-For.
func (s *Server) fromTrustedProxy(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	if ip.IsLoopback() {
		return true
	}
	for _, p := range s.Cfg.TrustedProxies {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

func textOf(v string) pgtype.Text { return pgtype.Text{String: v, Valid: true} }
