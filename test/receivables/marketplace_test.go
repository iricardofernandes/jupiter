//go:build integration

package receivables_test

import (
	"net/http"
	"testing"
	"time"

	registrysim "github.com/iricardofernandes/jupiter/internal/sim/registry"
	"github.com/iricardofernandes/jupiter/pkg/registryapi"
)

const sellerCNPJ = "11222333000262"

func (h *harness) ok(method, path string, body any) map[string]any {
	h.t.Helper()
	r := h.call(method, path, body)
	if r.status != http.StatusOK {
		h.t.Fatalf("%s %s: %d %s", method, path, r.status, r.raw)
	}
	return r.body
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func num(m map[string]any, k string) int64 {
	f, _ := m[k].(float64)
	return int64(f)
}

// seller signs up a seller as a recipient, with a Pix key, and verifies it.
func (h *harness) seller(verify bool) string {
	h.t.Helper()
	rec := h.ok(http.MethodPost, "/v1/recipients", map[string]any{
		"name": "Vendedora", "tax_id": sellerCNPJ, "payout_destination": map[string]any{"type": "pix", "pix_key": "vendedora@example.com"},
	})
	if str(rec, "status") != "pending" {
		h.t.Fatalf("a new recipient: %v", rec)
	}
	if verify {
		h.ok(http.MethodPost, "/v1/test_helpers/recipients/"+str(rec, "id")+"/verify", map[string]any{"status": "verified"})
	}
	return str(rec, "id")
}

// payWithSplit captures R$ 600.00 in six installments: 90% to the seller, liable for
// chargebacks; 10% to the marketplace, which pays the fee and takes the remainder.
func (h *harness) payWithSplit(seller string) string {
	h.t.Helper()
	it := h.ok(http.MethodPost, "/v1/payment_intents", map[string]any{
		"amount": 60000, "currency": "brl", "payment_method": "pm_card_visa", "confirm": true,
		"installments": map[string]any{"count": 6, "financed_by": "merchant"},
		"split": []map[string]any{
			{"recipient": seller, "percentage": "90.00", "liable": true},
			{"recipient": "me", "percentage": "10.00", "remainder": true, "charge_fee": true},
		},
	})
	if str(it, "status") != "succeeded" {
		h.t.Fatalf("paying: %v", it)
	}
	return str(it, "id")
}

func (h *harness) balance(recipient string) map[string]any {
	h.t.Helper()
	return h.ok(http.MethodGet, "/v1/recipients/"+recipient+"/balance", nil)
}

// A split payment: the seller's 90% and the marketplace's 10% less the whole fee become
// each one's pending balance and units, by settlement date; the merchant's own balance
// keeps nothing of it.
func TestASplitPayment(t *testing.T) {
	h := newHarness(t)
	seller := h.seller(true)
	h.payWithSplit(seller)
	if b := h.balance(seller); num(b, "pending") != 54000 || num(b, "available") != 0 || len(b["pending_on"].([]any)) != 6 { //nolint:forcetypeassert // the API answers an array
		t.Fatalf("the seller's balance: %v", b)
	}
	if b := h.balance("me"); num(b, "pending") != 6000-2094 {
		t.Fatalf("the marketplace's balance: %v", b)
	}
	sellerAgenda := h.ok(http.MethodGet, "/v1/receivables/agenda?recipient="+seller+"&from=2026-10-01&to=2027-06-30", nil)
	units, _ := sellerAgenda["data"].([]any)
	if len(units) != 6 || num(units[0].(map[string]any), "amount") != 9000 { //nolint:forcetypeassert // the API answers objects
		t.Fatalf("the seller's agenda: %v", sellerAgenda)
	}
	h.register()
	if r := h.reconcile("daily"); len(r.Divergences) != 0 {
		t.Fatalf("divergences: %+v", r.Divergences)
	}
	positions, err := h.registry.Positions(registrysim.Participant{TaxID: jupiterCNPJ, Role: registrysim.Accreditor}, sellerCNPJ, "", "", nil)
	if err != nil || len(positions) != 6 || positions[0].Value != 9000 {
		t.Fatalf("the seller's units at the registry: %+v, %v", positions, err)
	}
	h.consistent()
}

func TestSplitsThatAreRefused(t *testing.T) {
	h := newHarness(t)
	seller := h.seller(false)
	for name, split := range map[string][]map[string]any{
		"over 100%":       {{"recipient": seller, "percentage": "91.00", "liable": true}, {"recipient": "me", "percentage": "10.00", "remainder": true, "charge_fee": true}},
		"no liable":       {{"recipient": seller, "percentage": "90.00"}, {"recipient": "me", "percentage": "10.00", "remainder": true, "charge_fee": true}},
		"someone else's":  {{"recipient": "rp_01m3vtadkqfes9yh559x28xr1j", "percentage": "90.00", "liable": true}, {"recipient": "me", "percentage": "10.00", "remainder": true, "charge_fee": true}},
		"fee payers tiny": {{"recipient": seller, "percentage": "95.00", "liable": true}, {"recipient": "me", "percentage": "5.00", "remainder": true, "charge_fee": true}},
		"not verified":    {{"recipient": seller, "percentage": "90.00", "liable": true}, {"recipient": "me", "percentage": "10.00", "remainder": true, "charge_fee": true}},
	} {
		r := h.call(http.MethodPost, "/v1/payment_intents", map[string]any{"amount": 10000, "currency": "brl", "payment_method": "pm_card_visa", "split": split})
		if r.status != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, r.status, r.raw)
		}
	}
	rejected := h.ok(http.MethodPost, "/v1/recipients", map[string]any{"name": "Recusada", "tax_id": "12345678909"})
	h.ok(http.MethodPost, "/v1/test_helpers/recipients/"+str(rejected, "id")+"/verify", map[string]any{"status": "rejected"})
	if r := h.call(http.MethodPost, "/v1/payment_intents", map[string]any{"amount": 10000, "currency": "brl", "payment_method": "pix", "split": []map[string]any{
		{"recipient": "me", "percentage": "100.00", "liable": true, "remainder": true, "charge_fee": true},
	}}); r.status != http.StatusBadRequest {
		t.Errorf("a Pix payment split: %d %s", r.status, r.raw)
	}
	if r := h.call(http.MethodPost, "/v1/payment_intents", map[string]any{"amount": 10000, "currency": "brl", "payment_method": "pm_card_visa", "split": []map[string]any{
		{"recipient": str(rejected, "id"), "percentage": "90.00", "liable": true}, {"recipient": "me", "percentage": "10.00", "remainder": true, "charge_fee": true},
	}}); r.status != http.StatusBadRequest {
		t.Errorf("a rejected recipient: %d %s", r.status, r.raw)
	}
}

// Anticipating two of the seller's units: simulated, then created. The price is the
// units' present value at 1.99% a month; the seller has it available, the registry
// commits the units to Jupiter, and the ledger, the balances and the registry agree.
func TestAnticipatingTwoUnits(t *testing.T) {
	h := newHarness(t)
	seller := h.seller(true)
	h.payWithSplit(seller)
	h.register()
	agenda := h.ok(http.MethodGet, "/v1/receivables/agenda?recipient="+seller+"&from=2026-10-01&to=2027-06-30", nil)
	units, _ := agenda["data"].([]any)
	first, second := str(units[0].(map[string]any), "id"), str(units[1].(map[string]any), "id") //nolint:forcetypeassert // the API answers objects
	quote := h.ok(http.MethodPost, "/v1/anticipations/simulate", map[string]any{"recipient": seller, "units": []string{first, second}})
	// 9000 due in 33 days and 9000 in 60: 9000 / (1 + 0.0199 × 33/30) and 9000 / (1 + 0.0199 × 2).
	if num(quote, "amount") != 18000 || num(quote, "price") != 8807+8655 {
		t.Fatalf("the quote: %v", quote)
	}
	ant := h.ok(http.MethodPost, "/v1/anticipations", map[string]any{"quote": str(quote, "id")})
	if num(ant, "price") != num(quote, "price") || num(ant, "fee") != 18000-num(quote, "price") {
		t.Fatalf("the anticipation: %v", ant)
	}
	if again := h.ok(http.MethodPost, "/v1/anticipations", map[string]any{"quote": str(quote, "id")}); str(again, "id") != str(ant, "id") {
		t.Fatalf("the same quote again made %v", again)
	}
	if b := h.balance(seller); num(b, "available") != num(quote, "price") || num(b, "pending") != 54000-18000 {
		t.Fatalf("the seller's balance: %v", b)
	}
	if r := h.call(http.MethodPost, "/v1/anticipations/simulate", map[string]any{"recipient": seller, "units": []string{first}}); r.status != http.StatusBadRequest {
		t.Fatalf("anticipating a unit bought already: %d %s", r.status, r.raw)
	}
	h.register()
	if r := h.reconcile("daily"); len(r.Divergences) != 0 {
		t.Fatalf("divergences: %+v", r.Divergences)
	}
	positions, _ := h.registry.Positions(registrysim.Participant{TaxID: jupiterCNPJ, Role: registrysim.Accreditor}, sellerCNPJ, "", "", nil)
	if len(positions[0].Committed) != 1 || positions[0].Committed[0].Beneficiary != jupiterCNPJ || positions[0].Committed[0].Amount != 9000 ||
		positions[0].Committed[0].Effect != "ownership_transfer" || positions[2].Free != 9000 {
		t.Fatalf("the registry: %+v", positions[:3])
	}
	h.consistent()
}

// A refund after the seller anticipated: what the units still have pending goes first;
// what Jupiter bought, the seller pays back from its available balance.
func TestARefundAfterAnAnticipation(t *testing.T) {
	h := newHarness(t)
	seller := h.seller(true)
	intent := h.payWithSplit(seller)
	h.register()
	quote := h.ok(http.MethodPost, "/v1/anticipations/simulate", map[string]any{"recipient": seller})
	h.ok(http.MethodPost, "/v1/anticipations", map[string]any{"quote": str(quote, "id")})
	before := h.balance(seller)
	h.ok(http.MethodPost, "/v1/refunds", map[string]any{"payment_intent": intent})
	after := h.balance(seller)
	if num(after, "pending") != 0 || num(after, "available") != num(before, "available")-54000 {
		t.Fatalf("the seller's balance went from %v to %v", before, after)
	}
	if b := h.balance("me"); num(b, "pending") != 0 {
		t.Fatalf("the marketplace's balance: %v", b)
	}
	h.consistent()
}

// A seller that anticipates on its own has its units bought once they are a day old.
func TestAutomaticAnticipation(t *testing.T) {
	h := newHarness(t)
	seller := h.seller(true)
	h.ok(http.MethodPost, "/v1/recipients/"+seller, map[string]any{"automatic_anticipation": map[string]any{"enabled": true, "delay_days": 1}})
	h.payWithSplit(seller)
	h.register()
	if n, err := h.receivables.AnticipateAutomatically(t.Context(), h.pool); err != nil || n != 0 {
		t.Fatalf("on the day of the sale: %d, %v", n, err)
	}
	h.clock.Advance(25 * time.Hour)
	h.register()
	if n, err := h.receivables.AnticipateAutomatically(t.Context(), h.pool); err != nil || n != 1 {
		t.Fatalf("a day later: %d, %v", n, err)
	}
	if b := h.balance(seller); num(b, "pending") != 0 || num(b, "available") <= 50000 {
		t.Fatalf("the seller's balance: %v", b)
	}
	h.register()
	if r := h.reconcile("daily"); len(r.Divergences) != 0 {
		t.Fatalf("divergences: %+v", r.Divergences)
	}
	h.consistent()
}

// Changing where a verified seller is paid sends it back to verification: until then it
// is not paid out, nor split to; and a seller's money goes only to its own key.
func TestARecipientsDestinationIsVerified(t *testing.T) {
	h := newHarness(t)
	seller := h.seller(true)
	if r := h.ok(http.MethodPost, "/v1/recipients/"+seller, map[string]any{"payout_destination": map[string]any{"type": "pix", "pix_key": "outra@example.com"}}); str(r, "status") != "pending" {
		t.Fatalf("after a new destination: %v", r)
	}
	if r := h.call(http.MethodPost, "/v1/payouts", map[string]any{"amount": 100, "currency": "brl", "recipient": seller}); r.status != http.StatusBadRequest {
		t.Fatalf("a payout to a recipient pending verification: %d %s", r.status, r.raw)
	}
	h.ok(http.MethodPost, "/v1/test_helpers/recipients/"+seller+"/verify", map[string]any{"status": "verified"})
	if r := h.ok(http.MethodPost, "/v1/recipients/"+seller, map[string]any{"transfer_settings": map[string]any{"interval": "daily"}}); str(r, "status") != "verified" {
		t.Fatalf("new settings alone: %v", r)
	}
	if r := h.call(http.MethodPost, "/v1/payouts", map[string]any{
		"amount": 100, "currency": "brl", "recipient": seller, "destination": map[string]any{"type": "pix", "pix_key": "alguem@example.com"},
	}); r.status != http.StatusBadRequest {
		t.Fatalf("a recipient's payout to another key: %d %s", r.status, r.raw)
	}
}

func units(m map[string]any) []map[string]any {
	raw, _ := m["units"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, u := range raw {
		item, _ := u.(map[string]any)
		out = append(out, item)
	}
	return out
}

// A quote whose purchase reached the registry and was then undone, as when an answer is
// lost, can be carried out again: its own contracts count as free, and are placed again.
func TestAQuoteCarriedOutAgainAfterALostAnswer(t *testing.T) {
	h := newHarness(t)
	seller := h.seller(true)
	h.payWithSplit(seller)
	h.register()
	quote := h.ok(http.MethodPost, "/v1/anticipations/simulate", map[string]any{"recipient": seller})
	first := units(quote)[0]
	if err := h.registry.Accept(jupiterCNPJ, registryapi.Contract{
		ID: str(quote, "id") + "/" + str(first, "unit"), Holder: sellerCNPJ, Effect: "ownership_transfer", Rule: "fixed",
		Amount: num(first, "amount"), Arrangements: []string{"VCC"}, Accreditors: []string{jupiterCNPJ},
		From: str(first, "settlement_date"), To: str(first, "settlement_date"),
	}); err != nil {
		t.Fatal(err)
	}
	h.ok(http.MethodPost, "/v1/anticipations", map[string]any{"quote": str(quote, "id")})
	h.register()
	if r := h.reconcile("daily"); len(r.Divergences) != 0 {
		t.Fatalf("divergences: %+v", r.Divergences)
	}
	h.consistent()
}

// The registry committing to Jupiter what Jupiter does not hold, a contract left behind
// or one a refund made too large, is mended by the daily reconciliation.
func TestJupitersContractsAreMended(t *testing.T) {
	h := newHarness(t)
	seller := h.seller(true)
	intent := h.payWithSplit(seller)
	h.register()
	quote := h.ok(http.MethodPost, "/v1/anticipations/simulate", map[string]any{"recipient": seller})
	h.ok(http.MethodPost, "/v1/anticipations", map[string]any{"quote": str(quote, "id")})
	last := units(quote)[5]
	if err := h.registry.Accept(jupiterCNPJ, registryapi.Contract{
		ID: "left-behind", Holder: sellerCNPJ, Effect: "ownership_transfer", Rule: "fixed", Amount: 500,
		Arrangements: []string{"VCC"}, From: str(last, "settlement_date"), To: str(last, "settlement_date"),
	}); err != nil {
		t.Fatal(err)
	}
	// A partial refund takes from what Jupiter bought; a new sale then grows the units, which
	// Jupiter's fixed contracts would take again.
	h.ok(http.MethodPost, "/v1/refunds", map[string]any{"payment_intent": intent, "amount": 30000})
	h.payWithSplit(seller)
	h.register()
	if r := h.reconcile("daily"); len(r.Divergences) == 0 {
		t.Fatal("nothing to mend")
	}
	h.register()
	if r := h.reconcile("daily"); len(r.Divergences) != 0 {
		t.Fatalf("after mending: %+v", r.Divergences)
	}
	h.consistent()
}
