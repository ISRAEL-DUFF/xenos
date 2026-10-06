package billing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubServer plays iswallet as documented in the integration guide, and records what it was sent.
type stubServer struct {
	t  *testing.T
	mu sync.Mutex

	reqs  []stubReq
	reply func(r stubReq) (status int, body string) // overrides the defaults when it returns status != 0
}

type stubReq struct {
	Method, Path, Query string
	Auth, IdemKey       string
	Body                map[string]any
}

func newStub(t *testing.T) (*stubServer, *ISWallet) {
	s := &stubServer{t: t}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		req := stubReq{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), r.Header.Get("Idempotency-Key"), body}
		s.mu.Lock()
		s.reqs = append(s.reqs, req)
		reply := s.reply
		s.mu.Unlock()
		status, out := 0, ""
		if reply != nil {
			status, out = reply(req)
		}
		if status == 0 {
			status, out = defaultReply(req)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(out))
	}))
	t.Cleanup(ts.Close)
	c, err := NewISWallet(ISWalletConfig{BaseURL: ts.URL + "/", APIKey: "key_test",
		OwnerPrefix: "xenos-test", USDTDecimals: 6, RequestsPerMinute: 6000, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return s, c
}

func (s *stubServer) last() stubReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reqs[len(s.reqs)-1]
}

func defaultReply(r stubReq) (int, string) {
	switch {
	case r.Method == "GET" && r.Path == "/v1/wallets": // as the live sandbox: conversion adds a sibling USDT wallet
		if strings.Contains(r.Query, "owner_ref=op") {
			return 200, `{"wallets":[{"wallet_id":"wal_op","owner_ref":"op","currency":"NGN"},{"wallet_id":"wal_opusdt","owner_ref":"op","currency":"USDT"}],"total":2}`
		}
		return 200, `{"wallets":[{"wallet_id":"wal_usdt","owner_ref":"o","currency":"USDT"},{"wallet_id":"wal_cust","owner_ref":"o","currency":"NGN"}],"total":2}`
	case r.Method == "GET" && strings.HasPrefix(r.Path, "/v1/wallets/") && strings.Count(r.Path, "/") == 3:
		id := strings.TrimPrefix(r.Path, "/v1/wallets/")
		ref := "o"
		if id == "wal_op" {
			ref = "op"
		}
		return 200, `{"wallet_id":"` + id + `","owner_ref":"` + ref + `","owner_type":"client","currency":"NGN"}`
	case r.Path == "/v1/wallets":
		if r.Body["client_id"] != "xenos" { // as the live sandbox does
			return 403, `{"error":{"code":"SCOPE_INSUFFICIENT","message":"client_id must match your API key's client_id"}}`
		}
		return 201, `{"wallet_id":"wal_1","owner_ref":"` + str(r.Body["owner_ref"]) + `","currency":"NGN","tier":"TIER_1","status":"active"}`
	case strings.HasSuffix(r.Path, "/virtual-account"):
		return 200, `{"wallet_id":"wal_1","account_number":"8107536198","account_name":"ADA OKONKWO","bank_name":"Wema Bank","is_permanent":true}`
	case r.Path == "/v1/rates":
		return 200, `{"rate":{"from":"NGN","to":"USDT","market_rate":"0.00062500","spread_bps":150,"effective_rate":"0.00061562"}}`
	case r.Path == "/v1/convert/quotes":
		return 200, `{"quote_id":"cvq_1","debit_amount":5000000,"credit_amount":30781000,"fx_rate":"0.00061562","expires_at":"2026-10-05T09:15:02Z"}`
	case r.Path == "/v1/convert":
		return 200, `{"convert_id":"cv_1","status":"completed","debit_amount":5000000,"credit_amount":30781000}`
	case strings.HasSuffix(r.Path, "/balance"):
		return 200, `{"wallet_id":"wal_1","currency":"NGN","balance":{"total":5000000,"available":5000000,"pending":0},` +
			`"balances":[{"currency":"NGN","total":5000000,"available":4000000,"pending":1000000,"scale":2},` +
			`{"currency":"USDT","total":30781000,"available":30000000,"pending":781000,"scale":6}]}`
	case r.Path == "/v1/platform/account":
		return 200, `{"client_id":"xenos","operating_wallet_id":"wal_op","balances":[{"currency":"USDT","total":9000000,"available":8000000,"pending":0}]}`
	case r.Path == "/v1/transfers":
		return 200, `{"transfer_id":"tr_1","txn_id":"tx_1"}`
	case r.Path == "/v1/webhooks/subscriptions":
		return 201, `{"id":"sub_1","signing_secret":"whsec_shown_once"}`
	}
	return 404, `{"error":{"code":"NOT_FOUND","message":"no route"}}`
}

func str(v any) string { s, _ := v.(string); return s }

func errReply(status int, code string) func(stubReq) (int, string) {
	return func(stubReq) (int, string) {
		return status, `{"error":{"code":"` + code + `","message":"nope","request_id":"req_42"}}`
	}
}

func TestISWalletConfigIsValidated(t *testing.T) {
	ok := ISWalletConfig{BaseURL: "http://x", APIKey: "k", USDTDecimals: 6}
	if _, err := NewISWallet(ok); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*ISWalletConfig){
		"no url":      func(c *ISWalletConfig) { c.BaseURL = "" },
		"no key":      func(c *ISWalletConfig) { c.APIKey = "" },
		"no decimals": func(c *ISWalletConfig) { c.USDTDecimals = 0 },
	} {
		c := ok
		mut(&c)
		if _, err := NewISWallet(c); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestCreateCustomer(t *testing.T) {
	s, c := newStub(t)
	cust, err := c.CreateCustomer(context.Background(), "signup:ada@example.com", "user:8821", "ada@example.com", "+2348030000000")
	if err != nil || cust.ID != "wal_1" || cust.VirtualAcct != "8107536198" || cust.VirtualBank != "Wema Bank" || cust.VirtualName != "ADA OKONKWO" {
		t.Fatalf("customer = %+v %v", cust, err)
	}
	if s.reqs[0].Path != "/v1/platform/account" {
		t.Fatalf("the client_id must be learned from /v1/platform/account first: %+v", s.reqs[0])
	}
	create := s.reqs[1]
	if create.Method != "POST" || create.Path != "/v1/wallets" || create.Auth != "Bearer key_test" || create.IdemKey != "signup:ada@example.com" ||
		create.Body["owner_ref"] != "xenos-test:user:8821" || create.Body["owner_type"] != "client_end_user" ||
		create.Body["currency"] != "NGN" || create.Body["initial_tier"] != "TIER_1" || create.Body["email"] != "ada@example.com" ||
		create.Body["client_id"] != "xenos" {
		t.Fatalf("create request: %+v", create)
	}
	if _, nested := create.Body["kyc_profile"]; nested {
		t.Fatal("identity fields must be flat: a nested kyc_profile is rejected by iswallet")
	}
	if va := s.reqs[2]; va.Path != "/v1/wallets/wal_1/virtual-account" || va.Body["type"] != "permanent" || va.IdemKey != "va:wal_1" {
		t.Fatalf("virtual account request: %+v", va)
	}
}

func TestCreateCustomerExistingWalletIsSuccess(t *testing.T) {
	s, c := newStub(t)
	s.reply = func(r stubReq) (int, string) {
		if r.Path == "/v1/wallets" {
			return 409, `{"error":{"code":"WALLET_ALREADY_EXISTS","message":"exists","original_response":{"wallet_id":"wal_existing","currency":"NGN"}}}`
		}
		return 0, ""
	}
	cust, err := c.CreateCustomer(context.Background(), "signup:a", "user:1", "a@b.c", "")
	if err != nil || cust.ID != "wal_existing" {
		t.Fatalf("a 409 WALLET_ALREADY_EXISTS must be read as success with the nested wallet: %+v %v", cust, err)
	}
}

// The live sandbox returns an all-empty wallet in original_response (the guide says it carries the
// existing one). Trusting an empty id would link a customer to nothing, so it must surface as an error.
func TestCreateCustomerRefusesAnEmptyOriginalResponse(t *testing.T) {
	s, c := newStub(t)
	s.reply = func(r stubReq) (int, string) {
		if r.Path == "/v1/wallets" {
			return 409, `{"error":{"code":"WALLET_ALREADY_EXISTS","message":"exists","original_response":{"wallet_id":"","account_number":"","currency":""}}}`
		}
		return 0, ""
	}
	cust, err := c.CreateCustomer(context.Background(), "signup:a", "user:1", "a@b.c", "")
	var api *APIError
	if cust.ID != "" || !errors.As(err, &api) || api.Code != "WALLET_ALREADY_EXISTS" {
		t.Fatalf("must return the 409, not an empty customer: %+v %v", cust, err)
	}
}

func TestCreateCustomerSurvivesVirtualAccountFailure(t *testing.T) {
	s, c := newStub(t)
	s.reply = func(r stubReq) (int, string) {
		if strings.HasSuffix(r.Path, "/virtual-account") {
			return 422, `{"error":{"code":"KYC_REQUIRED","message":"email needed"}}`
		}
		return 0, ""
	}
	cust, err := c.CreateCustomer(context.Background(), "signup:a", "user:1", "", "")
	if err != nil || cust.ID != "wal_1" || cust.VirtualAcct != "" {
		t.Fatalf("the wallet exists even if the account number could not be issued yet: %+v %v", cust, err)
	}
}

func TestRateAndQuote(t *testing.T) {
	_, c := newStub(t)
	ctx := context.Background()
	if k, err := c.Rate(ctx); err != nil || k != 162438 {
		t.Fatalf("rate = %d %v (₦1,624.38 per USDT)", k, err)
	}
	q, err := c.Quote(ctx, "wal_1", 5_000_000)
	if err != nil {
		t.Fatal(err)
	}
	// USDT has 6 decimals (micro-USDT): 30,781,000 minor units = 30.781 USDT.
	if q.ID != "cvq_1" || q.AmountNGN != 5_000_000 || q.AmountUSDT != 30_781_000 || q.Rate != "1624.38" ||
		!q.ExpiresAt.Equal(time.Date(2026, 10, 5, 9, 15, 2, 0, time.UTC)) {
		t.Fatalf("quote = %+v", q)
	}
}

func TestQuoteRequestShape(t *testing.T) {
	s, c := newStub(t)
	_, _ = c.Quote(context.Background(), "wal_1", 5_000_000)
	r := s.last()
	if r.Path != "/v1/convert/quotes" || r.Body["source_wallet_id"] != "wal_1" || r.Body["target_currency"] != "USDT" ||
		r.Body["amount"] != float64(5_000_000) || r.Body["amount_side"] != "source" {
		t.Fatalf("quote request: %+v", r)
	}
}

func TestConvertSendsKeyInHeaderAndBody(t *testing.T) {
	s, c := newStub(t)
	mv, err := c.Convert(context.Background(), "conversion:7f21", "wal_1", "cvq_1")
	if err != nil || mv.ID != "cv_1" || mv.CreditUUSDT != 30_781_000 {
		t.Fatalf("convert = %+v %v", mv, err)
	}
	r := s.last()
	if r.Path != "/v1/convert" || r.IdemKey != "conversion:7f21" || r.Body["client_idempotency_key"] != "conversion:7f21" || r.Body["quote_id"] != "cvq_1" {
		t.Fatalf("convert request: %+v", r)
	}
}

func TestErrorCodesMapToSentinels(t *testing.T) {
	cases := []struct {
		status int
		code   string
		want   error
		call   func(c *ISWallet) error
	}{
		{422, "INSUFFICIENT_FUNDS", ErrInsufficientFunds, func(c *ISWallet) error { _, e := c.Charge(context.Background(), "k", "w", 6000, "n"); return e }},
		{422, "QUOTE_EXPIRED", ErrQuoteExpired, func(c *ISWallet) error { _, e := c.Convert(context.Background(), "k", "w", "q"); return e }},
		{409, "QUOTE_ALREADY_USED", ErrQuoteUsed, func(c *ISWallet) error { _, e := c.Convert(context.Background(), "k", "w", "q"); return e }},
		{422, "INSUFFICIENT_LIQUIDITY", ErrLiquidity, func(c *ISWallet) error { _, e := c.Convert(context.Background(), "k", "w", "q"); return e }},
		{503, "FX_UNAVAILABLE", ErrQuoteUnavailable, func(c *ISWallet) error { _, e := c.Quote(context.Background(), "w", 100); return e }},
		{503, "FX_UNAVAILABLE", ErrQuoteUnavailable, func(c *ISWallet) error { _, e := c.Rate(context.Background()); return e }},
		{422, "CURRENCY_MISMATCH", ErrCurrencyMismatch, func(c *ISWallet) error { _, e := c.Charge(context.Background(), "k", "w", 6000, "n"); return e }},
		{429, "RATE_LIMITED", ErrRateLimited, func(c *ISWallet) error { _, e := c.Charge(context.Background(), "k", "w", 6000, "n"); return e }},
	}
	for _, tc := range cases {
		s, c := newStub(t)
		s.reply = errReply(tc.status, tc.code)
		err := tc.call(c)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: %v is not %v", tc.code, err, tc.want)
		}
		var api *APIError
		if !errors.As(err, &api) || api.RequestID != "req_42" || api.Status != tc.status {
			t.Errorf("%s: the request id must be carried for support: %+v", tc.code, err)
		}
		// A different error must not be mistaken for any other sentinel.
		for _, other := range []error{ErrInsufficientFunds, ErrQuoteExpired, ErrQuoteUsed, ErrLiquidity, ErrQuoteUnavailable, ErrRateLimited, ErrCurrencyMismatch, ErrIdempotencyKeyReused} {
			if other != tc.want && errors.Is(err, other) {
				t.Errorf("%s wrongly matches %v", tc.code, other)
			}
		}
	}
}

func TestRateLimitWithoutErrorBody(t *testing.T) {
	s, c := newStub(t)
	s.reply = func(stubReq) (int, string) { return 429, `` } // no body, no Retry-After
	if _, err := c.Charge(context.Background(), "k", "w", 6000, "n"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("a bare 429 must still be ErrRateLimited: %v", err)
	}
}

func TestChargeAndAdjust(t *testing.T) {
	s, c := newStub(t)
	ctx := context.Background()

	mv, err := c.Charge(ctx, "vm:42:hour:2026100514", "wal_cust", 6000, "vm:42 hour:2026100514")
	if err != nil || mv.ID != "tr_1" {
		t.Fatalf("charge = %+v %v", mv, err)
	}
	r := s.last()
	// 6,000 micro-USDT is 6,000 minor units; the destination is the operating wallet from /v1/platform/account.
	if r.Path != "/v1/transfers" || r.IdemKey != "vm:42:hour:2026100514" || r.Body["from_wallet_id"] != "wal_usdt" ||
		r.Body["to_wallet_id"] != "wal_opusdt" || r.Body["amount"] != float64(6000) || r.Body["currency"] != "USDT" ||
		r.Body["narration"] != "vm:42 hour:2026100514" {
		t.Fatalf("charge request: %+v", r)
	}

	if _, err := c.Adjust(ctx, "adjustment:1", "wal_cust", 2_000_000, "goodwill"); err != nil {
		t.Fatal(err)
	}
	r = s.last()
	if r.Body["from_wallet_id"] != "wal_opusdt" || r.Body["to_wallet_id"] != "wal_usdt" || r.Body["amount"] != float64(2_000_000) || r.Body["narration"] != "adjustment: goodwill" {
		t.Fatalf("a credit is paid from the merchant wallet: %+v", r)
	}
	if _, err := c.Adjust(ctx, "adjustment:2", "wal_cust", -1_000_000, "correction"); err != nil {
		t.Fatal(err)
	}
	if r = s.last(); r.Body["from_wallet_id"] != "wal_usdt" || r.Body["to_wallet_id"] != "wal_opusdt" || r.Body["amount"] != float64(1_000_000) {
		t.Fatalf("a debit is collected into the merchant wallet: %+v", r)
	}
}

func TestChargeNeverRounds(t *testing.T) {
	// With a coarser minor unit (3 decimals) 6,500 micro-USDT does not land on a whole unit: refuse,
	// and never call iswallet. (The live scale is 6, where every micro-USDT amount is representable.)
	s, c := newStub(t)
	c.USDTDecimals = 3
	n := len(s.reqs)
	if _, err := c.Charge(context.Background(), "k", "w", 6500, "n"); !errors.Is(err, ErrUnrepresentable) {
		t.Fatalf("err = %v", err)
	}
	if len(s.reqs) != n {
		t.Fatal("an unrepresentable amount must not reach iswallet")
	}
}

func TestLongNarrationIsTruncated(t *testing.T) {
	s, c := newStub(t)
	_, _ = c.Charge(context.Background(), "k", "w", 6000, strings.Repeat("x", 500))
	if got := str(s.last().Body["narration"]); len(got) != 200 {
		t.Fatalf("narration length %d", len(got))
	}
}

func TestBalancesReadAvailableFromTheBalancesArray(t *testing.T) {
	s, c := newStub(t)
	b, err := c.Balances(context.Background(), "wal_1")
	if err != nil {
		t.Fatal(err)
	}
	// available (total minus pending outflows), not total; USDT is scaled to micro.
	if b.NGNKobo != 4_000_000 || b.USDTMicro != 30_000_000 {
		t.Fatalf("balances = %+v, want available NGN 4,000,000 kobo and 30,000,000 micro-USDT", b)
	}
	if r := s.last(); r.Method != "GET" || r.Path != "/v1/wallets/wal_1/balance" || r.Auth != "Bearer key_test" {
		t.Fatalf("request: %+v", r)
	}
}

func TestBalancesAbsentCurrencyIsZero(t *testing.T) {
	s, c := newStub(t)
	s.reply = func(r stubReq) (int, string) {
		if strings.HasSuffix(r.Path, "/balance") { // a new customer holds NGN only until their first convert
			return 200, `{"wallet_id":"w","currency":"NGN","balance":{"total":100,"available":100,"pending":0},"balances":[{"currency":"NGN","total":100,"available":100,"pending":0,"scale":2}]}`
		}
		return 0, ""
	}
	if b, err := c.Balances(context.Background(), "w"); err != nil || b.NGNKobo != 100 || b.USDTMicro != 0 {
		t.Fatalf("balances = %+v %v", b, err)
	}
}

// A scale we do not expect is refused rather than converted: a power-of-ten error in a balance
// would mis-state what a customer can spend.
func TestBalancesRefuseAnUnexpectedScale(t *testing.T) {
	for name, body := range map[string]string{
		"USDT": `{"balances":[{"currency":"USDT","available":6,"scale":3}]}`,
		"NGN":  `{"balances":[{"currency":"NGN","available":6,"scale":3}]}`,
	} {
		s, c := newStub(t)
		b := body
		s.reply = func(r stubReq) (int, string) {
			if strings.HasSuffix(r.Path, "/balance") {
				return 200, b
			}
			return 0, ""
		}
		if _, err := c.Balances(context.Background(), "w"); !errors.Is(err, ErrScaleMismatch) {
			t.Errorf("%s: err = %v, want ErrScaleMismatch", name, err)
		}
	}
}

func TestMerchantWalletComesFromPlatformAccountAndIsCached(t *testing.T) {
	s, c := newStub(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := c.Charge(ctx, "k"+string(rune('a'+i)), "wal_c", 6000, "n"); err != nil {
			t.Fatal(err)
		}
	}
	lookups := 0
	for _, r := range s.reqs {
		if r.Path == "/v1/platform/account" {
			lookups++
		}
	}
	if lookups != 1 {
		t.Fatalf("the operating wallet is stable: expected one lookup, got %d", lookups)
	}

	// An explicit override skips the lookup entirely.
	s2, c2 := newStub(t)
	c2.MerchantWallet = "wal_override"
	_, _ = c2.Charge(ctx, "k", "wal_c", 6000, "n")
	for _, r := range s2.reqs {
		if r.Path == "/v1/platform/account" {
			t.Fatal("a configured merchant wallet must not trigger a lookup")
		}
	}
	if s2.last().Body["to_wallet_id"] != "wal_override" {
		t.Fatalf("destination: %v", s2.last().Body["to_wallet_id"])
	}
}

func TestMerchantBalance(t *testing.T) {
	s, c := newStub(t)
	s.reply = func(r stubReq) (int, string) {
		if r.Path == "/v1/wallets/wal_opusdt/balance" {
			return 200, `{"balances":[{"currency":"USDT","available":8000000,"scale":6}]}`
		}
		return 0, ""
	}
	if b, err := c.MerchantBalance(context.Background()); err != nil || b != 8_000_000 {
		t.Fatalf("operating wallet available USDT = %d %v", b, err)
	}
}

func TestIdempotencyKeyReusedIsItsOwnError(t *testing.T) {
	s, c := newStub(t)
	s.reply = errReply(422, "IDEMPOTENCY_KEY_REUSED")
	err := func() error { _, e := c.Charge(context.Background(), "k", "w", 6000, "n"); return e }()
	if !errors.Is(err, ErrIdempotencyKeyReused) || errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("a key-reuse 422 must not look like insufficient funds: %v", err)
	}
}

func TestRequestIDFromHeaderWhenBodyHasNone(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-ID", "req_hdr")
		w.WriteHeader(502)
		_, _ = w.Write([]byte(`<html>bad gateway from a proxy</html>`)) // an answer iswallet did not generate
	}))
	defer ts.Close()
	c, _ := NewISWallet(ISWalletConfig{BaseURL: ts.URL, APIKey: "k", MerchantWallet: "w", USDTDecimals: 6, RequestsPerMinute: 6000})
	var api *APIError
	if _, err := c.Rate(context.Background()); !errors.As(err, &api) || api.RequestID != "req_hdr" || api.Status != 502 {
		t.Fatalf("err = %v", err)
	}
}

func TestSubscribeWebhook(t *testing.T) {
	s, c := newStub(t)
	id, secret, err := c.SubscribeWebhook(context.Background(), "xenos:sub:primary", "https://api.xenos.ng/v1/webhooks/ispend", "Xenos production inbox")
	if err != nil || id != "sub_1" || secret != "whsec_shown_once" {
		t.Fatalf("subscribe = %q %q %v", id, secret, err)
	}
	r := s.last()
	types, _ := r.Body["event_types"].([]any)
	if r.Path != "/v1/webhooks/subscriptions" || r.IdemKey != "xenos:sub:primary" || r.Body["endpoint_url"] == nil || len(types) != 2 ||
		types[0] != EventCreditPosted || types[1] != EventCreditReversed {
		t.Fatalf("subscribe request: %+v", r)
	}
}

func TestOwnClientSideRateLimit(t *testing.T) {
	s, c := newStub(t)
	_ = s
	// 60/minute with a burst of 10: the 11th call in a burst must wait ~1s, not hit iswallet at once.
	c.limiter.SetLimit(1)
	c.limiter.SetBurst(2)
	start := time.Now()
	for i := 0; i < 3; i++ {
		_, _ = c.Rate(context.Background())
	}
	if d := time.Since(start); d < 800*time.Millisecond {
		t.Fatalf("3 calls took %v: the client must pace itself under iswallet's 100/min limit", d)
	}
}

// The live sandbox keeps a customer's USDT in a sibling wallet that conversion creates: balances of the
// NGN wallet never list it, so Balances reads the sibling too, and charges are taken from it.
func TestUSDTLivesInASiblingWallet(t *testing.T) {
	s, c := newStub(t)
	s.reply = func(r stubReq) (int, string) {
		switch r.Path {
		case "/v1/wallets/wal_cust/balance":
			return 200, `{"balances":[{"currency":"NGN","available":100,"scale":2}]}`
		case "/v1/wallets/wal_usdt/balance":
			return 200, `{"balances":[{"currency":"USDT","available":35842347,"scale":6}]}`
		}
		return 0, ""
	}
	b, err := c.Balances(context.Background(), "wal_cust")
	if err != nil || b.NGNKobo != 100 || b.USDTMicro != 35_842_347 {
		t.Fatalf("balances = %+v %v", b, err)
	}
}

// Before the first conversion there is no USDT wallet: the charge goes to iswallet with the NGN wallet
// and is answered CURRENCY_MISMATCH, which metering already treats as "not an outage".
func TestNoUSDTWalletYet(t *testing.T) {
	s, c := newStub(t)
	s.reply = func(r stubReq) (int, string) {
		switch {
		case r.Method == "GET" && r.Path == "/v1/wallets":
			return 200, `{"wallets":[{"wallet_id":"wal_cust","owner_ref":"o","currency":"NGN"}],"total":1}`
		case r.Path == "/v1/transfers":
			return 422, `{"error":{"code":"CURRENCY_MISMATCH","message":"wallets must share currency USDT"}}`
		}
		return 0, ""
	}
	if _, err := c.Charge(context.Background(), "k", "wal_cust", 6000, "n"); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("got %v, want ErrCurrencyMismatch", err)
	}
}

// The operating wallet is NGN-currency; transfers need a USDT wallet of its own, which is created once
// (under a stable idempotency key) when it does not exist yet.
func TestOperatingUSDTWalletIsCreatedWhenMissing(t *testing.T) {
	s, c := newStub(t)
	s.reply = func(r stubReq) (int, string) {
		if r.Method == "GET" && r.Path == "/v1/wallets" {
			return 200, `{"wallets":[{"wallet_id":"wal_op","owner_ref":"op","currency":"NGN"}],"total":1}`
		}
		if r.Method == "POST" && r.Path == "/v1/wallets" {
			if r.Body["currency"] != "USDT" || r.Body["owner_ref"] != "op" || r.Body["owner_type"] != "client" || r.Body["client_id"] != "xenos" {
				t.Errorf("create body: %+v", r.Body)
			}
			return 201, `{"wallet_id":"wal_new_usdt","currency":"USDT"}`
		}
		return 0, ""
	}
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := c.Charge(ctx, "k"+string(rune('a'+i)), "wal_cust", 6000, "n"); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.last().Body["to_wallet_id"]; got != "wal_new_usdt" {
		t.Fatalf("charged into %v", got)
	}
	creates := 0
	for _, r := range s.reqs {
		if r.Method == "POST" && r.Path == "/v1/wallets" {
			creates++
			if r.IdemKey != "usdt-wallet:wal_op" {
				t.Errorf("idempotency key %q", r.IdemKey)
			}
		}
	}
	if creates != 1 {
		t.Fatalf("created %d times, want once", creates)
	}
}
