package pix_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/iricardofernandes/jupiter/internal/sim/pix"
	"github.com/iricardofernandes/jupiter/pkg/pixapi"
)

// The SPI statement lists each movement of the account; a fault loses a line, writes it
// twice, or puts it on the next day.
func TestTheStatement(t *testing.T) {
	e := newEnv(t)
	e.faults = func(ev pix.Event) pix.Fault {
		if ev.Kind != "statement" {
			return pix.Fault{}
		}
		return pix.Fault{Duplicate: ev.ID == "re1", Delay: ev.ID != "re1" && ev.ID != "po1"}
	}
	e.sim.AddKey(pix.Entry{Key: "seller@example.com", ISPB: "30000003", Name: "Vendedor", TaxID: "98765432100"})
	cob := e.createCob(txid, 10000)
	res := e.pay(pix.Payment{BRCode: *cob.PixCopiaECola, PayerTaxID: "12345678909"})
	e.notification()
	e.call(http.MethodPut, "/pix/"+res.EndToEndID+"/devolucao/re1", map[string]string{"valor": "10.00"}, nil)
	e.notification()
	e.call(http.MethodPut, "/transferencias/po1", pix.TransferRequest{Valor: "20.00", Chave: "seller@example.com"}, nil)
	day := e.clock.Now().In(time.FixedZone("BRT", -3*60*60))
	var today, tomorrow pixapi.Statement
	e.call(http.MethodGet, "/extrato?data="+day.Format(time.DateOnly), nil, &today)
	e.call(http.MethodGet, "/extrato?data="+day.AddDate(0, 0, 1).Format(time.DateOnly), nil, &tomorrow)
	if len(today.Lancamentos) != 3 || today.Lancamentos[0].Referencia != "re1" || today.Lancamentos[2].Referencia != "po1" ||
		today.Lancamentos[2].Tipo != pixapi.StatementDebit {
		t.Fatalf("today: %+v", today.Lancamentos)
	}
	if len(tomorrow.Lancamentos) != 1 || tomorrow.Lancamentos[0].Referencia != res.EndToEndID || tomorrow.Lancamentos[0].Valor != "100.00" {
		t.Fatalf("tomorrow: %+v", tomorrow.Lancamentos)
	}
	if code := e.call(http.MethodGet, "/extrato", nil, nil); code != http.StatusBadRequest {
		t.Fatalf("without a day: %d", code)
	}
}
