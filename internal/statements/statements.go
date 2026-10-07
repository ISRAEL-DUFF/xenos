// Package statements builds a customer's statement for one UTC calendar month from what was actually charged:
// every number is a sum of integer micro-USDT rows in usage_charges, conversions and adjustments.
package statements

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/israel-duff/xenos/internal/store"
	"github.com/israel-duff/xenos/internal/store/db"
)

// MonthLayout is how a month is written in URLs and statements: 2026-10.
const MonthLayout = "2006-01"

// ParseMonth reads "2026-10" and returns the half-open UTC range [from, to) of that month.
func ParseMonth(s string) (from, to time.Time, err error) {
	t, err := time.ParseInLocation(MonthLayout, s, time.UTC)
	if err != nil {
		return from, to, fmt.Errorf("a month looks like 2026-10")
	}
	if t.Year() < 2020 || t.After(time.Now().UTC().AddDate(0, 1, 0)) {
		return from, to, fmt.Errorf("that month is out of range")
	}
	return t, t.AddDate(0, 1, 0), nil
}

type VMLine struct {
	VMID          int64             `json:"vm_id"`
	Hostname      string            `json:"hostname"`
	Plan          string            `json:"plan"` // the plan the VM is on now; each hour's price is in the CSV
	Labels        map[string]string `json:"labels"`
	ChargedHours  int               `json:"charged_hours"`
	CappedHours   int               `json:"capped_hours"` // hours that cost nothing because the monthly cap was reached
	ChargedUUSDT  int64             `json:"charged_uusdt"`
	RefundedUUSDT int64             `json:"refunded_uusdt"`
}

type GroupLine struct {
	Value        string `json:"value"` // the label's value, or "(none)"
	VMs          int    `json:"vms"`
	ChargedUUSDT int64  `json:"charged_uusdt"`
}

type ConversionLine struct {
	ID         int64     `json:"id"`
	At         time.Time `json:"at"`
	AmountKobo int64     `json:"amount_ngn_kobo"`
	USDT       int64     `json:"amount_uusdt"`
	Rate       string    `json:"rate_kobo_per_usdt"`
}

type AdjustmentLine struct {
	ID    int64     `json:"id"`
	At    time.Time `json:"at"`
	UUSDT int64     `json:"amount_uusdt"` // positive credits the account
	Note  string    `json:"note"`
}

type Totals struct {
	ChargedUUSDT  int64 `json:"charged_uusdt"`
	RefundedUUSDT int64 `json:"refunded_uusdt"`
	ChargedHours  int   `json:"charged_hours"`
	CappedHours   int   `json:"capped_hours"`
	// UnpaidUUSDT is everything currently unpaid (any month): what the account still owes.
	UnpaidUUSDT int64 `json:"unpaid_uusdt"`
}

type Statement struct {
	Month       string           `json:"month"`
	From        time.Time        `json:"from"`
	To          time.Time        `json:"to"`
	Totals      Totals           `json:"totals"`
	VMs         []VMLine         `json:"vms"`
	GroupBy     string           `json:"group_by,omitempty"`
	Groups      []GroupLine      `json:"groups,omitempty"`
	Conversions []ConversionLine `json:"conversions"`
	Adjustments []AdjustmentLine `json:"adjustments"`
}

// Build assembles the month's statement. groupLabel, when set, also totals the VMs by that label's value.
func Build(ctx context.Context, st *store.Store, userID int64, month, groupLabel string) (Statement, error) {
	from, to, err := ParseMonth(month)
	if err != nil {
		return Statement{}, err
	}
	out := Statement{Month: month, From: from, To: to, VMs: []VMLine{}, Conversions: []ConversionLine{}, Adjustments: []AdjustmentLine{}}
	rows, err := st.Q.StatementCharges(ctx, db.StatementChargesParams{UserID: userID, Hour: from, Hour_2: to})
	if err != nil {
		return out, err
	}
	byVM := map[int64]*VMLine{}
	var order []int64 // the query is ordered by VM id: keep that order
	for _, r := range rows {
		l := byVM[r.VmID]
		if l == nil {
			l = &VMLine{VMID: r.VmID, Hostname: r.Hostname, Plan: r.PlanSlug, Labels: labelsOf(r.Labels)}
			byVM[r.VmID] = l
			order = append(order, r.VmID)
		}
		switch {
		case r.Status == "refunded":
			l.RefundedUUSDT += r.AmountUusdt
		case r.AmountUusdt == 0:
			l.CappedHours++
		default:
			l.ChargedHours++
			l.ChargedUUSDT += r.AmountUusdt
		}
	}
	for _, id := range order {
		l := *byVM[id]
		out.VMs = append(out.VMs, l)
		out.Totals.ChargedUUSDT += l.ChargedUUSDT
		out.Totals.RefundedUUSDT += l.RefundedUUSDT
		out.Totals.ChargedHours += l.ChargedHours
		out.Totals.CappedHours += l.CappedHours
	}
	if out.Totals.UnpaidUUSDT, err = st.Q.StatementUnpaid(ctx, userID); err != nil {
		return out, err
	}
	convs, err := st.Q.StatementConversions(ctx, db.StatementConversionsParams{UserID: userID, CreatedAt: from, CreatedAt_2: to})
	if err != nil {
		return out, err
	}
	for _, c := range convs {
		out.Conversions = append(out.Conversions, ConversionLine{ID: c.ID, At: c.CreatedAt, AmountKobo: c.AmountNgnKobo, USDT: c.AmountUusdt.Int64, Rate: c.Rate})
	}
	adjs, err := st.Q.StatementAdjustments(ctx, db.StatementAdjustmentsParams{UserID: userID, CreatedAt: from, CreatedAt_2: to})
	if err != nil {
		return out, err
	}
	for _, a := range adjs {
		out.Adjustments = append(out.Adjustments, AdjustmentLine{ID: a.ID, At: a.CreatedAt, UUSDT: a.AmountUusdt, Note: a.Note})
	}
	if groupLabel != "" {
		out.GroupBy = "label:" + groupLabel
		out.Groups = group(out.VMs, groupLabel)
	}
	return out, nil
}

func group(vms []VMLine, key string) []GroupLine {
	m := map[string]*GroupLine{}
	for _, v := range vms {
		val, ok := v.Labels[key]
		if !ok {
			val = "(none)"
		}
		g := m[val]
		if g == nil {
			g = &GroupLine{Value: val}
			m[val] = g
		}
		g.VMs++
		g.ChargedUUSDT += v.ChargedUUSDT
	}
	out := make([]GroupLine, 0, len(m))
	for _, g := range m {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Value < out[j].Value })
	return out
}

func labelsOf(raw []byte) map[string]string {
	m := map[string]string{}
	_ = json.Unmarshal(raw, &m)
	return m
}

// WriteCSV writes one row per charged hour. Cells that a spreadsheet would read as a formula are prefixed
// with an apostrophe, because hostnames and labels are customer-controlled text.
func WriteCSV(ctx context.Context, w io.Writer, st *store.Store, userID int64, month string) error {
	from, to, err := ParseMonth(month)
	if err != nil {
		return err
	}
	rows, err := st.Q.StatementCharges(ctx, db.StatementChargesParams{UserID: userID, Hour: from, Hour_2: to})
	if err != nil {
		return err
	}
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"vm_id", "hostname", "plan", "hour_utc", "amount_uusdt", "amount_usdt", "status", "labels"}); err != nil {
		return err
	}
	for _, r := range rows {
		labels := labelsOf(r.Labels)
		keys := make([]string, 0, len(labels))
		for k := range labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"="+labels[k])
		}
		rec := []string{fmt.Sprint(r.VmID), safeCell(r.Hostname), safeCell(r.PlanSlug), r.Hour.UTC().Format("2006-01-02T15:04:05Z"),
			fmt.Sprint(r.AmountUusdt), fmt.Sprintf("%d.%06d", r.AmountUusdt/1_000_000, r.AmountUusdt%1_000_000), r.Status, safeCell(strings.Join(parts, ";"))}
		if err := cw.Write(rec); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

func safeCell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}
