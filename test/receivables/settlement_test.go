//go:build integration

package receivables_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/recipients"
	registrysim "github.com/iricardofernandes/jupiter/internal/sim/registry"
	"github.com/iricardofernandes/jupiter/pkg/registryapi"
)

// settleOn runs the day's settlement on day, has the SLC settle it, and posts it.
func (h *harness) settleOn(day time.Time) {
	h.t.Helper()
	h.clock.Advance(day.Add(15 * time.Hour).Sub(h.clock.Now()))
	if err := h.receivables.SettleDay(h.t.Context(), h.pool); err != nil {
		h.t.Fatal(err)
	}
	h.slc.Tick()
	if err := h.receivables.SettleDay(h.t.Context(), h.pool); err != nil {
		h.t.Fatal(err)
	}
}

func date(s string) time.Time {
	d, _ := time.Parse(time.DateOnly, s)
	return d
}

// A merchant's units settle through the SLC on their dates: the network pays each
// unit's gross, its financier's share straight to the financier, the rest and Jupiter's
// fee into Jupiter's settlement account; the merchant's share becomes available.
func TestSettlementThroughTheSLC(t *testing.T) {
	h := newHarness(t)
	h.pay(60000, 6)
	h.register()
	h.ok(http.MethodPost, "/v1/receivables/opt_ins", map[string]any{"financier": bankCNPJ})
	if _, err := h.receivables.SyncOptIns(t.Context(), h.pool); err != nil {
		t.Fatal(err)
	}
	if err := h.registry.Accept(bankCNPJ, registryapi.Contract{
		ID: "loan-1", Holder: merchantCNPJ, Effect: string(registrysim.Lien), Rule: string(registrysim.Fixed), Amount: 15000,
		Domicile: registryapi.Domicile{ISPB: "33000167", Account: "loans"},
	}); err != nil {
		t.Fatal(err)
	}
	h.reconcile("daily")

	// 10000 a month, 9651 net of the fee: the first all to the bank.
	h.settleOn(date("2026-11-03"))
	g, err := h.slc.GradeOf(slcToken, "2026-11-03")
	if err != nil || g.Status != "settled" || g.Total != 10000 || g.PaidOther != 9651 || g.Credited != 349 {
		t.Fatalf("the first grade: %+v, %v", g, err)
	}
	if b := h.balance("me"); num(b, "available") != 0 {
		t.Fatalf("after the first: %v", b)
	}
	// Again the same day: nothing more.
	if err := h.receivables.SettleDay(t.Context(), h.pool); err != nil {
		t.Fatal(err)
	}
	h.consistent()

	// The second: the rest of the loan to the bank, the remainder to the merchant.
	h.settleOn(date("2026-12-03"))
	g, err = h.slc.GradeOf(slcToken, "2026-12-03")
	if err != nil || g.Status != "settled" || g.Total != 10000 || g.PaidOther != 15000-9651 || g.Credited != 10000-(15000-9651) {
		t.Fatalf("the second grade: %+v, %v", g, err)
	}
	if b := h.balance("me"); num(b, "available") != 9651-(15000-9651) {
		t.Fatalf("after the second: %v", b)
	}
	if r := h.reconcile("weekly"); len(r.Divergences) != 0 {
		t.Fatalf("divergences: %+v", r.Divergences)
	}
	h.consistent()
}

// Anticipations are reported to the SLC by the business day after; on the units' date,
// what Jupiter bought is paid to it.
func TestAnticipationsAreReportedToTheSLC(t *testing.T) {
	h := newHarness(t)
	seller := h.seller(true)
	h.payWithSplit(seller)
	h.register()
	units := h.sellerUnits(seller)
	h.anticipate(seller, units[0])
	if n, err := h.receivables.ReportAnticipations(t.Context(), h.pool); err != nil || n != 1 {
		t.Fatalf("reporting: %d, %v", n, err)
	}
	h.anticipate(seller, units[1])
	h.clock.Advance(5 * 24 * time.Hour) // Tuesday: it was due Friday
	if n, err := h.receivables.ReportAnticipations(t.Context(), h.pool); err != nil || n != 1 {
		t.Fatalf("reporting late: %d, %v", n, err)
	}
	reports, late := h.slc.Reports(slcToken)
	if len(reports) != 2 || len(late) != 1 || late[0].Due != "2026-10-02" {
		t.Fatalf("reports %+v, late %+v", reports, late)
	}
	h.register()
	h.settleOn(date("2026-11-03"))
	g, err := h.slc.GradeOf(slcToken, "2026-11-03")
	if err != nil || g.Status != "settled" || g.Credited != g.Total || g.Total != 10000 {
		t.Fatalf("the grade: %+v, %v", g, err)
	}
	h.consistent()
}

func (h *harness) sellerUnits(seller string) []string {
	h.t.Helper()
	agenda := h.ok(http.MethodGet, "/v1/receivables/agenda?recipient="+seller+"&from=2026-10-01&to=2027-06-30", nil)
	data, _ := agenda["data"].([]any)
	out := make([]string, 0, len(data))
	for _, u := range data {
		out = append(out, str(u.(map[string]any), "id")) //nolint:forcetypeassert // the API answers objects
	}
	return out
}

func (h *harness) anticipate(seller string, units ...string) {
	h.t.Helper()
	quote := h.ok(http.MethodPost, "/v1/anticipations/simulate", map[string]any{"recipient": seller, "units": units})
	h.ok(http.MethodPost, "/v1/anticipations", map[string]any{"quote": str(quote, "id")})
}

// bankSeller signs up a seller paid out to a bank account, and verifies it.
func (h *harness) bankSeller(account string, settings map[string]any) string {
	h.t.Helper()
	body := map[string]any{
		"name": "Vendedora", "tax_id": sellerCNPJ,
		"payout_destination": map[string]any{"type": "bank_account", "ispb": "60701190", "branch": "0001", "account": account},
	}
	if settings != nil {
		body["transfer_settings"] = settings
	}
	rec := h.ok(http.MethodPost, "/v1/recipients", body)
	h.ok(http.MethodPost, "/v1/test_helpers/recipients/"+str(rec, "id")+"/verify", map[string]any{"status": "verified"})
	return str(rec, "id")
}

// resolve lets the bank close a day and Jupiter ask after its payouts.
func (h *harness) resolve() {
	h.t.Helper()
	h.bank.Tick()
	h.clock.Advance(2 * time.Minute)
	if _, err := h.payments.ResolvePayouts(h.t.Context(), h.pool); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) payout(id string) map[string]any {
	h.t.Helper()
	return h.ok(http.MethodGet, "/v1/payouts/"+id, nil)
}

// A recipient's payout by bank transfer: at least R$ 10.00; paid, then sent back by the
// receiving bank, its amount is the recipient's again.
func TestAPayoutByBankTransferReturned(t *testing.T) {
	h := newHarness(t)
	seller := h.bankSeller("888888", nil)
	h.payWithSplit(seller)
	h.register()
	h.anticipate(seller, h.sellerUnits(seller)[0])
	available := num(h.balance(seller), "available")
	if r := h.call(http.MethodPost, "/v1/payouts", map[string]any{"amount": 999, "currency": "brl", "recipient": seller}); r.status != http.StatusBadRequest {
		t.Fatalf("a payout below the minimum: %d %s", r.status, r.raw)
	}
	po := h.ok(http.MethodPost, "/v1/payouts", map[string]any{"amount": 5000, "currency": "brl", "recipient": seller})
	dest, _ := po["destination"].(map[string]any)
	if str(po, "status") != "pending" || str(dest, "type") != "bank_account" || str(dest, "account") != "888888" {
		t.Fatalf("the payout: %v", po)
	}
	h.resolve()
	if p := h.payout(str(po, "id")); str(p, "status") != "paid" || num(h.balance(seller), "available") != available-5000 {
		t.Fatalf("paid: %v", p)
	}
	h.resolve()
	p := h.payout(str(po, "id"))
	if str(p, "status") != "returned" || str(p, "failure_code") != "transfer_returned" || p["returned_at"] == nil {
		t.Fatalf("returned: %v", p)
	}
	if b := h.balance(seller); num(b, "available") != available {
		t.Fatalf("the balance after the return: %v", b)
	}
	h.consistent()
}

// A recipient whose payouts an operator holds: its payout waits, held, until released.
func TestAHeldPayout(t *testing.T) {
	h := newHarness(t)
	seller := h.bankSeller("123456", nil)
	h.payWithSplit(seller)
	h.register()
	h.anticipate(seller, h.sellerUnits(seller)[0])
	h.hold(seller, true)
	po := h.ok(http.MethodPost, "/v1/payouts", map[string]any{"amount": 2000, "currency": "brl", "recipient": seller})
	h.resolve()
	if p := h.payout(str(po, "id")); str(p, "status") != "held" {
		t.Fatalf("held: %v", p)
	}
	h.consistent()
	h.hold(seller, false)
	h.resolve()
	h.resolve()
	if p := h.payout(str(po, "id")); str(p, "status") != "paid" {
		t.Fatalf("released: %v", p)
	}
	h.consistent()
}

func (h *harness) hold(recipient string, held bool) {
	h.t.Helper()
	err := postgres.InTx(h.t.Context(), h.pool, func(tx pgx.Tx) error {
		if err := h.recipients.HoldPayouts(h.t.Context(), tx, recipient, held); err != nil || held {
			return err
		}
		rec, err := h.recipients.ByID(h.t.Context(), tx, recipient)
		if err != nil {
			return err
		}
		owner := payments.Owner{Merchant: rec.Owner.Merchant, Livemode: rec.Owner.Livemode}
		_, _, err = h.payments.ReleaseHeldPayouts(h.t.Context(), tx, owner, recipient, rec.Default, rec.PayoutDestination())
		return err
	})
	if err != nil {
		h.t.Fatal(err)
	}
}

// A recipient's transfer settings pay its whole available balance out on their days,
// once a day.
func TestScheduledPayouts(t *testing.T) {
	h := newHarness(t)
	seller := h.bankSeller("123456", map[string]any{"interval": "daily"})
	h.payWithSplit(seller)
	h.register()
	h.anticipate(seller, h.sellerUnits(seller)[0])
	available := num(h.balance(seller), "available")
	if n, err := h.receivables.SchedulePayouts(t.Context(), h.pool); err != nil || n != 1 {
		t.Fatalf("scheduling: %d, %v", n, err)
	}
	if n, err := h.receivables.SchedulePayouts(t.Context(), h.pool); err != nil || n != 0 {
		t.Fatalf("scheduling again the same day: %d, %v", n, err)
	}
	list := h.ok(http.MethodGet, "/v1/payouts", nil)
	data, _ := list["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("payouts: %v", list)
	}
	po, _ := data[0].(map[string]any)
	if num(po, "amount") != available || str(po, "scheduled_on") != "2026-10-01" || str(po, "recipient") != seller {
		t.Fatalf("the scheduled payout: %v", po)
	}
	h.resolve()
	h.resolve()
	if p := h.payout(str(po, "id")); str(p, "status") != "paid" || num(h.balance(seller), "available") != 0 {
		t.Fatalf("paid: %v", p)
	}
	h.consistent()
}

// A transfer made and sent back before Jupiter heard it was made: paid, then returned.
func TestAPayoutReturnedBeforeItsAnswer(t *testing.T) {
	h := newHarness(t)
	seller := h.bankSeller("888888", nil)
	h.payWithSplit(seller)
	h.register()
	h.anticipate(seller, h.sellerUnits(seller)[0])
	available := num(h.balance(seller), "available")
	po := h.ok(http.MethodPost, "/v1/payouts", map[string]any{"amount": 5000, "currency": "brl", "recipient": seller})
	h.bank.Tick()
	h.resolve() // the bank made it, then sent it back, before Jupiter asked
	p := h.payout(str(po, "id"))
	if str(p, "status") != "returned" || num(h.balance(seller), "available") != available {
		t.Fatalf("returned before its answer: %v, %v", p, h.balance(seller))
	}
	h.consistent()
}

// In live mode, every destination of the merchant's own recipient, the first included,
// holds its payouts until an operator lets them go: a stolen key could have set it. Test
// mode, whose money is not real, holds nothing.
func TestTheMerchantsNewDestinationIsHeld(t *testing.T) {
	h := newHarness(t)
	for _, live := range []bool{false, true} {
		owner := recipients.Owner{Merchant: h.owner.Merchant, Livemode: live}
		for _, key := range []string{"loja@example.com", "outra@example.com"} {
			err := postgres.InTx(t.Context(), h.pool, func(tx pgx.Tx) error {
				me, err := h.recipients.Default(t.Context(), tx, owner)
				if err != nil {
					return err
				}
				_, err = h.recipients.Update(t.Context(), tx, owner, me.ID, recipients.Params{Destination: &recipients.Destination{Method: "pix", PixKey: key}})
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if me, err := h.recipients.DefaultOf(t.Context(), h.pool, owner); err != nil || me.PayoutsHeld != live {
				t.Fatalf("live=%t, after the destination %s: %+v, %v", live, key, me, err)
			}
		}
	}
}
