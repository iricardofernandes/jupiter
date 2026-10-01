package pix_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/iricardofernandes/jupiter/internal/sim/pix"
	"github.com/iricardofernandes/jupiter/pkg/pixapi"
)

func (e *env) claim(p pix.InfractionParams) pixapi.InfractionReport {
	e.t.Helper()
	if _, err := e.sim.ReportInfraction(e.t.Context(), p); err != nil {
		e.t.Fatal(err)
	}
	select {
	case n := <-e.infractions:
		return n
	case <-time.After(5 * time.Second):
		e.t.Fatal("no MED notification")
	}
	return pixapi.InfractionReport{}
}

// A MED claim blocks what the client's account holds of it; agreeing returns it to the
// payer, of nature MED_FRAUDE, and contesting the return gets it back when the payer's
// bank upholds the contestation.
func TestAMEDClaim(t *testing.T) {
	e := newEnv(t)
	cob := e.createCob(txid, 10000)
	res := e.pay(pix.Payment{BRCode: *cob.PixCopiaECola, PayerTaxID: "12345678909"})
	e.notification()
	claim := e.claim(pix.InfractionParams{EndToEndID: res.EndToEndID, Amount: 8000, Upheld: true})
	if claim.Status != pixapi.InfractionOpen || claim.Valor != "80.00" || e.sim.Blocked(clientID) != 8000 {
		t.Fatalf("the claim: %+v, blocked %d", claim, e.sim.Blocked(clientID))
	}
	// What a claim blocks cannot be spent: the account holds 1,100.00, 80.00 of it blocked.
	e.sim.AddKey(pix.Entry{Key: "seller@example.com", ISPB: "30000003", Name: "Vendedor", TaxID: "98765432100"})
	var tr pix.Transfer
	e.call(http.MethodPut, "/transferencias/po1", pix.TransferRequest{Valor: "1050.00", Chave: "seller@example.com"}, &tr)
	if tr.Status != "NAO_REALIZADO" {
		t.Fatalf("spending blocked money: %+v", tr)
	}
	var out pixapi.InfractionReport
	if code := e.call(http.MethodPost, "/infracoes/"+claim.ID+"/analise", pixapi.InfractionAnalysis{
		AnalysisResult: pixapi.AnalysisAgreed, RefundID: "dp1", Valor: "80.00",
		FundsTrace: []pixapi.TraceHop{{EndToEndID: "E30000001202610051500abcdefghijk", Valor: "10.00"}},
	}, &out); code != http.StatusOK || out.Status != pixapi.InfractionClosed || out.RefundStatus != "DEVOLVIDO" || len(out.FundsTrace) != 1 {
		t.Fatalf("agreeing: %d %+v", code, out)
	}
	if e.sim.Blocked(clientID) != 0 || e.sim.Balance(clientID) != 110000-8000 {
		t.Fatalf("after the return: blocked %d, balance %d", e.sim.Blocked(clientID), e.sim.Balance(clientID))
	}
	var d pixapi.Devolucao
	if code := e.call(http.MethodGet, "/pix/"+res.EndToEndID+"/devolucao/dp1", nil, &d); code != http.StatusOK || d.Natureza == nil || *d.Natureza != pixapi.DevolucaoNaturezaMEDFRAUDE {
		t.Fatalf("the MED return: %d %+v", code, d)
	}
	if code := e.call(http.MethodPost, "/infracoes/"+claim.ID+"/analise", pixapi.InfractionAnalysis{AnalysisResult: pixapi.AnalysisDisagreed}, nil); code != http.StatusConflict {
		t.Fatalf("a different answer after the first: %d", code)
	}
	if code := e.call(http.MethodPost, "/infracoes/"+claim.ID+"/contestacao", pixapi.InfractionContestation{Details: "venda legítima"}, &out); code != http.StatusOK ||
		out.Contestation == nil || out.Contestation.Status != pixapi.ContestationUpheld || e.sim.Balance(clientID) != 110000 {
		t.Fatalf("contesting: %d %+v, balance %d", code, out, e.sim.Balance(clientID))
	}
}

// A claim no one answers loses its block after 11 days; claims come only within 80 days
// of the Pix, for no more than is left of it.
func TestMEDWindows(t *testing.T) {
	e := newEnv(t)
	cob := e.createCob(txid, 5000)
	res := e.pay(pix.Payment{BRCode: *cob.PixCopiaECola, PayerTaxID: "12345678909"})
	e.notification()
	if _, err := e.sim.ReportInfraction(t.Context(), pix.InfractionParams{EndToEndID: res.EndToEndID, Amount: 5001}); err == nil {
		t.Fatal("a claim for more than the Pix")
	}
	claim := e.claim(pix.InfractionParams{EndToEndID: res.EndToEndID})
	e.clock.Advance(11*24*time.Hour + time.Minute)
	e.token = e.issueToken(e.client)
	var out pixapi.InfractionReports
	start := e.clock.Now().Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	if code := e.call(http.MethodGet, "/infracoes?inicio="+start+"&fim="+e.clock.Now().Format(time.RFC3339), nil, &out); code != http.StatusOK ||
		len(out.InfractionReports) != 1 || out.InfractionReports[0].ID != claim.ID || e.sim.Blocked(clientID) != 0 {
		t.Fatalf("listing after the block ended: %d %+v, blocked %d", code, out, e.sim.Blocked(clientID))
	}
	e.clock.Advance(70 * 24 * time.Hour)
	if _, err := e.sim.ReportInfraction(t.Context(), pix.InfractionParams{EndToEndID: res.EndToEndID, Amount: 100}); err == nil {
		t.Fatal("a claim 81 days after the Pix")
	}
}
