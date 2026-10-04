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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/israel-duff/xenos/internal/billing"
	"github.com/israel-duff/xenos/internal/jobs"
	"github.com/israel-duff/xenos/internal/store"
	"github.com/israel-duff/xenos/internal/store/db"
)

const (
	JobConversion = "conversion.run"

	// MinConversionKobo is the smallest manual conversion (₦100).
	MinConversionKobo = 100 * 100
)

type Service struct {
	Store  *store.Store
	ISpend billing.ISpend
	Cache  *billing.BalanceCache
	Log    *slog.Logger
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

// HandleDeposit records a verified deposit event. It is safe to call any number
// of times with the same event: the event id is stored once, so a replay does
// nothing. With auto-convert on, the conversion itself runs in a job so the
// webhook can answer immediately and iSpend outages are retried.
func (s *Service) HandleDeposit(ctx context.Context, ev billing.Event) error {
	user, err := s.Store.Q.GetUserByISpendCustomer(ctx, textOf(ev.CustomerID))
	if errors.Is(err, pgx.ErrNoRows) {
		s.Log.Warn("deposit for unknown customer", "customer", ev.CustomerID, "event", ev.ID)
		return nil // nothing we can do; acknowledge so iSpend stops retrying
	} else if err != nil {
		return err
	}
	if ev.AmountKobo <= 0 {
		return fmt.Errorf("deposit event %s has non-positive amount", ev.ID)
	}
	return s.Store.InTx(ctx, func(q *db.Queries, tx pgx.Tx) error {
		if n, err := q.InsertWebhookEvent(ctx, ev.ID); err != nil {
			return err
		} else if n == 0 {
			return nil // replay
		}
		s.Cache.Invalidate(ev.CustomerID)
		if !user.AutoConvert {
			return nil // the naira stays in the NGN wallet until the customer converts
		}
		id, err := q.CreateConversion(ctx, db.CreateConversionParams{
			UserID: user.ID, AmountNgnKobo: ev.AmountKobo, DepositEventID: textOf("deposit:" + ev.ID)})
		if err != nil {
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
	err = s.Store.Q.SaveQuote(ctx, db.SaveQuoteParams{ID: q.ID, UserID: userID, AmountNgnKobo: q.AmountNGN, AmountUusdt: q.AmountUSDT, Rate: q.Rate})
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

// RunConversion executes one conversion against iSpend. It is idempotent: the
// iSpend idempotency key is the conversion id, so retries after a timeout or a
// crash cannot convert twice. Automatic conversions take a fresh quote on every
// attempt; manual ones keep the quote the customer confirmed.
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
		return errors.New("user has no iSpend customer yet")
	}
	cust := user.IspendCustomerID.String
	fail := func(err error) error {
		_ = s.Store.Q.SetConversionError(ctx, db.SetConversionErrorParams{ID: id, LastError: textOf(err.Error())})
		return err
	}

	var quote billing.Quote
	if c.IspendQuoteID.Valid {
		saved, err := s.Store.Q.GetUserQuote(ctx, db.GetUserQuoteParams{ID: c.IspendQuoteID.String, UserID: c.UserID})
		if err != nil {
			return err
		}
		quote = billing.Quote{ID: saved.ID, AmountNGN: saved.AmountNgnKobo, AmountUSDT: saved.AmountUusdt, Rate: saved.Rate}
	} else {
		if quote, err = s.ISpend.Quote(ctx, cust, c.AmountNgnKobo); err != nil {
			return fail(err) // the job retries; the naira is safe in the NGN wallet meanwhile
		}
	}

	mv, err := s.ISpend.Convert(ctx, "conversion:"+strconv.FormatInt(id, 10), cust, quote.ID)
	if errors.Is(err, billing.ErrInsufficientFunds) || (errors.Is(err, billing.ErrQuoteExpired) && c.IspendQuoteID.Valid) {
		// Retrying cannot help: the NGN wallet lacks the amount, or the customer's quote is stale.
		_ = fail(err)
		return s.Store.Q.FailConversion(ctx, db.FailConversionParams{ID: id, LastError: textOf(err.Error())})
	} else if err != nil {
		return fail(err)
	}
	s.Cache.Invalidate(cust)
	return s.Store.Q.CompleteConversion(ctx, db.CompleteConversionParams{
		ID: id, AmountUusdt: pgtype.Int8{Int64: quote.AmountUSDT, Valid: true}, Column3: quote.Rate,
		IspendQuoteID: textOf(quote.ID), IspendMovementID: textOf(mv.ID)})
}

func textOf(v string) pgtype.Text { return pgtype.Text{String: v, Valid: true} }
