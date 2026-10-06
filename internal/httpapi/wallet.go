package httpapi

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/israel-duff/xenos/internal/billing"
	"github.com/israel-duff/xenos/internal/store/db"
	"github.com/israel-duff/xenos/internal/wallet"
)

type virtualAccountJSON struct {
	Bank          string `json:"bank"`
	AccountNumber string `json:"account_number"`
	AccountName   string `json:"account_name"`
}

type conversionJSON struct {
	ID         int64     `json:"id"`
	AmountKobo int64     `json:"amount_ngn_kobo"`
	AmountUSDT *int64    `json:"amount_uusdt"`
	Rate       *string   `json:"rate"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
}

type chargeJSON struct {
	ID       int64     `json:"id"`
	VMID     int64     `json:"vm_id"`
	Hostname string    `json:"hostname"`
	Hour     time.Time `json:"hour"`
	Amount   int64     `json:"amount_uusdt"`
	Status   string    `json:"status"`
}

type walletJSON struct {
	NGNKobo          int64               `json:"ngn_kobo"`
	USDTMicro        int64               `json:"usdt_uusdt"`
	RateKobo         *int64              `json:"rate_kobo_per_usdt"` // null when iSpend cannot quote
	USDTInKobo       *int64              `json:"usdt_in_ngn_kobo"`   // display only
	HourlyUUSDT      int64               `json:"hourly_uusdt"`
	RunwayHours      *int64              `json:"runway_hours"` // null when nothing is running
	UnpaidUUSDT      int64               `json:"unpaid_uusdt"`
	AutoConvert      bool                `json:"auto_convert"`
	EmailVerified    bool                `json:"email_verified"`
	QuotingPaused    bool                `json:"quoting_paused"`
	GraceEndsAt      *time.Time          `json:"grace_ends_at"`
	VirtualAccount   *virtualAccountJSON `json:"virtual_account"`    // only after email verification
	DepositLimitKobo int64               `json:"deposit_limit_kobo"` // per transfer and per day on a basic account; 0 = unknown
	Conversions      []conversionJSON    `json:"conversions"`
	Charges          []chargeJSON        `json:"charges"`
}

func (s *Server) getWallet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := principalFrom(ctx).User
	if !u.IspendCustomerID.Valid {
		writeErr(w, http.StatusServiceUnavailable, "wallet is not available yet, try again shortly")
		return
	}
	cust := u.IspendCustomerID.String
	bal, err := s.Cache.Get(ctx, cust)
	if err != nil {
		s.Log.Error("ispend balances", "user_id", u.ID, "err", err)
		writeErr(w, http.StatusServiceUnavailable, "wallet is not available right now, try again shortly")
		return
	}

	out := walletJSON{NGNKobo: bal.NGNKobo, USDTMicro: bal.USDTMicro, AutoConvert: u.AutoConvert,
		EmailVerified: u.EmailVerifiedAt.Valid, Conversions: []conversionJSON{}, Charges: []chargeJSON{}}
	if rate, err := s.ISpend.Rate(ctx); err == nil && rate > 0 {
		out.RateKobo = &rate
		inKobo := bal.USDTMicro * rate / 1_000_000
		out.USDTInKobo = &inKobo
	} else {
		out.QuotingPaused = true // deposits stay safe in the NGN wallet until quoting resumes
	}
	if u.GraceStartedAt.Valid {
		end := u.GraceStartedAt.Time.Add(s.Cfg.Grace())
		out.GraceEndsAt = &end
	}
	if u.EmailVerifiedAt.Valid { // funding details are withheld until the email is verified
		if !u.VaAccountNumber.Valid {
			// Not issued at signup (iswallet was down): issue it now. The call is idempotent.
			if c, err := s.ISpend.Customer(ctx, cust); err == nil {
				s.saveVirtualAccount(ctx, &u, c)
			} else {
				s.Log.Warn("virtual account not available yet", "user_id", u.ID, "err", err)
			}
		}
		if u.VaAccountNumber.Valid {
			out.VirtualAccount = &virtualAccountJSON{Bank: u.VaBank.String, AccountNumber: u.VaAccountNumber.String, AccountName: u.VaAccountName.String}
		}
	}
	out.DepositLimitKobo = s.Cfg.DepositLimitKobo

	if out.HourlyUUSDT, err = s.Store.Q.UserHourlyRate(ctx, u.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	if out.HourlyUUSDT > 0 {
		h := billing.RunwayHours(bal.USDTMicro, out.HourlyUUSDT)
		out.RunwayHours = &h
	}
	if out.UnpaidUUSDT, err = s.Store.Q.UserUnpaidTotal(ctx, u.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	convs, err := s.Store.Q.ListUserConversions(ctx, u.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for _, c := range convs {
		cj := conversionJSON{ID: c.ID, AmountKobo: c.AmountNgnKobo, Status: c.Status, CreatedAt: c.CreatedAt}
		if c.AmountUusdt.Valid {
			cj.AmountUSDT = &c.AmountUusdt.Int64
		}
		if c.Rate != "" {
			rate := c.Rate
			cj.Rate = &rate
		}
		out.Conversions = append(out.Conversions, cj)
	}
	charges, err := s.Store.Q.ListUserCharges(ctx, u.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for _, c := range charges {
		out.Charges = append(out.Charges, chargeJSON{c.ID, c.VmID, c.Hostname, c.Hour, c.AmountUusdt, c.Status})
	}
	writeJSON(w, http.StatusOK, out)
}

// convert is two-step. Without quote_id it returns a quote to show the customer;
// with quote_id it converts exactly that quote.
func (s *Server) convert(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := principalFrom(ctx).User
	var in struct {
		AmountKobo int64  `json:"amount_ngn_kobo"`
		QuoteID    string `json:"quote_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !u.IspendCustomerID.Valid {
		writeErr(w, http.StatusServiceUnavailable, "wallet is not available yet, try again shortly")
		return
	}
	cust := u.IspendCustomerID.String

	if in.QuoteID != "" {
		conv, err := s.Wallet.StartManual(ctx, s.Jobs, u.ID, in.QuoteID)
		if errors.Is(err, wallet.ErrUnknownQuote) {
			writeErr(w, http.StatusBadRequest, "unknown quote, request a new one")
			return
		} else if errors.Is(err, billing.ErrQuoteExpired) {
			writeErr(w, http.StatusConflict, "this quote has expired (quotes last 60 seconds); request a new one")
			return
		} else if err != nil {
			s.fail(w, r, err)
			return
		}
		status := http.StatusOK
		if conv.Status == "pending" {
			status = http.StatusAccepted // iSpend is slow or down; a job will finish it
		}
		cj := conversionJSON{ID: conv.ID, AmountKobo: conv.AmountNgnKobo, Status: conv.Status, CreatedAt: conv.CreatedAt}
		if conv.AmountUusdt.Valid {
			cj.AmountUSDT = &conv.AmountUusdt.Int64
		}
		if conv.Status == "failed" {
			writeErr(w, http.StatusConflict, "this quote can no longer be used, request a new one")
			return
		}
		writeJSON(w, status, cj)
		return
	}

	if in.AmountKobo < wallet.MinConversionKobo {
		writeErr(w, http.StatusBadRequest, "amount is below the minimum conversion")
		return
	}
	bal, err := s.ISpend.Balances(ctx, cust)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "wallet is not available right now, try again shortly")
		return
	}
	if in.AmountKobo > bal.NGNKobo {
		writeErr(w, http.StatusConflict, "amount is more than your naira balance")
		return
	}
	q, err := s.Wallet.Quote(ctx, u.ID, cust, in.AmountKobo)
	if err != nil {
		s.Log.Warn("quote failed", "user_id", u.ID, "err", err)
		writeErr(w, http.StatusServiceUnavailable, "conversion is paused right now, your naira is safe; try again later")
		return
	}
	resp := map[string]any{"quote_id": q.ID, "amount_ngn_kobo": q.AmountNGN, "amount_uusdt": q.AmountUSDT, "rate": q.Rate, "expires_at": q.ExpiresAt}
	if hourly, err := s.Store.Q.UserHourlyRate(ctx, u.ID); err == nil && hourly > 0 {
		resp["added_runway_hours"] = q.AmountUSDT / hourly
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) walletSettings(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AutoConvert *bool `json:"auto_convert"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.AutoConvert == nil {
		writeErr(w, http.StatusBadRequest, "auto_convert is required")
		return
	}
	if err := s.Store.Q.UpdateAutoConvert(r.Context(), db.UpdateAutoConvertParams{ID: principalFrom(r.Context()).User.ID, AutoConvert: *in.AutoConvert}); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"auto_convert": *in.AutoConvert})
}

// ispendWebhook receives iswallet events. It is authenticated by signature, not session.
func (s *Server) ispendWebhook(w http.ResponseWriter, r *http.Request) {
	// The signature covers the raw bytes, so read them before anything parses or re-serialises.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "unreadable body")
		return
	}
	ev, err := billing.ParseWebhook(s.Cfg.ISpendWebhookSecret, r.Header, body, time.Now())
	if errors.Is(err, billing.ErrBadSignature) {
		// Alert-worthy: repeated failures mean a wrong secret or someone probing.
		s.Log.Warn("webhook signature failure", "ip", s.clientIP(r))
		// Cap what a flood can write: the monitor only needs a count above its threshold.
		if s.webhookFailLimit.Allow(s.clientIP(r)) {
			if err := s.Store.Q.RecordWebhookFailure(r.Context(), s.clientIP(r)); err != nil {
				s.Log.Error("record webhook failure", "err", err)
			}
		}
		writeErr(w, http.StatusUnauthorized, "invalid signature")
		return
	} else if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Wallet.HandleWebhook(r.Context(), ev); errors.Is(err, wallet.ErrBadEvent) {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	} else if err != nil {
		s.fail(w, r, err) // 5xx makes iswallet redeliver (10 attempts, backed off), which is safe
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
