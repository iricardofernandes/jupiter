//go:build simulation

package simulation_test

import (
	"bytes"
	"fmt"
	"net/http"
	"time"

	"github.com/iricardofernandes/jupiter/internal/payments"
	pixsim "github.com/iricardofernandes/jupiter/internal/sim/pix"
)

// scenario is one customer's or merchant's story on a rail, chosen from the seed when it
// starts. Like the card scripts, it advances a request at a time; a request that dies, or
// answers 409 or 5xx, is sent again later with the same Idempotency-Key, and a finished
// one is sometimes sent again and must answer the same.
type scenario struct {
	id       int
	kind     string
	amount   int64
	intent   string
	pending  *railRequest
	sequence int
	done     bool
	// idle is waiting for the background or a payer, not for its own next request.
	idle      bool
	lastReply map[string][]byte

	installments int
	declined     bool
	refund       int64
	refunded     bool
	dispute      bool
	disputeID    string
	answer       string
	answered     bool
	pays         bool
	paid         bool
	claimed      bool
	upheld       bool
	qr, line     string
	e2e          string
	due          time.Time
}

type railRequest struct {
	method, path, key string
	body              any
}

const (
	kindCard   = "card"
	kindPix    = "pix"
	kindBoleto = "boleto"
	kindPayout = "payout"
	kindStray  = "stray"
)

func (r *rails) newScenario(n int) *scenario {
	g := r.rng
	sc := &scenario{id: n, lastReply: map[string][]byte{}}
	switch roll := g.IntN(100); {
	case roll < 35:
		sc.kind, sc.amount = kindCard, 1000+g.Int64N(200_000)
		sc.installments = 1 + g.IntN(6)
		sc.declined = g.IntN(10) == 0
		sc.dispute = g.IntN(8) == 0
	case roll < 65:
		sc.kind, sc.amount = kindPix, 100+g.Int64N(50_000)
		sc.pays = g.IntN(10) < 9
		sc.dispute = g.IntN(10) == 0
		sc.upheld = g.IntN(2) == 0
	case roll < 85:
		sc.kind, sc.amount = kindBoleto, 1000+g.Int64N(100_000)
		sc.pays = g.IntN(100) < 85
		sc.due = r.clock.Now().In(brasilia).AddDate(0, 0, 3+g.IntN(8))
	case roll < 93:
		sc.kind, sc.amount = kindPayout, 100+g.Int64N(5000)
	default:
		sc.kind, sc.amount = kindStray, 100+g.Int64N(20_000)
	}
	if sc.kind == kindCard || sc.kind == kindPix {
		if g.IntN(4) == 0 {
			sc.refund = 1 + g.Int64N(sc.amount)
		}
	}
	sc.answer = []string{"win", "lose", "accept", "none"}[g.IntN(4)]
	r.stats.kinds[sc.kind]++
	r.record("scenario %d: %s %d installments=%d declined=%t refund=%d dispute=%t answer=%s pays=%t", n, sc.kind, sc.amount, sc.installments, sc.declined, sc.refund, sc.dispute, sc.answer, sc.pays)
	return sc
}

func (r *rails) request(sc *scenario, path string, body any) *railRequest {
	sc.sequence++
	return &railRequest{method: http.MethodPost, path: path, body: body, key: fmt.Sprintf("rail-%d-%d", sc.id, sc.sequence)}
}

// step sends the scenario's next request, or sends its pending one again.
func (r *rails) step(sc *scenario) {
	if sc.pending == nil {
		sc.pending = r.next(sc)
		if sc.pending == nil {
			return
		}
	}
	p := sc.pending
	r.crash = r.faulty && r.rng.IntN(100) < 4
	resp := r.send(p.method, p.path, p.key, p.body)
	if resp.crashed || resp.status == http.StatusConflict || resp.status >= 500 {
		r.record("scenario %d: %s %s crashed=%t status=%d", sc.id, p.method, p.path[:min(len(p.path), 16)], resp.crashed, resp.status)
		return
	}
	r.record("scenario %d: %s %s → %d", sc.id, p.method, p.path[:min(len(p.path), 16)], resp.status)
	if previous, ok := sc.lastReply[p.key]; ok && !bytes.Equal(previous, resp.raw) {
		r.fatalf("scenario %d: %s answered differently to the same Idempotency-Key:\n%s\n%s", sc.id, p.key, previous, resp.raw)
	}
	sc.lastReply[p.key] = resp.raw
	if r.faulty && r.rng.IntN(100) < 5 {
		r.stats.duplicates++
		return
	}
	sc.pending = nil
	r.applyReply(sc, p, resp)
}

// applyReply takes what a scenario needs from an answer.
func (r *rails) applyReply(sc *scenario, p *railRequest, resp railResponse) {
	switch {
	case sc.kind == kindPayout:
		if resp.status != http.StatusOK && resp.status != http.StatusBadRequest {
			r.fatalf("scenario %d: the payout answered %d %s", sc.id, resp.status, resp.raw)
		}
		sc.done = true
	case p.path == "/v1/payment_intents" && sc.intent == "":
		it := resp.body
		if resp.status == http.StatusPaymentRequired {
			it = obj(obj(resp.body, "error"), "payment_intent")
		} else if resp.status != http.StatusOK {
			r.fatalf("scenario %d: creating the payment answered %d %s", sc.id, resp.status, resp.raw)
		}
		sc.intent = str(it, "id")
		sc.qr = str(obj(obj(it, "next_action"), "pix_display_qr_code"), "data")
		sc.line = str(obj(obj(it, "next_action"), "boleto_display_details"), "line")
		if sc.intent == "" {
			r.fatalf("scenario %d: no payment intent in %s", sc.id, resp.raw)
		}
	case p.path == "/v1/test_helpers/disputes":
		if resp.status != http.StatusOK {
			r.fatalf("scenario %d: opening a chargeback on %s (%s) answered %d %s", sc.id, sc.intent, r.get("/v1/payment_intents/" + sc.intent)["payment_method"], resp.status, resp.raw)
		}
		sc.disputeID = str(resp.body, "id")
	case p.path == "/v1/refunds" && resp.status != http.StatusOK && resp.status != http.StatusBadRequest:
		r.fatalf("scenario %d: refunding answered %d %s", sc.id, resp.status, resp.raw)
	}
}

// next decides the scenario's next request from where its payment stands; or does what a
// payer does, and returns no request.
func (r *rails) next(sc *scenario) *railRequest {
	sc.idle = false
	switch sc.kind {
	case kindPayout:
		return r.request(sc, "/v1/payouts", map[string]any{
			"amount": sc.amount, "currency": "brl", "destination": map[string]any{"type": "pix", "pix_key": railPayoutKey},
		})
	case kindStray:
		r.payPix(sc, pixsim.Payment{Key: railPixKey, Amount: reais(sc.amount)})
		sc.done = true
		return nil
	}
	if sc.intent == "" {
		return r.request(sc, "/v1/payment_intents", r.createBody(sc))
	}
	it := r.get("/v1/payment_intents/" + sc.intent)
	switch str(it, "status") {
	case "processing":
		sc.idle = true
		return nil
	case "requires_action":
		return r.awaitPayer(sc)
	case "succeeded":
		return r.afterSuccess(sc, it)
	default:
		sc.done = true
		return nil
	}
}

func (r *rails) createBody(sc *scenario) map[string]any {
	body := map[string]any{"amount": sc.amount, "currency": "brl", "confirm": true}
	switch sc.kind {
	case kindCard:
		body["payment_method"] = payments.TestCardVisa
		if sc.declined {
			body["payment_method"] = payments.TestCardDeclined
		}
		if sc.installments > 1 {
			body["installments"] = map[string]any{"count": sc.installments, "financed_by": "merchant"}
		}
	case kindPix:
		body["payment_method"] = "pix"
	case kindBoleto:
		body["payment_method"] = "boleto"
		body["boleto"] = map[string]any{
			"due_date": sc.due.Format(time.DateOnly), "days_after_due": 2,
			"payer": map[string]any{"name": "Maria da Silva", "tax_id": "12345678909"},
		}
	}
	return body
}

// awaitPayer is a customer with a charge in front of them: paying it, or not.
func (r *rails) awaitPayer(sc *scenario) *railRequest {
	sc.idle = true
	if !sc.pays || sc.paid {
		return nil
	}
	switch sc.kind {
	case kindPix:
		res := r.payPix(sc, pixsim.Payment{BRCode: sc.qr})
		sc.paid, sc.e2e = true, res.EndToEndID
	case kindBoleto:
		// The payer pays before the due day; the bank knows the boleto once Jupiter's
		// remittance has reached it.
		if !r.clock.Now().In(brasilia).Before(sc.due.AddDate(0, 0, -1)) {
			return nil
		}
		if _, err := r.bank.Pay(sc.line); err == nil {
			sc.paid = true
			r.record("scenario %d: boleto paid", sc.id)
		}
	}
	return nil
}

func (r *rails) payPix(sc *scenario, p pixsim.Payment) pixsim.PaymentResult {
	p.PayerName, p.PayerTaxID = "Maria da Silva", "12345678909"
	res, err := r.pix.Pay(r.t.Context(), p)
	r.pix.Close()
	if err != nil {
		r.fatalf("scenario %d: paying by Pix: %v", sc.id, err)
	}
	r.record("scenario %d: Pix paid, refused %q", sc.id, res.Refused)
	return res
}

// afterSuccess is what happens to a payment that succeeded: a refund, a chargeback or a
// MED claim, and the merchant's answer.
func (r *rails) afterSuccess(sc *scenario, it map[string]any) *railRequest {
	switch {
	case sc.refund > 0 && !sc.refunded:
		sc.refunded = true
		left := num(it, "amount_received") - num(it, "amount_refunded")
		return r.request(sc, "/v1/refunds", map[string]any{"payment_intent": sc.intent, "amount": min(sc.refund, left)})
	case !sc.dispute:
		sc.done = true
		return nil
	case sc.kind == kindCard && sc.disputeID == "":
		return r.request(sc, "/v1/test_helpers/disputes", map[string]any{"payment_intent": sc.intent, "reason_code": "13.1"})
	case sc.kind == kindPix && !sc.claimed:
		sc.claimed = true
		if _, err := r.pix.ReportInfraction(r.t.Context(), pixsim.InfractionParams{EndToEndID: sc.e2e, Details: "golpe", Upheld: sc.upheld}); err != nil {
			r.record("scenario %d: MED claim refused", sc.id)
			sc.done = true
		}
		r.pix.Close()
		sc.idle = true
		return nil
	}
	return r.answerDispute(sc)
}

// answerDispute is the merchant answering its dispute, as the scenario says: with
// evidence that wins or loses, by accepting, or not at all.
func (r *rails) answerDispute(sc *scenario) *railRequest {
	if sc.disputeID == "" {
		list := r.get("/v1/disputes?payment_intent=" + sc.intent)
		data, _ := list["data"].([]any)
		if len(data) == 0 {
			sc.idle = true
			return nil
		}
		d, _ := data[0].(map[string]any)
		sc.disputeID = str(d, "id")
	}
	d := r.get("/v1/disputes/" + sc.disputeID)
	switch status := str(d, "status"); {
	case status == "won" || status == "lost":
		sc.done = true
		return nil
	case status != "needs_response" || sc.answered || sc.answer == "none":
		sc.idle = true
		return nil
	}
	sc.answered = true
	switch sc.answer {
	case "accept":
		return r.request(sc, "/v1/disputes/"+sc.disputeID+"/close", nil)
	case "win":
		return r.request(sc, "/v1/disputes/"+sc.disputeID, map[string]any{"evidence": map[string]any{"uncategorized_text": "winning_evidence"}, "submit": true})
	default:
		return r.request(sc, "/v1/disputes/"+sc.disputeID, map[string]any{"evidence": map[string]any{"uncategorized_text": "losing_evidence"}, "submit": true})
	}
}

// final is how the scenario ended, for the run's summary.
func (sc *scenario) final(r *rails) string {
	if sc.intent == "" {
		return "done"
	}
	return str(r.get("/v1/payment_intents/"+sc.intent), "status")
}

func reais(centavos int64) string { return fmt.Sprintf("%d.%02d", centavos/100, centavos%100) }

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func num(m map[string]any, k string) int64 {
	f, _ := m[k].(float64)
	return int64(f)
}

func obj(m map[string]any, k string) map[string]any {
	o, _ := m[k].(map[string]any)
	return o
}
