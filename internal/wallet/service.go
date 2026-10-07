// Package wallet turns naira deposits into USDT compute credit through iSpend.
// The exchange rate is applied exactly once, here; usage is metered in USDT
// (see internal/metering), so later rate moves never change credit already bought.
package wallet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/israel-duff/xenos/internal/billing"
	"github.com/israel-duff/xenos/internal/jobs"
	"github.com/israel-duff/xenos/internal/store"
	"github.com/israel-duff/xenos/internal/store/db"
)

// ErrBadEvent marks a delivery that is validly signed but cannot be acted on (the webhook answers 400).
var ErrBadEvent = errors.New("wallet: malformed event")

const (
	JobConversion = "conversion.run"

	// MinConversionKobo is the smallest manual conversion (₦100).
	MinConversionKobo = 100 * 100
)

// Alerter delivers operator alerts (satisfied by *alert.Notifier). It may be nil.
type Alerter interface {
	Notify(ctx context.Context, key string, cooldown time.Duration, text string)
}

type Service struct {
	Store   *store.Store
	ISpend  billing.ISpend
	Cache   *billing.BalanceCache
	Log     *slog.Logger
	Alerter Alerter
	Now     func() time.Time // defaults to time.Now
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) alert(ctx context.Context, key string, cooldown time.Duration, text string) {
	if s.Alerter != nil {
		s.Alerter.Notify(ctx, key, cooldown, text)
	}
}

type convPayload struct {
	ConversionID int64 `json:"conversion_id"`
}

func (s *Service) Handlers() map[string]jobs.Handler {
	return map[string]jobs.Handler{
		JobConversion: func(ctx context.Context, j *jobs.Job) error {
			var p convPayload
			if err := json.Unmarshal(j.Payload, &p); err != nil {
				return fmt.Errorf("bad payload: %w", err)
			}
			return s.RunConversion(ctx, p.ConversionID)
		},
	}
}

// Sweep re-queues pending conversions that have no live job, for example after
// a long iSpend outage exhausted the job's retries. The naira is safe in the
// NGN wallet meanwhile; this makes sure it is eventually converted.
func (s *Service) Sweep(ctx context.Context, jq *jobs.Queue) error {
	ids, err := s.Store.Q.ListStalledConversions(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := jq.Enqueue(ctx, JobConversion, convPayload{ConversionID: id}); err != nil {
			return err
		}
	}
	return nil
}

// HandleWebhook acts on a verified iswallet delivery. Anything it does not need is acknowledged
// and ignored, so iswallet stops retrying it.
func (s *Service) HandleWebhook(ctx context.Context, ev billing.Event) error {
	switch ev.Type {
	case billing.EventCreditPosted:
		// Only naira arriving by bank transfer is a customer deposit. A USDT credit (a conversion or
		// an adjustment we made ourselves) is not, and must never be converted again.
		if ev.Currency != "NGN" || ev.SourceType != billing.SourceBankInflow {
			s.Log.Info("ignoring credit that is not a naira bank deposit", "currency", ev.Currency, "source", ev.SourceType, "txn", ev.TxnID)
			return nil
		}
		return s.HandleDeposit(ctx, ev)
	case billing.EventCreditReversed:
		return s.HandleReversal(ctx, ev)
	default:
		s.Log.Info("ignoring webhook event", "type", ev.Type, "txn", ev.TxnID)
		return nil
	}
}

// HandleDeposit records a confirmed naira deposit. It is safe to call any number of times for
// the same ledger event: the event is stored once (keyed by iswallet's transaction id), so a
// redelivery does nothing. With auto-convert on, the conversion itself runs in a job so the
// webhook answers immediately and iswallet outages are retried.
func (s *Service) HandleDeposit(ctx context.Context, ev billing.Event) error {
	user, err := s.Store.Q.GetUserByISpendCustomer(ctx, textOf(ev.WalletID))
	if errors.Is(err, pgx.ErrNoRows) {
		s.Log.Warn("deposit for an unknown wallet", "wallet", ev.WalletID, "txn", ev.TxnID)
		return nil // not ours to act on; acknowledge so iswallet stops retrying
	} else if err != nil {
		return err
	}
	if ev.Amount <= 0 {
		return fmt.Errorf("%w: deposit %s has a non-positive amount", ErrBadEvent, ev.DedupeKey())
	}
	return s.Store.InTx(ctx, func(q *db.Queries, tx pgx.Tx) error {
		if n, err := q.InsertWebhookEvent(ctx, ev.DedupeKey()); err != nil {
			return err
		} else if n == 0 {
			return nil // redelivery
		}
		s.Cache.Invalidate(ev.WalletID)
		if user.Status == "closing" || user.Status == "closed" {
			// Money arrived for an account that is closing or closed: do not convert it. An operator decides
			// (usually a refund to the sender), because nobody can use it.
			s.alert(ctx, "deposit-closed:"+ev.DedupeKey(), 0, fmt.Sprintf(
				"A deposit of %d kobo arrived for a %s account (user %d, wallet %s). It was not converted: refund it or reopen the account.",
				ev.Amount, user.Status, user.ID, ev.WalletID))
			return nil
		}
		if !user.AutoConvert || !user.EmailVerifiedAt.Valid {
			// The naira stays in the NGN balance: the customer asked for that, or has not
			// verified their email yet (it is converted when they do).
			return nil
		}
		id, err := q.CreateConversion(ctx, db.CreateConversionParams{
			UserID: user.ID, AmountNgnKobo: ev.Amount, DepositEventID: textOf("deposit:" + ev.DedupeKey())})
		if err != nil {
			return err
		}
		return jobs.EnqueueTx(ctx, tx, JobConversion, convPayload{ConversionID: id})
	})
}

// HandleReversal records a confirmed deposit that iswallet later clawed back. The reversal can
// take the wallet negative; iswallet bears that loss and the customer keeps the credit they were
// given, so nothing is clawed back here. We record it and tell an operator.
func (s *Service) HandleReversal(ctx context.Context, ev billing.Event) error {
	user, err := s.Store.Q.GetUserByISpendCustomer(ctx, textOf(ev.WalletID))
	var uid pgtype.Int8
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		s.Log.Warn("reversal for an unknown wallet", "wallet", ev.WalletID, "txn", ev.TxnID)
	case err != nil:
		return err
	default:
		uid = pgtype.Int8{Int64: user.ID, Valid: true}
	}
	n, err := s.Store.Q.InsertDepositReversal(ctx, db.InsertDepositReversalParams{
		EventKey: ev.DedupeKey(), UserID: uid, WalletID: ev.WalletID, AmountKobo: ev.Amount,
		UncoveredKobo: ev.UncoveredAmount, OriginalRef: ev.ReversedProviderRef})
	if err != nil || n == 0 {
		return err // n == 0: a redelivery
	}
	s.Cache.Invalidate(ev.WalletID)
	who := ev.WalletID
	if uid.Valid {
		who = user.Email
	}
	s.alert(ctx, "deposit-reversed:"+ev.DedupeKey(), 0, fmt.Sprintf(
		"A confirmed deposit was reversed for %s: %d kobo (original %s), %d kobo of it already spent. iswallet bears the loss and the customer keeps their credit; review the account if this looks abnormal.",
		who, ev.Amount, ev.ReversedProviderRef, ev.UncoveredAmount))
	return nil
}

// ConvertHeldOnVerify converts naira that arrived while the customer's email was
// unverified, if they use auto-convert. It runs once, when verification succeeds.
func (s *Service) ConvertHeldOnVerify(ctx context.Context, userID int64) error {
	user, err := s.Store.Q.GetUserByID(ctx, userID)
	if err != nil || !user.AutoConvert || !user.IspendCustomerID.Valid {
		return err
	}
	bal, err := s.ISpend.Balances(ctx, user.IspendCustomerID.String)
	if err != nil || bal.NGNKobo < MinConversionKobo {
		return err
	}
	return s.Store.InTx(ctx, func(q *db.Queries, tx pgx.Tx) error {
		id, err := q.CreateConversion(ctx, db.CreateConversionParams{
			UserID: userID, AmountNgnKobo: bal.NGNKobo, DepositEventID: textOf("verify:" + strconv.FormatInt(userID, 10))})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		} else if err != nil {
			return err
		}
		return jobs.EnqueueTx(ctx, tx, JobConversion, convPayload{ConversionID: id})
	})
}

// Quote asks iSpend for a conversion quote and remembers it, so confirming
// converts exactly what the customer was shown.
func (s *Service) Quote(ctx context.Context, userID int64, customerID string, amountKobo int64) (billing.Quote, error) {
	q, err := s.ISpend.Quote(ctx, customerID, amountKobo)
	if err != nil {
		return billing.Quote{}, err
	}
	err = s.Store.Q.SaveQuote(ctx, db.SaveQuoteParams{ID: q.ID, UserID: userID, AmountNgnKobo: q.AmountNGN, AmountUusdt: q.AmountUSDT,
		Rate: q.Rate, ExpiresAt: pgtype.Timestamptz{Time: q.ExpiresAt, Valid: !q.ExpiresAt.IsZero()}})
	return q, err
}

var ErrUnknownQuote = errors.New("wallet: unknown quote")

// StartManual converts a quote the customer confirmed and runs it. The quote id
// is the dedupe key, so a double click converts once. If iSpend is unreachable the
// conversion stays pending and a job retries it against the same quote; if the
// quote has expired it fails and the customer must request a new one.
func (s *Service) StartManual(ctx context.Context, jq *jobs.Queue, userID int64, quoteID string) (db.Conversion, error) {
	q, err := s.Store.Q.GetUserQuote(ctx, db.GetUserQuoteParams{ID: quoteID, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Conversion{}, ErrUnknownQuote
	} else if err != nil {
		return db.Conversion{}, err
	}
	if q.ExpiresAt.Valid && !s.now().Before(q.ExpiresAt.Time) {
		return db.Conversion{}, billing.ErrQuoteExpired // quotes live 60 seconds; do not even ask
	}
	key := "quote:" + q.ID
	id, err := s.Store.Q.CreateConversion(ctx, db.CreateConversionParams{
		UserID: userID, AmountNgnKobo: q.AmountNgnKobo, DepositEventID: textOf(key), IspendQuoteID: textOf(q.ID)})
	if errors.Is(err, pgx.ErrNoRows) { // an earlier click already created it
		return s.Store.Q.GetConversionByKey(ctx, textOf(key))
	} else if err != nil {
		return db.Conversion{}, err
	}
	if err := s.RunConversion(ctx, id); err != nil {
		s.Log.Warn("manual conversion deferred", "conversion", id, "err", err)
		if qerr := jq.Enqueue(ctx, JobConversion, convPayload{ConversionID: id}); qerr != nil {
			return db.Conversion{}, qerr
		}
	}
	return s.Store.Q.GetConversion(ctx, id)
}

// convertKey is the iswallet idempotency key for one attempt of a conversion. The first attempt
// is "conversion:<id>"; a later one exists only if the earlier quote expired without executing.
func convertKey(id int64, attempt int32) string {
	if attempt <= 1 {
		return "conversion:" + strconv.FormatInt(id, 10)
	}
	return fmt.Sprintf("conversion:%d:%d", id, attempt)
}

// RunConversion executes one conversion against iswallet and is safe to repeat.
//
// iswallet rejects a repeated key with a different payload, so the quote is persisted BEFORE it is
// executed and every retry replays that same quote under that same key. A replay returns the
// original result even after the quote has expired, so a conversion that executed but whose answer
// we lost can never run twice. Only when the quote expired and was never executed (the replay says
// QUOTE_EXPIRED) does an automatic conversion take a fresh quote under a new key; a customer's own
// quote is never silently replaced, so they never get a rate they did not see.
func (s *Service) RunConversion(ctx context.Context, id int64) error {
	c, err := s.Store.Q.GetConversion(ctx, id)
	if err != nil {
		return err
	}
	if c.Status != "pending" {
		return nil
	}
	user, err := s.Store.Q.GetUserByID(ctx, c.UserID)
	if err != nil {
		return err
	}
	if !user.IspendCustomerID.Valid {
		return errors.New("user has no iswallet wallet yet")
	}
	cust := user.IspendCustomerID.String
	manual := strings.HasPrefix(c.DepositEventID.String, "quote:")
	retry := func(err error) error { // leave pending; the job and the sweep try again
		_ = s.Store.Q.SetConversionError(ctx, db.SetConversionErrorParams{ID: id, LastError: textOf(err.Error())})
		return err
	}
	giveUp := func(err error) error { // retrying cannot help
		return s.Store.Q.FailConversion(ctx, db.FailConversionParams{ID: id, LastError: textOf(err.Error())})
	}

	var quote billing.Quote
	if c.IspendQuoteID.Valid {
		saved, err := s.Store.Q.GetUserQuote(ctx, db.GetUserQuoteParams{ID: c.IspendQuoteID.String, UserID: c.UserID})
		if err != nil {
			return err
		}
		quote = billing.Quote{ID: saved.ID, AmountNGN: saved.AmountNgnKobo, AmountUSDT: saved.AmountUusdt, Rate: saved.Rate}
	} else {
		fresh, err := s.ISpend.Quote(ctx, cust, c.AmountNgnKobo)
		if err != nil {
			if errors.Is(err, billing.ErrLiquidity) {
				s.liquidityAlert(ctx)
			}
			return retry(err) // rates paused or iswallet down: the naira is safe in the NGN balance meanwhile
		}
		if err := s.Store.Q.SaveQuote(ctx, db.SaveQuoteParams{ID: fresh.ID, UserID: c.UserID, AmountNgnKobo: fresh.AmountNGN,
			AmountUusdt: fresh.AmountUSDT, Rate: fresh.Rate, ExpiresAt: pgtype.Timestamptz{Time: fresh.ExpiresAt, Valid: !fresh.ExpiresAt.IsZero()}}); err != nil {
			return err
		}
		if err := s.Store.Q.SetConversionQuote(ctx, db.SetConversionQuoteParams{ID: id, IspendQuoteID: textOf(fresh.ID)}); err != nil {
			return err
		}
		quote = fresh
	}

	mv, err := s.ISpend.Convert(ctx, convertKey(id, c.ConvertAttempt), cust, quote.ID)
	switch {
	case err == nil:
	case errors.Is(err, billing.ErrInsufficientFunds):
		_ = retry(err)
		return giveUp(err)
	case errors.Is(err, billing.ErrQuoteExpired):
		if manual {
			_ = retry(err)
			return giveUp(err) // the customer must request a new quote
		}
		// Never executed (a replay of an executed key would have returned its result), so a new
		// quote under a new key is safe.
		if nerr := s.Store.Q.NextConversionAttempt(ctx, id); nerr != nil {
			return nerr
		}
		return retry(err)
	case errors.Is(err, billing.ErrIdempotencyKeyReused):
		// Not retryable: a different payload under a key we already used is a bug in how we build keys.
		s.alert(ctx, fmt.Sprintf("conversion-key-reused-%d", id), 0, fmt.Sprintf(
			"Conversion %d: iswallet refused the idempotency key as reused with a different payload. This is a bug in our key handling; do not retry blindly.", id))
		_ = retry(err)
		return giveUp(err)
	case errors.Is(err, billing.ErrQuoteUsed):
		s.alert(ctx, fmt.Sprintf("conversion-quote-used-%d", id), 0, fmt.Sprintf(
			"Conversion %d: iswallet says its quote was already executed under a different key. Check the ledger before doing anything else.", id))
		_ = retry(err)
		return giveUp(err)
	case errors.Is(err, billing.ErrLiquidity):
		s.liquidityAlert(ctx)
		return retry(err) // iswallet's inventory, not the customer's problem: hold and try again later
	default:
		return retry(err)
	}

	s.Cache.Invalidate(cust)
	// The quote is the binding promise, so it is what we record. iswallet's execute response also
	// reports the credit; if the two ever disagree, say so rather than silently trusting either.
	credited := quote.AmountUSDT
	if mv.CreditUUSDT != 0 && mv.CreditUUSDT != quote.AmountUSDT {
		s.Log.Error("convert credit differs from the quote", "conversion", id, "quote_uusdt", quote.AmountUSDT, "convert_uusdt", mv.CreditUUSDT)
		s.alert(ctx, fmt.Sprintf("convert-credit-mismatch-%d", id), 0, fmt.Sprintf(
			"Conversion %d: iswallet's execute response says it credited %d micro-USDT but the quote promised %d. We recorded the quote. Check the customer's balance against the ledger.",
			id, mv.CreditUUSDT, quote.AmountUSDT))
	}
	return s.Store.Q.CompleteConversion(ctx, db.CompleteConversionParams{
		ID: id, AmountUusdt: pgtype.Int8{Int64: credited, Valid: true}, Column3: quote.Rate,
		IspendQuoteID: textOf(quote.ID), IspendMovementID: textOf(mv.ID)})
}

func (s *Service) liquidityAlert(ctx context.Context) {
	s.alert(ctx, "iswallet-liquidity", time.Hour,
		"iswallet refused a naira conversion for lack of USDT liquidity (INSUFFICIENT_LIQUIDITY). Customer naira is safe and conversions are being retried; tell iswallet their USDT inventory needs topping up for our volume.")
}

func textOf(v string) pgtype.Text { return pgtype.Text{String: v, Valid: true} }
