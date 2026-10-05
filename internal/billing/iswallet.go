package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

// ErrBalancesUnconfirmed is returned by Balances until iswallet documents how to read a
// wallet's balances. The guide describes the call ("one call, both currencies") but gives
// no path or response shape, and a wrong guess about a money read is worse than failing.
// Every caller already treats a Balances error as "wallet unavailable" and fails safe.
var ErrBalancesUnconfirmed = errors.New("billing: the iswallet balance endpoint is not yet confirmed")

// ISWallet is the real client for the iswallet (iSpend) API.
// Reference: XENOS-INTEGRATION-GUIDE.md from the iswallet team (v1.0, 5 Oct 2026).
type ISWallet struct {
	BaseURL        string // e.g. https://synledger.name.ng/iwallet
	APIKey         string
	MerchantWallet string // the Xenos wallet charges are paid into
	// OwnerPrefix namespaces owner_ref. iswallet's owner_ref uniqueness is global across tenants,
	// so a bare "8821" can collide with another tenant. Use a different prefix per environment too:
	// sandbox wallets are never reset, so a rebuilt dev database would otherwise reuse old refs.
	OwnerPrefix string
	// USDTDecimals is the scale of iswallet's USDT minor unit. REQUIRED: a wrong value mis-prices
	// every charge by a power of ten. Confirm it with iswallet; do not assume 6.
	USDTDecimals int

	HTTP *http.Client
	Log  *slog.Logger

	limiter *rate.Limiter
}

// ISWalletConfig builds a client.
type ISWalletConfig struct {
	BaseURL, APIKey, MerchantWallet, OwnerPrefix string
	USDTDecimals                                 int
	// RequestsPerMinute caps our own call rate. iswallet allows 100/min per key and sends no
	// Retry-After on 429, so we stay under it ourselves. Default 80.
	RequestsPerMinute int
	Log               *slog.Logger
}

func NewISWallet(c ISWalletConfig) (*ISWallet, error) {
	switch {
	case c.BaseURL == "":
		return nil, errors.New("iswallet: base URL is required")
	case c.APIKey == "":
		return nil, errors.New("iswallet: API key is required")
	case c.MerchantWallet == "":
		return nil, errors.New("iswallet: merchant wallet id is required (create one wallet for Xenos and set XENOS_ISPEND_MERCHANT_WALLET)")
	case c.USDTDecimals < 1 || c.USDTDecimals > 12:
		return nil, errors.New("iswallet: USDT decimals must be set explicitly (XENOS_ISPEND_USDT_DECIMALS); a wrong scale mis-prices every charge")
	}
	if c.OwnerPrefix == "" {
		c.OwnerPrefix = "xenos"
	}
	rpm := c.RequestsPerMinute
	if rpm <= 0 {
		rpm = 80
	}
	log := c.Log
	if log == nil {
		log = slog.Default()
	}
	return &ISWallet{
		BaseURL: strings.TrimRight(c.BaseURL, "/"), APIKey: c.APIKey, MerchantWallet: c.MerchantWallet,
		OwnerPrefix: c.OwnerPrefix, USDTDecimals: c.USDTDecimals, Log: log,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
		limiter: rate.NewLimiter(rate.Limit(float64(rpm)/60), 10),
	}, nil
}

// APIError is an error answer from iswallet.
type APIError struct {
	Status    int
	Code      string
	Message   string
	RequestID string // quote this to iswallet support
	Original  json.RawMessage
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("iswallet: %d %s", e.Status, e.Code)
	if e.Message != "" {
		s += ": " + e.Message
	}
	if e.RequestID != "" {
		s += " (request " + e.RequestID + ")"
	}
	return s
}

// Is maps iswallet's error codes onto the sentinels the rest of Xenos branches on.
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrInsufficientFunds:
		return e.Code == "INSUFFICIENT_FUNDS"
	case ErrQuoteExpired:
		return e.Code == "QUOTE_EXPIRED"
	case ErrQuoteUsed:
		return e.Code == "QUOTE_ALREADY_USED"
	case ErrLiquidity:
		return e.Code == "INSUFFICIENT_LIQUIDITY"
	case ErrQuoteUnavailable:
		return e.Code == "FX_UNAVAILABLE"
	case ErrRateLimited:
		return e.Status == http.StatusTooManyRequests || e.Code == "RATE_LIMITED"
	}
	return false
}

func (c *ISWallet) do(ctx context.Context, method, path, idemKey string, in, out any) error {
	if err := c.limiter.Wait(ctx); err != nil {
		return err
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return parseAPIError(resp, raw)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("iswallet: unreadable %d response from %s %s: %w", resp.StatusCode, method, path, err)
	}
	return nil
}

// parseAPIError tolerates the envelope shapes we expect: {"error":{"code","message","original_response"},"request_id"}.
func parseAPIError(resp *http.Response, raw []byte) error {
	e := &APIError{Status: resp.StatusCode, RequestID: resp.Header.Get("X-Request-Id")}
	var env struct {
		Error     json.RawMessage `json:"error"`
		Code      string          `json:"code"`
		Message   string          `json:"message"`
		RequestID string          `json:"request_id"`
	}
	if json.Unmarshal(raw, &env) == nil {
		e.Code, e.Message = env.Code, env.Message
		if env.RequestID != "" {
			e.RequestID = env.RequestID
		}
		var inner struct {
			Code      string          `json:"code"`
			Message   string          `json:"message"`
			RequestID string          `json:"request_id"`
			Original  json.RawMessage `json:"original_response"`
		}
		if len(env.Error) > 0 && json.Unmarshal(env.Error, &inner) == nil {
			if inner.Code != "" {
				e.Code = inner.Code
			}
			if inner.Message != "" {
				e.Message = inner.Message
			}
			if inner.RequestID != "" {
				e.RequestID = inner.RequestID
			}
			e.Original = inner.Original
		} else if len(env.Error) > 0 {
			var s string
			if json.Unmarshal(env.Error, &s) == nil && e.Message == "" {
				e.Message = s
			}
		}
	}
	if e.Code == "" && resp.StatusCode == http.StatusTooManyRequests {
		e.Code = "RATE_LIMITED"
	}
	if e.Code == "" {
		e.Code = fmt.Sprintf("HTTP_%d", resp.StatusCode)
	}
	return e
}

// ---- customers ----

type walletJSON struct {
	WalletID string `json:"wallet_id"`
}

func (c *ISWallet) CreateCustomer(ctx context.Context, key, ref, email, phone string) (Customer, error) {
	in := map[string]string{
		"owner_type": "client_end_user", "owner_ref": c.OwnerPrefix + ":" + ref,
		"currency": "NGN", "initial_tier": "TIER_1", "email": email,
	}
	if phone != "" {
		in["phone"] = phone
	}
	var w walletJSON
	err := c.do(ctx, http.MethodPost, "/v1/wallets", key, in, &w)
	var api *APIError
	if errors.As(err, &api) && api.Code == "WALLET_ALREADY_EXISTS" {
		// A re-run: the existing wallet is nested in the error. Treat it as success.
		if id := walletIDFrom(api.Original); id != "" {
			w, err = walletJSON{WalletID: id}, nil
		}
	}
	if err != nil {
		return Customer{}, err
	}
	if w.WalletID == "" {
		return Customer{}, errors.New("iswallet: create wallet returned no wallet_id")
	}
	cust, vaErr := c.Customer(ctx, w.WalletID)
	if vaErr != nil {
		// The wallet exists; the account number can be issued later (Customer is idempotent).
		c.Log.Warn("virtual account not issued at signup", "wallet", w.WalletID, "err", vaErr)
		return Customer{ID: w.WalletID}, nil
	}
	return cust, nil
}

func walletIDFrom(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var flat walletJSON
	if json.Unmarshal(raw, &flat) == nil && flat.WalletID != "" {
		return flat.WalletID
	}
	var nested struct {
		Wallet walletJSON `json:"wallet"`
	}
	if json.Unmarshal(raw, &nested) == nil {
		return nested.Wallet.WalletID
	}
	return ""
}

// Customer issues (or re-reads) the wallet's permanent virtual account. The call is
// synchronous and idempotent: asking again returns the same account.
func (c *ISWallet) Customer(ctx context.Context, id string) (Customer, error) {
	var va struct {
		AccountNumber string `json:"account_number"`
		AccountName   string `json:"account_name"`
		BankName      string `json:"bank_name"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/wallets/"+url.PathEscape(id)+"/virtual-account", "va:"+id, map[string]string{"type": "permanent"}, &va)
	if err != nil {
		return Customer{}, err
	}
	return Customer{ID: id, VirtualAcct: va.AccountNumber, VirtualBank: va.BankName, VirtualName: va.AccountName}, nil
}

// Balances is not implemented: see ErrBalancesUnconfirmed.
func (c *ISWallet) Balances(context.Context, string) (Balances, error) {
	return Balances{}, ErrBalancesUnconfirmed
}

// ---- rates and conversion ----

func (c *ISWallet) Rate(ctx context.Context) (int64, error) {
	var out struct {
		Rate struct {
			Effective string `json:"effective_rate"` // USDT per 1 NGN, spread included
		} `json:"rate"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/rates?from=NGN&to=USDT", "", nil, &out); err != nil {
		return 0, err
	}
	return KoboPerUSDT(out.Rate.Effective)
}

func (c *ISWallet) Quote(ctx context.Context, customerID string, amountNGN int64) (Quote, error) {
	in := map[string]any{"source_wallet_id": customerID, "target_currency": "USDT", "amount": amountNGN, "amount_side": "source"}
	var out struct {
		QuoteID      string    `json:"quote_id"`
		DebitAmount  int64     `json:"debit_amount"`
		CreditAmount int64     `json:"credit_amount"`
		FXRate       string    `json:"fx_rate"` // USDT per 1 NGN
		ExpiresAt    time.Time `json:"expires_at"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/convert/quotes", "", in, &out); err != nil {
		return Quote{}, err
	}
	if out.QuoteID == "" {
		return Quote{}, errors.New("iswallet: quote response has no quote_id")
	}
	return Quote{ID: out.QuoteID, AmountNGN: out.DebitAmount, AmountUSDT: MinorToMicro(out.CreditAmount, c.USDTDecimals),
		Rate: NairaPerUSDT(out.FXRate), ExpiresAt: out.ExpiresAt}, nil
}

// Convert executes a quote. The idempotency key goes in the header AND the body (iswallet
// takes it as client_idempotency_key here). A replay returns the original result, even after expiry.
func (c *ISWallet) Convert(ctx context.Context, key, _ /* customerID: carried by the quote */, quoteID string) (Movement, error) {
	in := map[string]any{"quote_id": quoteID, "client_idempotency_key": key, "narration": "compute credit",
		"metadata": map[string]string{"source": "xenos"}}
	var out struct {
		ConvertID    string `json:"convert_id"`
		Status       string `json:"status"`
		CreditAmount int64  `json:"credit_amount"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/convert", key, in, &out); err != nil {
		return Movement{}, err
	}
	if out.ConvertID == "" || (out.Status != "" && out.Status != "completed") {
		return Movement{}, fmt.Errorf("iswallet: convert did not complete (status %q)", out.Status)
	}
	return Movement{ID: out.ConvertID, CreditUUSDT: MinorToMicro(out.CreditAmount, c.USDTDecimals)}, nil
}

// ---- transfers ----

func (c *ISWallet) transfer(ctx context.Context, key, from, to string, uusdt int64, narration string) (Movement, error) {
	minor, err := MicroToMinor(uusdt, c.USDTDecimals)
	if err != nil {
		return Movement{}, err // never round a charge
	}
	if len(narration) > 200 {
		narration = narration[:200]
	}
	in := map[string]any{"from_wallet_id": from, "to_wallet_id": to, "amount": minor, "currency": "USDT", "narration": narration}
	var out struct {
		TransferID string `json:"transfer_id"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/transfers", key, in, &out); err != nil {
		return Movement{}, err
	}
	if out.TransferID == "" {
		return Movement{}, errors.New("iswallet: transfer response has no transfer_id")
	}
	return Movement{ID: out.TransferID}, nil
}

func (c *ISWallet) Charge(ctx context.Context, key, customerID string, uusdt int64, narration string) (Movement, error) {
	return c.transfer(ctx, key, customerID, c.MerchantWallet, uusdt, narration)
}

// Adjust is a transfer between the customer and the Xenos merchant wallet: a credit is paid
// from the merchant wallet, a debit is collected into it. (iswallet's /v1/platform/credits is
// an operator facility whose request shape the guide does not give.)
func (c *ISWallet) Adjust(ctx context.Context, key, customerID string, uusdt int64, note string) (Movement, error) {
	if uusdt >= 0 {
		return c.transfer(ctx, key, c.MerchantWallet, customerID, uusdt, "adjustment: "+note)
	}
	return c.transfer(ctx, key, customerID, c.MerchantWallet, -uusdt, "adjustment: "+note)
}

// ---- webhooks ----

// SubscribeWebhook registers our endpoint. The signing secret is returned ONCE; store it as
// XENOS_ISPEND_WEBHOOK_SECRET. One subscription per environment.
func (c *ISWallet) SubscribeWebhook(ctx context.Context, key, endpointURL, description string) (id, secret string, err error) {
	in := map[string]any{"endpoint_url": endpointURL, "description": description,
		"event_types": []string{EventCreditPosted, EventCreditReversed}}
	var out struct {
		ID             string `json:"id"`
		SubscriptionID string `json:"subscription_id"`
		SigningSecret  string `json:"signing_secret"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/webhooks/subscriptions", key, in, &out); err != nil {
		return "", "", err
	}
	id = out.ID
	if id == "" {
		id = out.SubscriptionID
	}
	return id, out.SigningSecret, nil
}

var _ ISpend = (*ISWallet)(nil)
