//go:build integration

package receivables_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/iricardofernandes/jupiter/internal/receivables"
	registrysim "github.com/iricardofernandes/jupiter/internal/sim/registry"
	"github.com/iricardofernandes/jupiter/pkg/registryapi"
)

func dates(entries []receivables.Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.SettlementDate)
	}
	return out
}

// R$ 600.00 in six installments financed by the merchant, captured on Thursday 1
// October 2026: six units of R$ 96.51 (R$ 100.00 less 3.49%), every 30 days from
// D+30, each on a business day. D+30 is a Saturday and the Monday after it is Finados.
func TestSixInstallmentsBecomeSixUnits(t *testing.T) {
	h := newHarness(t)
	h.pay(60000, 6)
	entries := h.agenda(time.Time{})
	want := []string{"2026-11-03", "2026-11-30", "2026-12-30", "2027-01-29", "2027-03-01", "2027-03-30"}
	if got := dates(entries); len(got) != 6 || got[0] != want[0] || got[4] != want[4] || got[5] != want[5] {
		t.Fatalf("settlement dates %v, want %v", got, want)
	}
	for _, e := range entries {
		if e.Value != 9651 || e.Free != 9651 || e.Arrangement != registryapi.ArrangementVisaCredit || e.Registered {
			t.Fatalf("unit %+v", e)
		}
	}
	h.register()
	positions, err := h.registry.Positions(registrysim.Participant{TaxID: jupiterCNPJ, Role: registrysim.Accreditor}, merchantCNPJ, "", "", nil)
	if err != nil || len(positions) != 6 || positions[0].Value != 9651 || !strings.HasPrefix(positions[0].Domicile.Account, "rp_") {
		t.Fatalf("the registry has %+v, %v", positions, err)
	}
	if r := h.reconcile(receivables.Daily); len(r.Divergences) != 0 {
		t.Fatalf("divergences: %+v", r.Divergences)
	}
	r := h.call(http.MethodGet, "/v1/receivables/agenda?from=2026-10-01&to=2027-06-30", nil)
	data, _ := r.body["data"].([]any)
	if r.status != http.StatusOK || len(data) != 6 {
		t.Fatalf("GET agenda: %d %s", r.status, r.raw)
	}
	if first, _ := data[0].(map[string]any); first["amount"] != float64(9651) || first["registered"] != true || first["settlement_date"] != "2026-11-03" {
		t.Fatalf("the first unit: %v", first)
	}
	if r := h.call(http.MethodGet, "/v1/receivables/agenda?from=2026-10-01&to=2026-12-31&as_of=1759000000", nil); r.status != http.StatusOK || r.body["data"] == nil || len(r.body["data"].([]any)) != 0 { //nolint:forcetypeassert // checked for nil, and the API answers an array
		t.Fatalf("the agenda before the payment: %d %s", r.status, r.raw)
	}
	if r := h.call(http.MethodGet, "/v1/receivables/agenda?from=2027-01-01&to=2026-01-01", nil); r.status != http.StatusBadRequest {
		t.Fatalf("a range backwards: %d %s", r.status, r.raw)
	}
	// A second payment on the same day lands on the same units.
	h.pay(10000, 1)
	h.register()
	if entries = h.agenda(time.Time{}); len(entries) != 6 || entries[0].Value != 9651+9701 {
		t.Fatalf("after a payment in one go: %+v", entries[0])
	}
	h.consistent()
}

// A refund takes the payment's net share out of its units, in proportion; the registry
// is told, and the fee comes back to the merchant.
func TestARefundReducesTheUnits(t *testing.T) {
	h := newHarness(t)
	intent := h.pay(60000, 6)
	h.register()
	if r := h.call(http.MethodPost, "/v1/refunds", map[string]any{"payment_intent": intent, "amount": 30000}); r.status != http.StatusOK {
		t.Fatalf("refunding: %d %s", r.status, r.raw)
	}
	// R$ 300.00 back, with half the fee (R$ 10.47): the units lose R$ 289.53, about a
	// sixth each.
	var total int64
	for _, e := range h.agenda(time.Time{}) {
		total += e.Value
	}
	if total != 6*9651-28953 {
		t.Fatalf("the units hold %d", total)
	}
	h.register()
	if r := h.reconcile(receivables.Daily); len(r.Divergences) != 0 {
		t.Fatalf("divergences: %+v", r.Divergences)
	}
	h.consistent()
}

// A financier can see the agenda only once the merchant opts in; its contract takes the
// earliest units first, and the agenda as of before the contract shows them free.
func TestAContractOnTheAgenda(t *testing.T) {
	h := newHarness(t)
	h.pay(60000, 6)
	h.register()
	bank := registrysim.Participant{TaxID: bankCNPJ, Role: registrysim.Financier}
	if _, err := h.registry.Positions(bank, merchantCNPJ, "", "", nil); !errors.Is(err, registrysim.ErrForbidden) {
		t.Fatalf("a financier without the merchant's opt-in: %v", err)
	}
	r := h.call(http.MethodPost, "/v1/receivables/opt_ins", map[string]any{"financier": bankCNPJ})
	if r.status != http.StatusOK {
		t.Fatalf("opting in: %d %s", r.status, r.raw)
	}
	if _, err := h.receivables.SyncOptIns(t.Context(), h.pool); err != nil {
		t.Fatal(err)
	}
	if positions, err := h.registry.Positions(bank, merchantCNPJ, "", "", nil); err != nil || len(positions) != 6 {
		t.Fatalf("the financier once opted in sees %d units, %v", len(positions), err)
	}
	before := h.clock.Now()
	h.clock.Advance(time.Hour)
	if err := h.registry.Accept(bankCNPJ, registryapi.Contract{
		ID: "loan-1", Holder: merchantCNPJ, Effect: string(registrysim.Lien), Rule: string(registrysim.Fixed), Amount: 15000,
		Domicile: registryapi.Domicile{ISPB: "33000167", Account: "loans"},
	}); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(time.Hour)
	h.reconcile(receivables.Daily)
	now := h.agenda(time.Time{})
	if now[0].Free != 0 || now[1].Free != 9651-(15000-9651) || len(now[0].Committed) != 1 || now[0].Committed[0].Beneficiary != bankCNPJ {
		t.Fatalf("after the contract: %+v / %+v", now[0], now[1])
	}
	if then := h.agenda(before); then[0].Free != 9651 || len(then[0].Committed) != 0 {
		t.Fatalf("as of before the contract: %+v", then[0])
	}
	if r := h.reconcile(receivables.Fortnightly); len(r.Divergences) != 0 {
		t.Fatalf("fortnightly: %+v", r.Divergences)
	}
	// On its date, settling the first unit pays the financier, first, through the
	// registry's split.
	h.clock.Advance(33 * 24 * time.Hour)
	paid, err := h.receivables.Settle(t.Context(), h.pool, now[0].Unit, time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC))
	if err != nil || len(paid) != 1 || paid[0].To != bankCNPJ || paid[0].Amount != 9651 {
		t.Fatalf("settling: %+v, %v", paid, err)
	}
	if again, err := h.receivables.Settle(t.Context(), h.pool, now[0].Unit, time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)); err != nil || len(again) != 1 {
		t.Fatalf("settling again: %+v, %v", again, err)
	}
	if r := h.reconcile(receivables.Weekly); len(r.Divergences) != 0 {
		t.Fatalf("weekly: %+v", r.Divergences)
	}
	h.consistent()
}

// A unit the registry holds differently is found by the daily reconciliation, sent
// again, and the divergence closes on the next one.
func TestADivergenceIsFixed(t *testing.T) {
	h := newHarness(t)
	h.pay(10000, 1)
	h.register()
	entry := h.agenda(time.Time{})[0]
	h.registry.SetUnits(jupiterCNPJ, []registryapi.Unit{{
		Holder: merchantCNPJ, Arrangement: entry.Arrangement, SettlementDate: entry.SettlementDate, Value: 1,
		ConstitutedOn: "2026-10-01",
	}})
	if r := h.reconcile(receivables.Daily); len(r.Divergences) != 1 {
		t.Fatalf("divergences: %+v", r.Divergences)
	}
	h.register()
	if r := h.reconcile(receivables.Daily); len(r.Divergences) != 0 {
		t.Fatalf("after sending it again: %+v", r.Divergences)
	}
	h.consistent()
}

// A unit not registered by the business day after its sale is a violation, and the
// registry sees the update arrive late.
func TestALateRegistration(t *testing.T) {
	h := newHarness(t)
	h.pay(10000, 1)
	h.clock.Advance(4 * 24 * time.Hour) // Monday 5 October, past Friday's deadline
	found, err := h.receivables.Check(t.Context(), h.pool)
	if err != nil || len(found) != 1 {
		t.Fatalf("check: %+v, %v", found, err)
	}
	h.register()
	if late := h.registry.Late(jupiterCNPJ); len(late) != 1 || late[0].Kind != "update" || late[0].Due != "2026-10-02" {
		t.Fatalf("the registry saw %+v", late)
	}
	h.consistent()
}

// A merchant without a CPF or CNPJ cannot opt in, and its units, which cannot be
// registered, hold up no other merchant's.
func TestAMerchantWithoutATaxIDHoldsUpNoOne(t *testing.T) {
	h := newHarness(t)
	ours := h.key
	h.key = h.another("")
	for range 3 {
		h.pay(10000, 6)
	}
	if r := h.call(http.MethodPost, "/v1/receivables/opt_ins", map[string]any{"financier": bankCNPJ}); r.status != http.StatusBadRequest {
		t.Fatalf("opting in without a tax id: %d %s", r.status, r.raw)
	}
	h.register()
	h.key = ours
	h.pay(10000, 1)
	h.register()
	if entries := h.agenda(time.Time{}); len(entries) != 1 || !entries[0].Registered {
		t.Fatalf("the merchant with a CNPJ: %+v", entries)
	}
	if r := h.call(http.MethodPost, "/v1/receivables/opt_ins", map[string]any{"financier": "1234"}); r.status != http.StatusBadRequest {
		t.Fatalf("a financier that is not a CNPJ: %d %s", r.status, r.raw)
	}
	if r := h.call(http.MethodGet, "/v1/receivables/agenda?from=2026-10-01&to=2026-12-31&as_of=99999999999", nil); r.status != http.StatusBadRequest {
		t.Fatalf("an agenda as of the far future: %d %s", r.status, r.raw)
	}
}
