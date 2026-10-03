//go:build integration

package receivables_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/platform/page"
	"github.com/iricardofernandes/jupiter/internal/reconciliation"
	banksim "github.com/iricardofernandes/jupiter/internal/sim/bank"
)

// reconcile reconciles test mode through a day.
func (h *harness) reconcileThrough(day string) {
	h.t.Helper()
	if _, err := h.recon.Reconcile(h.t.Context(), false, date(day)); err != nil {
		h.t.Fatal(err)
	}
}

// openBreaks are the open breaks, as "counterparty stream kind key", sorted.
func (h *harness) openBreaks() []string {
	h.t.Helper()
	found, _, err := h.recon.Breaks(h.t.Context(), h.pool, false, "", "open", page.Request{Limit: 100})
	if err != nil {
		h.t.Fatal(err)
	}
	out := make([]string, 0, len(found))
	for _, b := range found {
		out = append(out, strings.Join([]string{b.Counterparty, b.Stream, b.Kind, b.Key}, " "))
	}
	slices.Sort(out)
	return out
}

func (h *harness) wantBreaks(when string, want ...string) {
	h.t.Helper()
	slices.Sort(want)
	if got := h.openBreaks(); !slices.Equal(got, want) {
		h.t.Fatalf("%s, open breaks:\n%s\nwant:\n%s", when, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// issueBoleto creates and confirms a boleto due in two weeks.
func (h *harness) issueBoleto(amount int64) string {
	h.t.Helper()
	it := h.ok(http.MethodPost, "/v1/payment_intents", map[string]any{
		"amount": amount, "currency": "brl", "payment_method": "boleto", "confirm": true,
		"boleto": map[string]any{"due_date": "2026-10-15", "days_after_due": 5, "pix": false, "payer": map[string]any{"name": "Maria da Silva", "tax_id": "12345678909"}},
	})
	return str(it, "id")
}

// bankDay sends the bank the remittance, lets it close the day, and reads its returns.
func (h *harness) bankDay() {
	h.t.Helper()
	if _, err := h.transfers.Remit(h.t.Context(), h.payments); err != nil {
		h.t.Fatal(err)
	}
	h.bank.Tick()
	if _, err := h.transfers.ImportReturns(h.t.Context(), h.pool, h.payments); err != nil {
		h.t.Fatal(err)
	}
	h.clock.Advance(2 * time.Minute)
	if _, err := h.payments.ResolvePayouts(h.t.Context(), h.pool); err != nil {
		h.t.Fatal(err)
	}
}

// The bank loses a return record, writes a statement line twice and another a day late,
// and loses a transfer's line: reconciliation finds each, and nothing else, and the late
// line's break resolves the day it arrives.
func TestTheBankReconciles(t *testing.T) {
	h := newHarness(t)
	var boletos []string
	for _, amount := range []int64{1000, 2000, 3000, 4000} {
		boletos = append(boletos, h.issueBoleto(amount))
	}
	h.bankDay()
	byLine := map[string]string{}
	for _, b := range h.bank.Boletos(jupiterBank) {
		byLine[b.Line] = b.OurNumber
	}
	seller := h.bankSeller("123456", nil)
	h.payWithSplit(seller)
	h.register()
	h.anticipate(seller, h.sellerUnits(seller)[0])
	h.register()
	po := h.ok(http.MethodPost, "/v1/payouts", map[string]any{"amount": 5000, "currency": "brl", "recipient": seller})
	transfer := strings.ReplaceAll(str(po, "id"), "_", "")

	numbers := map[string]string{}
	for _, id := range boletos {
		it := h.ok(http.MethodGet, "/v1/payment_intents/"+id, nil)
		line := str(obj(obj(it, "next_action"), "boleto_display_details"), "line")
		numbers[id] = byLine[line]
	}
	lost, twice, late := numbers[boletos[1]], numbers[boletos[2]], numbers[boletos[3]]
	h.setBankFaults(func(e banksim.Event) banksim.Fault {
		return banksim.Fault{
			Drop:      (e.Kind == "return" && e.Reference == lost) || (e.Kind == "statement" && e.Reference == transfer),
			Duplicate: e.Kind == "statement" && e.Reference == twice,
			Delay:     e.Kind == "statement" && e.Reference == late,
		}
	})
	for _, id := range boletos {
		it := h.ok(http.MethodGet, "/v1/payment_intents/"+id, nil)
		if _, err := h.bank.Pay(str(obj(obj(it, "next_action"), "boleto_display_details"), "line")); err != nil {
			t.Fatal(err)
		}
	}
	h.bankDay() // Thursday 1 October: the boletos paid, the transfer made
	if p := h.payout(str(po, "id")); str(p, "status") != "paid" {
		t.Fatalf("the payout: %v", p)
	}
	h.reconcileThrough("2026-10-01")
	h.wantBreaks("on the 1st", "bank statement missing_at_counterparty "+transfer)

	h.clock.Advance(24 * time.Hour)
	h.bankDay() // Friday the 2nd: the boletos credited
	h.reconcileThrough("2026-10-02")
	h.wantBreaks("on the 2nd",
		"bank statement missing_at_counterparty "+transfer,
		"bank statement missing_at_jupiter "+lost,
		"bank statement duplicate "+twice,
		"bank statement missing_at_counterparty "+late,
	)

	h.clock.Advance(3 * 24 * time.Hour)
	h.bankDay() // Monday the 5th: the late line
	h.reconcileThrough("2026-10-05")
	h.wantBreaks("on the 5th",
		"bank statement missing_at_counterparty "+transfer,
		"bank statement missing_at_jupiter "+lost,
		"bank statement duplicate "+twice,
	)
	// The 2nd's report: its three credits Jupiter booked matched, the late one since; four
	// breaks open at its end, three of them opened that day.
	report, err := h.recon.Report(t.Context(), h.pool, false, date("2026-10-02"), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range report.Streams {
		if s.Counterparty == "bank" && s.Stream == "statement" && (s.Matched != 3 || s.Amount != 8000 || s.OpenedOnDay != 3 || s.Open != 4) {
			t.Fatalf("the statement on the 2nd: %+v", s)
		}
	}
	// An operator settles the duplicate with the bank: it stays resolved.
	found, _, err := h.recon.Breaks(t.Context(), h.pool, false, "", "open", page.Request{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range found {
		if b.Kind == reconciliation.Duplicate {
			if err := h.recon.Resolve(t.Context(), b.ID, "the bank reversed the second line", "ana"); err != nil {
				t.Fatal(err)
			}
		}
	}
	h.clock.Advance(24 * time.Hour)
	h.reconcileThrough("2026-10-06")
	h.wantBreaks("once resolved",
		"bank statement missing_at_counterparty "+transfer,
		"bank statement missing_at_jupiter "+lost,
	)
	h.consistent()
}

// A settled grade is reconciled with the SLC, and its credit with the bank's statement,
// where it arrives a day late.
func TestTheSLCReconciles(t *testing.T) {
	h := newHarness(t)
	h.pay(10000, 1)
	h.register()
	first := h.agenda(time.Time{})[0].SettlementDate
	day, err := time.Parse(time.DateOnly, first)
	if err != nil {
		t.Fatal(err)
	}
	h.setBankFaults(func(e banksim.Event) banksim.Fault {
		return banksim.Fault{Delay: e.Kind == "statement" && strings.HasPrefix(e.Reference, "SLC/")}
	})
	h.settleOn(day)
	h.reconcileThrough(first)
	h.wantBreaks("on the settlement day", "bank statement missing_at_counterparty SLC/"+first)
	next := day.AddDate(0, 0, 1)
	for next.Weekday() == time.Saturday || next.Weekday() == time.Sunday {
		next = next.AddDate(0, 0, 1)
	}
	h.reconcileThrough(next.Format(time.DateOnly))
	h.wantBreaks("the day after")
	report, err := h.recon.Report(t.Context(), h.pool, false, day, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Streams) == 0 {
		t.Fatalf("the report: %+v", report)
	}
}

func obj(m map[string]any, k string) map[string]any {
	o, _ := m[k].(map[string]any)
	return o
}

// A stream the counterparty cannot answer for opens no break and leaves the days to be
// read again; once it answers, it matches.
func TestAStreamThatCannotBeRead(t *testing.T) {
	h := newHarness(t)
	down := true
	stream := reconciliation.Stream{
		Counterparty: "bank", Name: "statement",
		Ours: []reconciliation.OursFunc{func(context.Context, *pgxpool.Pool, bool, time.Time) ([]reconciliation.Record, error) {
			return []reconciliation.Record{{Identity: "po_1", Key: "po1", Direction: reconciliation.Out, Amount: 500, Date: date("2026-10-01")}}, nil
		}},
		Theirs: func(_ context.Context, _ *pgxpool.Pool, _ bool, day time.Time) ([]reconciliation.Record, error) {
			if down {
				return nil, errors.New("the bank is down")
			}
			if !day.Equal(date("2026-10-01")) {
				return nil, nil
			}
			return []reconciliation.Record{
				{Identity: "L1", Key: "po1", Direction: reconciliation.Out, Amount: 500, Date: day},
				{Identity: "L2", Key: "", Direction: reconciliation.In, Amount: 1, Date: day},
			}, nil
		},
	}
	s := reconciliation.New(reconciliation.Config{Pool: h.pool, Now: h.clock.Now, Test: reconciliation.Mode{Streams: []reconciliation.Stream{stream}}})
	if _, err := s.Reconcile(t.Context(), false, date("2026-10-01")); err == nil {
		t.Fatal("a stream that cannot be read reported nothing")
	}
	if open, _, err := s.Breaks(t.Context(), h.pool, false, "", "open", page.Request{Limit: 10}); err != nil || len(open) != 0 {
		t.Fatalf("breaks of a stream not read: %+v, %v", open, err)
	}
	down = false
	run, err := s.Reconcile(t.Context(), false, date("2026-10-01"))
	if err != nil || run.Matched != 1 || run.Opened != 1 {
		t.Fatalf("read again: %+v, %v (a line without a key opens a break of its own, the rest matched)", run, err)
	}
}
