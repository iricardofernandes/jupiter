package pix_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/iricardofernandes/jupiter/internal/sim/pix"
	"github.com/iricardofernandes/jupiter/pkg/brcode"
	"github.com/iricardofernandes/jupiter/pkg/pixapi"
)

const payer = "12345678909"

func (e *env) createRec(retry string, loc *int64) pixapi.RecCompleta {
	e.t.Helper()
	body := pixapi.RecSolicitada{PoliticaRetentativa: pixapi.RecSolicitadaPoliticaRetentativa(retry), Loc: loc}
	body.Vinculo.Contrato, body.Vinculo.Objeto = "sub_42", pixapi.Ptr("Plano mensal")
	body.Vinculo.Devedor = pixapi.Pessoa{Cpf: pixapi.Ptr(payer), Nome: pixapi.Ptr("Maria")}
	_ = body.Calendario.DataInicial.UnmarshalText([]byte("2026-10-20"))
	body.Calendario.Periodicidade = "MENSAL"
	body.Valor = pixapi.New(&body.Valor)
	body.Valor.ValorRec = pixapi.Ptr("49.90")
	var out pixapi.RecCompleta
	if code := e.call(http.MethodPost, "/rec", body, &out); code != http.StatusCreated {
		e.t.Fatalf("POST /rec: %d", code)
	}
	return out
}

func (e *env) nextRec() pixapi.RecNotification {
	e.t.Helper()
	select {
	case n := <-e.recs:
		return n
	case <-time.After(5 * time.Second):
		e.t.Fatal("no recurrence notification")
	}
	return pixapi.RecNotification{}
}

func (e *env) nextCobr() pixapi.CobRNotification {
	e.t.Helper()
	select {
	case n := <-e.cobrs:
		return n
	case <-time.After(5 * time.Second):
		e.t.Fatal("no recurring charge notification")
	}
	return pixapi.CobRNotification{}
}

// approved makes and approves a recurrence by journey 1.
func (e *env) approved(retry string) pixapi.RecCompleta {
	e.t.Helper()
	rec := e.createRec(retry, nil)
	var req pixapi.SolicRecCompleta
	body := pixapi.SolicRecBase{IdRec: rec.IdRec, Destinatario: pixapi.Destinatario{Conta: "12345", IspbParticipante: pix.PayerISPB, Cpf: pixapi.Ptr(payer)}}
	body.Calendario.DataExpiracaoSolicitacao = e.clock.Now().Add(48 * time.Hour)
	if code := e.call(http.MethodPost, "/solicrec", body, &req); code != http.StatusCreated || req.Status != "RECEBIDA" {
		e.t.Fatalf("POST /solicrec: %d %+v", code, req)
	}
	if err := e.sim.DecideRequest(context.Background(), req.IdSolicRec, true); err != nil {
		e.t.Fatal(err)
	}
	if n := e.nextRec(); n.Status != "APROVADA" || n.Ativacao == nil || n.Ativacao.TipoJornada != "JORNADA_1" {
		e.t.Fatalf("notified: %+v", n)
	}
	return rec
}

func TestJourneyOne(t *testing.T) {
	e := newEnv(t)
	rec := e.createRec("NAO_PERMITE", nil)
	if !pixapi.ValidRecID(rec.IdRec) || rec.IdRec[:2] != "RN" || rec.Status != "CRIADA" || rec.Ativacao.TipoJornada != "AGUARDANDO_DEFINICAO" {
		t.Fatalf("created: %+v", rec)
	}
	var req pixapi.SolicRecCompleta
	body := pixapi.SolicRecBase{IdRec: rec.IdRec, Destinatario: pixapi.Destinatario{Conta: "12345", IspbParticipante: pix.PayerISPB, Cpf: pixapi.Ptr(payer)}}
	body.Calendario.DataExpiracaoSolicitacao = e.clock.Now().Add(48 * time.Hour)
	e.call(http.MethodPost, "/solicrec", body, &req)
	if err := e.sim.DecideRequest(context.Background(), req.IdSolicRec, false); err != nil {
		t.Fatal(err)
	}
	n := e.nextRec()
	if n.Status != "REJEITADA" || n.Encerramento == nil || n.Encerramento.Rejeicao == nil || *n.Encerramento.Rejeicao.Codigo != "AP13" {
		t.Fatalf("a rejection: %+v", n)
	}
	body.Destinatario.IspbParticipante = "99999999"
	other := e.createRec("NAO_PERMITE", nil)
	body.IdRec = other.IdRec
	e.call(http.MethodPost, "/solicrec", body, &req)
	if req.Status != "REJEITADA" || req.Encerramento == nil {
		t.Fatalf("to a bank that is not there: %+v", req)
	}
}

func TestJourneyTwo(t *testing.T) {
	e := newEnv(t)
	var loc pixapi.PayloadLocationRecGerada
	if code := e.call(http.MethodPost, "/locrec", nil, &loc); code != http.StatusCreated {
		t.Fatalf("POST /locrec: %d", code)
	}
	rec := e.createRec("PERMITE_3R_7D", &loc.Id)
	if rec.IdRec[:2] != "RR" || rec.DadosQR == nil || rec.DadosQR.PixCopiaECola == nil {
		t.Fatalf("created: %+v", rec)
	}
	code, err := brcode.Parse(*rec.DadosQR.PixCopiaECola)
	if err != nil || code.Kind() != brcode.Composite || code.RecurrenceURL != *loc.Location {
		t.Fatalf("the QR code: %+v, %v", code, err)
	}
	if res := e.pay(pix.Payment{BRCode: *rec.DadosQR.PixCopiaECola, PayerTaxID: "98765432100"}); res.Refused == "" {
		t.Fatalf("another payer authorized it: %+v", res)
	}
	res := e.pay(pix.Payment{BRCode: *rec.DadosQR.PixCopiaECola, PayerTaxID: payer})
	if res.Authorized != rec.IdRec {
		t.Fatalf("authorizing: %+v", res)
	}
	if n := e.nextRec(); n.Status != "APROVADA" || n.Ativacao.TipoJornada != "JORNADA_2" {
		t.Fatalf("notified: %+v", n)
	}
}

func (e *env) createCobr(idRec, txid, due string) (pixapi.CobRCompleta, int) {
	e.t.Helper()
	var body pixapi.CobRSolicitada
	body.IdRec, body.AjusteDiaUtil, body.Valor.Original = idRec, true, "49.90"
	_ = body.Calendario.DataDeVencimento.UnmarshalText([]byte(due))
	body.Recebedor = pixapi.DadosBancariosRecebedor{Conta: clientID, TipoConta: "PAGAMENTO"}
	var out pixapi.CobRCompleta
	code := e.call(http.MethodPut, "/cobr/"+txid, body, &out)
	return out, code
}

// day moves the clock a day and lets the bank work.
func (e *env) day(n int) {
	for range n {
		e.clock.Advance(24 * time.Hour)
		e.sim.Tick(context.Background())
	}
	e.token = e.issueToken(e.client)
}

func TestARecurringChargeIsScheduledAndPaid(t *testing.T) {
	e := newEnv(t)
	rec := e.approved("NAO_PERMITE")
	cobr, code := e.createCobr(rec.IdRec, txid, "2026-10-20")
	if code != http.StatusCreated || cobr.Status != "CRIADA" {
		t.Fatalf("PUT /cobr: %d %+v", code, cobr)
	}
	e.day(4) // 2026-10-09: outside the ten-day window
	e.call(http.MethodGet, "/cobr/"+txid, nil, &cobr)
	if cobr.Status != "CRIADA" {
		t.Fatalf("scheduled before the window: %s", cobr.Status)
	}
	e.day(1) // 2026-10-10
	n := e.nextCobr()
	if n.Status != "ATIVA" || len(*n.Tentativas) != 1 || (*n.Tentativas)[0].Status != "AGENDADA" || (*n.Tentativas)[0].Tipo != "AGND" {
		t.Fatalf("scheduled: %+v", n)
	}
	e.day(10) // 2026-10-20
	n = e.nextCobr()
	if n.Status != "CONCLUIDA" || n.Pix == nil || (*n.Pix)[0].Valor != "49.90" || (*n.Pix)[0].EndToEndId != (*n.Tentativas)[0].EndToEndId {
		t.Fatalf("paid: %+v", n)
	}
	var got pixapi.Pix
	if code := e.call(http.MethodGet, "/pix/"+(*n.Pix)[0].EndToEndId, nil, &got); code != http.StatusOK || *got.Txid != txid {
		t.Fatalf("GET /pix: %d %+v", code, got)
	}
	if _, code := e.createCobr(rec.IdRec, "pa01m3sw7skseyk9y97z426m10e7", "2026-11-01"); code != http.StatusBadRequest {
		t.Fatalf("a second charge in the cycle: %d", code)
	}
}

func TestRetriesAfterTheDueDate(t *testing.T) {
	e := newEnv(t)
	rec := e.approved("PERMITE_3R_7D")
	e.sim.SetPayerFunds(payer, 1000)
	e.createCobr(rec.IdRec, txid, "2026-10-20")
	e.day(15) // 2026-10-20: no funds
	e.nextCobr()
	e.day(1) // 2026-10-21: the first attempt expired
	n := e.nextCobr()
	if n.Status != "ATIVA" || (*n.Tentativas)[0].Status != "EXPIRADA" {
		t.Fatalf("after the first attempt: %+v", n)
	}
	var out pixapi.CobRCompleta
	if code := e.call(http.MethodPost, "/cobr/"+txid+"/retentativa/2026-10-21", nil, &out); code != http.StatusBadRequest {
		t.Fatalf("a retry for today: %d", code)
	}
	if code := e.call(http.MethodPost, "/cobr/"+txid+"/retentativa/2026-10-28", nil, &out); code != http.StatusBadRequest {
		t.Fatalf("a retry past seven days: %d", code)
	}
	if code := e.call(http.MethodPost, "/cobr/"+txid+"/retentativa/2026-10-22", nil, &out); code != http.StatusCreated {
		t.Fatalf("a retry: %d", code)
	}
	e.nextCobr()
	e.day(1) // 2026-10-22: still no funds
	e.day(1)
	e.nextCobr()
	e.call(http.MethodPost, "/cobr/"+txid+"/retentativa/2026-10-24", nil, &out)
	e.nextCobr()
	e.sim.SetPayerFunds(payer, -1)
	e.day(1)
	e.day(1) // 2026-10-24
	n = e.nextCobr()
	if n.Status != "CONCLUIDA" || len(*n.Tentativas) != 3 || (*n.Tentativas)[2].Tipo != "NTAG" || (*n.Tentativas)[2].Status != "PAGA" {
		t.Fatalf("paid on the second retry: %+v", n)
	}
	e1, e3 := (*n.Tentativas)[0].EndToEndId, (*n.Tentativas)[2].EndToEndId
	if e1 == e3 {
		t.Fatal("a retry with the first attempt's endToEndId")
	}
}

func TestACobrWithoutRetriesExpires(t *testing.T) {
	e := newEnv(t)
	rec := e.approved("NAO_PERMITE")
	e.sim.SetPayerFunds(payer, 0)
	e.createCobr(rec.IdRec, txid, "2026-10-20")
	e.day(16)
	e.nextCobr()
	if n := e.nextCobr(); n.Status != "EXPIRADA" {
		t.Fatalf("unpaid: %+v", n)
	}
}

func TestCancellations(t *testing.T) {
	e := newEnv(t)
	rec := e.approved("NAO_PERMITE")
	e.createCobr(rec.IdRec, txid, "2026-10-20")
	e.day(14) // 2026-10-19, the day before
	e.nextCobr()
	var out pixapi.CobRCompleta
	if code := e.call(http.MethodPatch, "/cobr/"+txid, map[string]string{"status": "CANCELADA"}, &out); code != http.StatusOK || out.Status != "CANCELADA" {
		t.Fatalf("the receiver canceling on the day before: %d %+v", code, out)
	}
	if n := e.nextCobr(); n.Encerramento == nil || n.Encerramento.Cancelamento == nil || *n.Encerramento.Cancelamento.Solicitante != "USUARIO_RECEBEDOR" {
		t.Fatalf("notified: %+v", n)
	}
	other := "pa01m3sw7skseyk9y97z426m10e8"
	if _, code := e.createCobr(rec.IdRec, other, "2026-11-20"); code != http.StatusCreated {
		t.Fatalf("the next cycle's charge: %d", code)
	}
	if err := e.sim.CancelAsPayer(context.Background(), rec.IdRec); err != nil {
		t.Fatal(err)
	}
	if n := e.nextRec(); n.Status != "CANCELADA" || *n.Encerramento.Cancelamento.Solicitante != "USUARIO_PAGADOR" {
		t.Fatalf("the payer canceling: %+v", n)
	}
	if n := e.nextCobr(); n.Status != "CANCELADA" || n.Txid != other {
		t.Fatalf("the charge under it: %+v", n)
	}
	if _, code := e.createCobr(rec.IdRec, "pa01m3sw7skseyk9y97z426m10e9", "2026-12-20"); code != http.StatusBadRequest {
		t.Fatalf("a charge under a canceled recurrence: %d", code)
	}
}

func TestChargeRules(t *testing.T) {
	e := newEnv(t)
	rec := e.approved("NAO_PERMITE")
	if _, code := e.createCobr(rec.IdRec, txid, "2026-10-06"); code != http.StatusBadRequest {
		t.Fatalf("due tomorrow: %d", code)
	}
	var body pixapi.CobRSolicitada
	body.IdRec, body.Valor.Original = rec.IdRec, "50.00"
	_ = body.Calendario.DataDeVencimento.UnmarshalText([]byte("2026-10-20"))
	body.Recebedor = pixapi.DadosBancariosRecebedor{Conta: clientID, TipoConta: "PAGAMENTO"}
	if code := e.call(http.MethodPut, "/cobr/"+txid, body, nil); code != http.StatusBadRequest {
		t.Fatalf("an amount other than the recurrence's: %d", code)
	}
	body.Valor.Original, body.Recebedor.Conta = "49.90", "someone-else"
	if code := e.call(http.MethodPut, "/cobr/"+txid, body, nil); code != http.StatusBadRequest {
		t.Fatalf("to an account not the client's: %d", code)
	}
}
