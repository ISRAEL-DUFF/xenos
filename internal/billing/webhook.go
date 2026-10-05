package billing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// iswallet webhook delivery (guide §8):
//
//	X-iSpend-Signature        sha256=<hex HMAC-SHA256(signing_secret, "<timestamp>.<raw body>")>
//	X-iSpend-Timestamp        unix seconds
//	X-iSpend-Idempotency-Key  per-delivery key
//
// Events we subscribe to: wallet.credit.posted (a deposit is spendable) and
// wallet.credit.reversed (a confirmed deposit was clawed back).

const (
	HeaderSignature   = "X-iSpend-Signature"
	HeaderTimestamp   = "X-iSpend-Timestamp"
	HeaderDeliveryKey = "X-iSpend-Idempotency-Key"

	EventCreditPosted   = "wallet.credit.posted"
	EventCreditReversed = "wallet.credit.reversed"

	// SourceBankInflow is the source_type of a naira bank transfer into a virtual account.
	SourceBankInflow = "pull_inflow"

	// SignatureTolerance bounds how old a signed delivery may be, limiting replay of captured requests.
	SignatureTolerance = 5 * time.Minute
)

var ErrBadSignature = errors.New("billing: invalid webhook signature")

// Event is a verified webhook delivery. Amounts are in the currency's minor unit
// (kobo for NGN); only NGN deposits are acted on, so no USDT scaling happens here.
type Event struct {
	DeliveryKey string
	Type        string
	// TypeInferred is true when the delivery did not say what it is and we worked it out
	// from its fields. TODO: confirm with iswallet where the event type is carried.
	TypeInferred bool

	WalletID     string
	Amount       int64 // credited amount, after any provider deduction
	Currency     string
	BalanceAfter int64
	TxnID        string
	OperationID  string
	SourceType   string
	SourceRef    string

	// Reversals only.
	ReversedProviderRef string
	UncoveredAmount     int64
}

// DedupeKey identifies the underlying ledger event across redeliveries. The
// transaction id is stable; the per-delivery key is the fallback.
func (e Event) DedupeKey() string {
	if e.TxnID != "" {
		return e.Type + ":" + e.TxnID
	}
	return e.Type + ":" + e.DeliveryKey
}

type webhookBody struct {
	EventType   string `json:"event_type"`
	Type        string `json:"type"`
	WalletID    string `json:"wallet_id"`
	Amount      int64  `json:"amount"`
	Currency    string `json:"currency"`
	Balance     int64  `json:"balance_after"`
	TxnID       string `json:"txn_id"`
	OperationID string `json:"operation_id"`
	SourceType  string `json:"source_type"`
	SourceRef   string `json:"source_ref"`

	ReversedProviderRef string `json:"reversed_provider_reference"`
	UncoveredAmount     int64  `json:"uncovered_amount"`
}

// SignWebhook returns the headers iswallet would send for body at time t (tests and dev tooling).
func SignWebhook(secret string, t time.Time, body []byte, deliveryKey string) http.Header {
	ts := strconv.FormatInt(t.Unix(), 10)
	h := http.Header{}
	h.Set(HeaderSignature, "sha256="+mac(secret, ts, body))
	h.Set(HeaderTimestamp, ts)
	h.Set(HeaderDeliveryKey, deliveryKey)
	return h
}

func mac(secret, ts string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts + "."))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// ParseWebhook verifies the signature over the RAW body, checks freshness, then
// decodes the event. An empty secret always fails, so a misconfigured server
// rejects everything.
func ParseWebhook(secret string, h http.Header, body []byte, now time.Time) (Event, error) {
	sig := strings.TrimPrefix(h.Get(HeaderSignature), "sha256=")
	ts := h.Get(HeaderTimestamp)
	if secret == "" || sig == "" || ts == "" {
		return Event{}, ErrBadSignature
	}
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return Event{}, ErrBadSignature
	}
	if d := now.Sub(time.Unix(unix, 0)); d > SignatureTolerance || d < -SignatureTolerance {
		return Event{}, ErrBadSignature
	}
	got, err := hex.DecodeString(sig)
	want, _ := hex.DecodeString(mac(secret, ts, body))
	if err != nil || !hmac.Equal(got, want) {
		return Event{}, ErrBadSignature
	}

	var b webhookBody
	if err := json.Unmarshal(body, &b); err != nil {
		return Event{}, fmt.Errorf("billing: malformed webhook body")
	}
	ev := Event{DeliveryKey: h.Get(HeaderDeliveryKey), WalletID: b.WalletID, Amount: b.Amount, Currency: b.Currency,
		BalanceAfter: b.Balance, TxnID: b.TxnID, OperationID: b.OperationID, SourceType: b.SourceType, SourceRef: b.SourceRef,
		ReversedProviderRef: b.ReversedProviderRef, UncoveredAmount: b.UncoveredAmount}
	switch {
	case b.EventType != "":
		ev.Type = b.EventType
	case b.Type != "":
		ev.Type = b.Type
	case h.Get("X-iSpend-Event") != "":
		ev.Type = h.Get("X-iSpend-Event")
	case b.ReversedProviderRef != "":
		ev.Type, ev.TypeInferred = EventCreditReversed, true
	default:
		ev.Type, ev.TypeInferred = EventCreditPosted, true
	}
	return ev, nil
}
