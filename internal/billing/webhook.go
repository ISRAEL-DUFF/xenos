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

// iswallet webhook delivery (guide v1.1 §9):
//
//	X-iSpend-Signature        sha256=<hex HMAC-SHA256(signing_secret, "<timestamp>.<raw body>")>
//	X-iSpend-Timestamp        unix seconds
//	X-iSpend-Idempotency-Key  per-delivery key
//
// The POSTed body is an envelope: {id, idempotency_key, event_type, schema_version, occurred_at,
// wallet_id, data{...}}. `id` is the stable event id; the payload is under `data`.
// Events we subscribe to: wallet.credit.posted (a credit is spendable) and wallet.credit.reversed
// (a confirmed deposit was clawed back).

const (
	HeaderSignature   = "X-iSpend-Signature"
	HeaderTimestamp   = "X-iSpend-Timestamp"
	HeaderDeliveryKey = "X-iSpend-Idempotency-Key"

	EventCreditPosted   = "wallet.credit.posted"
	EventCreditReversed = "wallet.credit.reversed"

	// SourceBankInflow is the source_type of a naira bank transfer into a virtual account, the
	// only credit we auto-convert. Other credits, notably our own adjustment transfers
	// ("wallet_transfer"), must never be converted.
	SourceBankInflow = "pull_inflow"

	// SignatureTolerance bounds how old a signed delivery may be, limiting replay of captured requests.
	SignatureTolerance = 5 * time.Minute
)

var ErrBadSignature = errors.New("billing: invalid webhook signature")

// Event is a verified webhook delivery. Amounts are in the currency's minor unit
// (kobo for NGN); only NGN deposits are acted on, so no USDT scaling happens here.
type Event struct {
	ID          string // stable event id, the same on every retry
	DeliveryKey string
	Type        string
	OccurredAt  time.Time

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

// DedupeKey identifies the underlying ledger fact across redeliveries. The transaction id is the
// ledger fact itself, so it also protects us if iswallet ever emitted two events for one movement;
// the stable event id is the fallback.
func (e Event) DedupeKey() string {
	if e.TxnID != "" {
		return e.Type + ":" + e.TxnID
	}
	return e.Type + ":" + e.ID
}

type envelope struct {
	ID             string    `json:"id"`
	IdempotencyKey string    `json:"idempotency_key"`
	EventType      string    `json:"event_type"`
	OccurredAt     time.Time `json:"occurred_at"`
	WalletID       string    `json:"wallet_id"`
	Data           struct {
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
	} `json:"data"`
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

// ParseWebhook verifies the signature over the RAW body, checks freshness, then decodes the
// envelope. An empty secret always fails, so a misconfigured server rejects everything. A body
// that does not say what event it is, is rejected: we never infer an event's meaning from its shape.
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

	var e envelope
	if err := json.Unmarshal(body, &e); err != nil || e.EventType == "" {
		return Event{}, fmt.Errorf("billing: malformed webhook body (no event_type)")
	}
	if e.ID == "" && e.Data.TxnID == "" {
		// Without either, every such event would share one dedupe key and all but the first would be dropped.
		return Event{}, fmt.Errorf("billing: malformed webhook body (no event id or txn_id)")
	}
	d := e.Data
	wallet := d.WalletID
	if wallet == "" {
		wallet = e.WalletID
	}
	key := e.IdempotencyKey
	if key == "" {
		key = h.Get(HeaderDeliveryKey)
	}
	return Event{ID: e.ID, DeliveryKey: key, Type: e.EventType, OccurredAt: e.OccurredAt, WalletID: wallet,
		Amount: d.Amount, Currency: d.Currency, BalanceAfter: d.Balance, TxnID: d.TxnID, OperationID: d.OperationID,
		SourceType: d.SourceType, SourceRef: d.SourceRef, ReversedProviderRef: d.ReversedProviderRef,
		UncoveredAmount: d.UncoveredAmount}, nil
}
