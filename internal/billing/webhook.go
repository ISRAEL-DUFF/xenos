package billing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ASSUMPTION: the signature scheme and payload below are a placeholder until
// the real iSpend webhook documentation is available. Only this file and the
// handler that calls it need to change.
//
//	header  X-Ispend-Signature: t=<unix seconds>,v1=<hex HMAC-SHA256(secret, "<t>.<body>")>
//	body    {"id":"evt_…","type":"deposit.confirmed","customer_id":"…","amount_kobo":500000}

const (
	SignatureHeader = "X-Ispend-Signature"
	EventDeposit    = "deposit.confirmed"

	// SignatureTolerance bounds how old a signed webhook may be, limiting replay of captured requests.
	SignatureTolerance = 5 * time.Minute
)

var ErrBadSignature = errors.New("billing: invalid webhook signature")

// Event is a verified webhook delivery.
type Event struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	CustomerID string `json:"customer_id"`
	AmountKobo int64  `json:"amount_kobo"`
}

// SignWebhook produces the header value for body at time t (used by tests and the dev simulator).
func SignWebhook(secret string, t time.Time, body []byte) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	return fmt.Sprintf("t=%s,v1=%s", ts, mac(secret, ts, body))
}

func mac(secret, ts string, body []byte) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(ts + "."))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// ParseWebhook verifies the signature and freshness, then decodes the event.
// An empty secret always fails, so a misconfigured server rejects everything.
func ParseWebhook(secret, header string, body []byte, now time.Time) (Event, error) {
	if secret == "" || header == "" {
		return Event{}, ErrBadSignature
	}
	var ts, sig string
	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			ts = v
		case "v1":
			sig = v
		}
	}
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || sig == "" {
		return Event{}, ErrBadSignature
	}
	if d := now.Sub(time.Unix(unix, 0)); d > SignatureTolerance || d < -SignatureTolerance {
		return Event{}, ErrBadSignature
	}
	want, err := hex.DecodeString(sig)
	if err != nil || !hmac.Equal(want, mustDecode(mac(secret, ts, body))) {
		return Event{}, ErrBadSignature
	}
	var ev Event
	if err := json.Unmarshal(body, &ev); err != nil || ev.ID == "" {
		return Event{}, fmt.Errorf("billing: malformed webhook body")
	}
	return ev, nil
}

func mustDecode(h string) []byte {
	b, _ := hex.DecodeString(h)
	return b
}
