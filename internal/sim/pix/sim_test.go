package pix_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iricardofernandes/jupiter/internal/platform/mtls"
	"github.com/iricardofernandes/jupiter/internal/sim/pix"
	"github.com/iricardofernandes/jupiter/pkg/brcode"
	"github.com/iricardofernandes/jupiter/pkg/pixapi"
)

const (
	clientID = "jupiter"
	secret   = "s3cret"
	key      = "0d9c3f4e-3a2b-4c1d-9e8f-7a6b5c4d3e2f"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type env struct {
	t        *testing.T
	sim      *pix.Sim
	server   *httptest.Server
	pki      *mtls.PKI
	clock    *clock
	client   *http.Client
	token    string
	received chan pixapi.WebhookPixBody
	// recs and cobrs receive the Pix Automático notifications.
	recs  chan pixapi.RecNotification
	cobrs chan pixapi.CobRNotification
	// infractions receive MED claims.
	infractions chan pixapi.InfractionReport
	faults      func(pix.Event) pix.Fault
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pki, err := mtls.NewPKI("pix-sim-test")
	if err != nil {
		t.Fatal(err)
	}
	e := &env{
		t: t, pki: pki, clock: &clock{now: time.Date(2026, 10, 5, 15, 0, 0, 0, time.UTC)}, received: make(chan pixapi.WebhookPixBody, 10),
		recs: make(chan pixapi.RecNotification, 50), cobrs: make(chan pixapi.CobRNotification, 50),
		infractions: make(chan pixapi.InfractionReport, 10),
	}

	// Jupiter's side of notifications: mutual TLS, admitting the bank's identity.
	receiverCert, _ := pki.Server("127.0.0.1")
	receiver := httptest.NewUnstartedServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/rec"):
			var body pixapi.WebhookRecBody
			_ = json.NewDecoder(r.Body).Decode(&body)
			for _, n := range *body.Recs {
				e.recs <- n
			}
		case strings.HasSuffix(r.URL.Path, "/infracoes"):
			var body pixapi.InfractionReports
			_ = json.NewDecoder(r.Body).Decode(&body)
			for _, n := range body.InfractionReports {
				e.infractions <- n
			}
		case strings.HasSuffix(r.URL.Path, "/cobr"):
			var body pixapi.WebhookCobRBody
			_ = json.NewDecoder(r.Body).Decode(&body)
			for _, n := range *body.Cobsr {
				e.cobrs <- n
			}
		default:
			var body pixapi.WebhookPixBody
			_ = json.NewDecoder(r.Body).Decode(&body)
			e.received <- body
		}
	}))
	receiver.TLS = mtls.ServerConfig(receiverCert.TLS, pki.Pool(), "spiffe://sim-pix/webhook")
	receiver.StartTLS()
	t.Cleanup(receiver.Close)

	bankCert, _ := pki.Server("127.0.0.1")
	webhookCert, _ := pki.Client("spiffe://sim-pix/webhook")
	var handler http.Handler
	e.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	e.server.TLS = pix.ServerTLS(bankCert.TLS, pki.Pool())
	e.sim, err = pix.New(pix.Config{
		Now: e.clock.Now, Host: e.server.Listener.Addr().String(),
		Clients:    []pix.Client{{ID: clientID, Secret: secret, Name: "Jupiter Pagamentos", TaxID: "12345678000195", Keys: []string{key}, Balance: 100000}},
		WebhookTLS: mtls.ClientConfig(webhookCert.TLS, pki.Pool()), PayerTLS: &tls.Config{RootCAs: pki.Pool(), MinVersion: tls.VersionTLS12},
		Faults: func(ev pix.Event) pix.Fault {
			if e.faults != nil {
				return e.faults(ev)
			}
			return pix.Fault{}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler = e.sim.Handler()
	e.server.StartTLS()
	t.Cleanup(e.server.Close)
	t.Cleanup(e.sim.Close)

	e.client = e.clientWith("spiffe://jupiter/pix")
	e.token = e.issueToken(e.client)
	for _, path := range []string{"/webhook/" + key, "/webhookrec", "/webhookcobr"} {
		if code := e.call(http.MethodPut, path, map[string]string{"webhookUrl": receiver.URL + "/pix/webhook"}, nil); code != http.StatusOK {
			t.Fatalf("registering %s: %d", path, code)
		}
	}
	return e
}

func (e *env) clientWith(identity string) *http.Client {
	cert, err := e.pki.Client(identity)
	if err != nil {
		e.t.Fatal(err)
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: mtls.ClientConfig(cert.TLS, e.pki.Pool())}}
}

func (e *env) issueToken(c *http.Client) string {
	e.t.Helper()
	req, _ := http.NewRequestWithContext(e.t.Context(), http.MethodPost, e.server.URL+"/oauth/token",
		strings.NewReader(url.Values{"grant_type": {"client_credentials"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(clientID, secret)
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusOK || out.AccessToken == "" {
		e.t.Fatalf("token: %d", resp.StatusCode)
	}
	return out.AccessToken
}

func (e *env) callWith(c *http.Client, token, method, path string, body, out any) int {
	e.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, _ := http.NewRequestWithContext(e.t.Context(), method, e.server.URL+path, reader)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil {
		_ = json.Unmarshal(raw, out)
	}
	return resp.StatusCode
}

func (e *env) call(method, path string, body, out any) int {
	e.t.Helper()
	return e.callWith(e.client, e.token, method, path, body, out)
}

func (e *env) createCob(txid string, amount int64) pixapi.CobCompleta {
	e.t.Helper()
	var body pixapi.CobSolicitada
	body.Chave, body.Valor.Original = key, pixapi.FormatValor(amount)
	body.Calendario.Expiracao = pixapi.Ptr(int32(3600))
	var out pixapi.CobCompleta
	if code := e.call(http.MethodPut, "/cob/"+txid, body, &out); code != http.StatusCreated {
		e.t.Fatalf("PUT /cob: %d", code)
	}
	return out
}

func (e *env) pay(p pix.Payment) pix.PaymentResult {
	e.t.Helper()
	res, err := e.sim.Pay(context.Background(), p)
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}

func (e *env) notification() pixapi.Pix {
	e.t.Helper()
	select {
	case body := <-e.received:
		if body.Pix == nil || len(*body.Pix) != 1 {
			e.t.Fatalf("notification: %+v", body)
		}
		return (*body.Pix)[0]
	case <-time.After(5 * time.Second):
		e.t.Fatal("no notification")
	}
	return pixapi.Pix{}
}

const txid = "pa01m3sw7skseyk9y97z426m10e6"

// Phase 7 exit criterion: a token is good only with the certificate it was issued to.
func TestATokenIsBoundToItsCertificate(t *testing.T) {
	e := newEnv(t)
	if code := e.call(http.MethodGet, "/cob/"+txid, nil, nil); code != http.StatusNotFound {
		t.Fatalf("with its own certificate: %d", code)
	}
	other := e.clientWith("spiffe://jupiter/pix")
	var problem pixapi.Problema
	if code := e.callWith(other, e.token, http.MethodGet, "/cob/"+txid, nil, &problem); code != http.StatusUnauthorized ||
		problem.Type != pixapi.ErrorType("AcessoNegado") {
		t.Fatalf("with another certificate: %d %+v", code, problem)
	}
	noCert := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: e.pki.Pool(), MinVersion: tls.VersionTLS12}}}
	if code := e.callWith(noCert, e.token, http.MethodGet, "/cob/"+txid, nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("without a certificate: %d", code)
	}
	if code := e.callWith(e.client, "forged", http.MethodGet, "/cob/"+txid, nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("with an unknown token: %d", code)
	}
	e.clock.Advance(2 * time.Hour)
	if code := e.call(http.MethodGet, "/cob/"+txid, nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("with an expired token: %d", code)
	}
}

func TestADynamicCodeIsPaidOnce(t *testing.T) {
	e := newEnv(t)
	cob := e.createCob(txid, 12345)
	if cob.Status != pixapi.CobStatusATIVA || cob.PixCopiaECola == nil || cob.Location == nil {
		t.Fatalf("created: %+v", cob)
	}
	code, err := brcode.Parse(*cob.PixCopiaECola)
	if err != nil || code.Kind() != brcode.Dynamic || code.URL != *cob.Location {
		t.Fatalf("copy and paste %q: %+v, %v", *cob.PixCopiaECola, code, err)
	}
	res := e.pay(pix.Payment{BRCode: *cob.PixCopiaECola, PayerName: "Maria", PayerTaxID: "12345678909"})
	if res.Refused != "" || !pixapi.ValidEndToEndID(res.EndToEndID) || res.Amount != "123.45" {
		t.Fatalf("paying: %+v", res)
	}
	got := e.notification()
	if got.EndToEndId != res.EndToEndID || *got.Txid != txid || got.Valor != "123.45" {
		t.Fatalf("notified: %+v", got)
	}
	var after pixapi.CobCompleta
	e.call(http.MethodGet, "/cob/"+txid, nil, &after)
	if after.Status != pixapi.CobStatusCONCLUIDA || after.Pix == nil || (*after.Pix)[0].EndToEndId != res.EndToEndID {
		t.Fatalf("after payment: %+v", after)
	}
	if again := e.pay(pix.Payment{BRCode: *cob.PixCopiaECola}); again.Refused == "" {
		t.Fatalf("paid twice: %+v", again)
	}
	if e.sim.Balance(clientID) != 100000+12345 {
		t.Fatalf("balance %d", e.sim.Balance(clientID))
	}
}

func TestExpiredAndRemovedChargesCannotBePaid(t *testing.T) {
	e := newEnv(t)
	cob := e.createCob(txid, 1000)
	e.clock.Advance(time.Hour)
	if res := e.pay(pix.Payment{BRCode: *cob.PixCopiaECola}); res.Refused == "" {
		t.Fatalf("an expired charge was paid: %+v", res)
	}
	e.token = e.issueToken(e.client)
	var still pixapi.CobCompleta
	e.call(http.MethodGet, "/cob/"+txid, nil, &still)
	if still.Status != pixapi.CobStatusATIVA {
		t.Fatalf("expiry is not a status: %s", still.Status)
	}

	other := "pa01m3sw7skseyk9y97z426m10e7"
	cob = e.createCob(other, 1000)
	var removed pixapi.CobCompleta
	if code := e.call(http.MethodPatch, "/cob/"+other, map[string]string{"status": "REMOVIDA_PELO_USUARIO_RECEBEDOR"}, &removed); code != http.StatusOK ||
		removed.Status != pixapi.CobStatusREMOVIDAPELOUSUARIORECEBEDOR || *removed.Revisao != 1 {
		t.Fatalf("removing: %d %+v", code, removed)
	}
	if res := e.pay(pix.Payment{BRCode: *cob.PixCopiaECola}); res.Refused == "" {
		t.Fatalf("a removed charge was paid: %+v", res)
	}
	var problem pixapi.Problema
	var body pixapi.CobSolicitada
	body.Chave, body.Valor.Original = key, "10.00"
	if code := e.call(http.MethodPut, "/cob/"+other, body, &problem); code != http.StatusBadRequest || !strings.Contains(*problem.Detail, "já utilizado") {
		t.Fatalf("reusing a txid: %d %+v", code, problem)
	}
}

func TestADueDateChargeAddsUp(t *testing.T) {
	e := newEnv(t)
	var body pixapi.CobVSolicitada
	body.Chave, body.Valor.Original = key, "100.00"
	_ = body.Calendario.DataDeVencimento.UnmarshalText([]byte("2026-10-10"))
	body.Devedor = pixapi.Pessoa{Cpf: pixapi.Ptr("12345678909"), Nome: pixapi.Ptr("Maria")}
	multa := pixapi.New(&body.Valor.Multa)
	multa.Modalidade, multa.ValorPerc = 2, "2.00"
	juros := pixapi.New(&body.Valor.Juros)
	juros.Modalidade, juros.ValorPerc = 3, "1.50"
	desconto := pixapi.New(&body.Valor.Desconto)
	desconto.Modalidade = 1
	item := pixapi.Append(&desconto.DescontoDataFixa)
	_ = item.Data.UnmarshalText([]byte("2026-10-07"))
	item.ValorPerc = "5.00"
	var cob pixapi.CobVCompleta
	if code := e.call(http.MethodPut, "/cobv/"+txid, body, &cob); code != http.StatusCreated {
		t.Fatalf("PUT /cobv: %d", code)
	}
	// On 2026-10-05, before the discount date: R$ 100.00 - 5.00.
	if final := finalAmount(e.payload(*cob.PixCopiaECola, "")); final != "95.00" {
		t.Fatalf("early: %s", final)
	}
	// Twenty days late: 2% fine and 1.5% a month for 20 days, 1.00.
	if final := finalAmount(e.payload(*cob.PixCopiaECola, "2026-10-30")); final != "103.00" {
		t.Fatalf("late: %s", final)
	}
	e.clock.Advance(25 * 24 * time.Hour) // 2026-10-30
	res := e.pay(pix.Payment{BRCode: *cob.PixCopiaECola})
	if res.Amount != "103.00" {
		t.Fatalf("paying late: %+v", res)
	}
	if got := e.notification(); got.Valor != "103.00" || got.ComponentesValor == nil {
		t.Fatalf("notified: %+v", got)
	}
	e.clock.Advance(40 * 24 * time.Hour)
	var expired pixapi.CobVSolicitada
	expired.Chave, expired.Valor.Original, expired.Devedor = key, "1.00", body.Devedor
	_ = expired.Calendario.DataDeVencimento.UnmarshalText([]byte("2026-10-01"))
	e.token = e.issueToken(e.client)
	if code := e.call(http.MethodPut, "/cobv/pa01m3sw7skseyk9y97z426m10e8", expired, nil); code != http.StatusBadRequest {
		t.Fatalf("a due date in the past: %d", code)
	}
}

func finalAmount(payload map[string]any) string {
	valor, _ := payload["valor"].(map[string]any)
	final, _ := valor["final"].(string)
	return final
}

// payload fetches a dynamic code's payload as a payer would, without checking it.
func (e *env) payload(copyPaste, dpp string) map[string]any {
	e.t.Helper()
	code, _ := brcode.Parse(copyPaste)
	u := "https://" + code.URL
	if dpp != "" {
		u += "?DPP=" + dpp
	}
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: e.pki.Pool(), MinVersion: tls.VersionTLS12}}}
	req, _ := http.NewRequestWithContext(e.t.Context(), http.MethodGet, u, nil)
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.Header.Get("Content-Type") != "application/jose" {
		e.t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	parts := strings.Split(string(raw), ".")
	body, _ := decode(parts[1])
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	return out
}

func decode(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

func TestReturns(t *testing.T) {
	e := newEnv(t)
	cob := e.createCob(txid, 10000)
	res := e.pay(pix.Payment{BRCode: *cob.PixCopiaECola, PayerTaxID: "12345678909"})
	e.notification()
	var d pixapi.Devolucao
	if code := e.call(http.MethodPut, "/pix/"+res.EndToEndID+"/devolucao/re1", map[string]string{"valor": "30.00"}, &d); code != http.StatusCreated ||
		d.Status != pixapi.DevolucaoStatusEMPROCESSAMENTO {
		t.Fatalf("a partial return: %d %+v", code, d)
	}
	got := e.notification()
	if got.Devolucoes == nil || (*got.Devolucoes)[0].Status != pixapi.DevolucaoStatusDEVOLVIDO {
		t.Fatalf("notified: %+v", got)
	}
	if code := e.call(http.MethodPut, "/pix/"+res.EndToEndID+"/devolucao/re1", map[string]string{"valor": "30.00"}, &d); code != http.StatusCreated || d.Status != pixapi.DevolucaoStatusDEVOLVIDO {
		t.Fatalf("asking again: %d %+v", code, d)
	}
	if code := e.call(http.MethodPut, "/pix/"+res.EndToEndID+"/devolucao/re2", map[string]string{"valor": "70.01"}, nil); code != http.StatusBadRequest {
		t.Fatalf("returning more than was paid: %d", code)
	}
	e.sim.ClosePayer("12345678909")
	e.call(http.MethodPut, "/pix/"+res.EndToEndID+"/devolucao/re3", map[string]string{"valor": "70.00"}, nil)
	if got := e.notification(); (*got.Devolucoes)[1].Status != pixapi.DevolucaoStatusNAOREALIZADO {
		t.Fatalf("to a closed account: %+v", *got.Devolucoes)
	}
	if e.sim.Balance(clientID) != 100000+10000-3000 {
		t.Fatalf("balance %d", e.sim.Balance(clientID))
	}
}

func TestStaticCodesAndKeys(t *testing.T) {
	e := newEnv(t)
	static, _ := brcode.Pix{Key: key, Amount: "15.00", MerchantName: "Jupiter", MerchantCity: "SAO PAULO", TxID: "pedido42"}.Encode()
	withTxID := e.pay(pix.Payment{BRCode: static})
	if got := e.notification(); got.EndToEndId != withTxID.EndToEndID || *got.Txid != "pedido42" {
		t.Fatalf("notified: %+v", got)
	}
	plain := e.pay(pix.Payment{Key: key, Amount: "7.50"})
	select {
	case body := <-e.received:
		t.Fatalf("a Pix without a txid was notified: %+v", body)
	case <-time.After(100 * time.Millisecond):
	}
	var list pixapi.PixConsultados
	q := url.Values{"inicio": {"2026-10-05T00:00:00Z"}, "fim": {"2026-10-06T00:00:00Z"}}
	if code := e.call(http.MethodGet, "/pix?"+q.Encode(), nil, &list); code != http.StatusOK || len(*list.Pix) != 2 {
		t.Fatalf("GET /pix: %d %+v", code, list)
	}
	var one pixapi.Pix
	if code := e.call(http.MethodGet, "/pix/"+plain.EndToEndID, nil, &one); code != http.StatusOK || one.Valor != "7.50" || one.Txid != nil {
		t.Fatalf("GET /pix/{e2eid}: %d %+v", code, one)
	}
	if res := e.pay(pix.Payment{Key: "unknown@example.com", Amount: "1.00"}); res.Refused == "" {
		t.Fatalf("paid a key not in DICT: %+v", res)
	}
}

func TestTransfers(t *testing.T) {
	e := newEnv(t)
	e.sim.AddKey(pix.Entry{Key: "seller@example.com", ISPB: "30000003", Name: "Vendedor", TaxID: "98765432100"})
	var out pix.Transfer
	body := pix.TransferRequest{Valor: "600.00", Chave: "seller@example.com"}
	if code := e.call(http.MethodPut, "/transferencias/po1", body, &out); code != http.StatusCreated || out.Status != "REALIZADO" ||
		out.Favorecido == nil || out.Favorecido.Nome != "Vendedor" {
		t.Fatalf("a transfer: %d %+v", code, out)
	}
	if code := e.call(http.MethodPut, "/transferencias/po1", body, &out); code != http.StatusOK || out.Status != "REALIZADO" {
		t.Fatalf("asking again: %d %+v", code, out)
	}
	if e.sim.Balance(clientID) != 100000-60000 {
		t.Fatalf("balance %d", e.sim.Balance(clientID))
	}
	e.call(http.MethodPut, "/transferencias/po2", pix.TransferRequest{Valor: "1.00", Chave: "nobody@example.com"}, &out)
	if out.Status != "NAO_REALIZADO" {
		t.Fatalf("to an unknown key: %+v", out)
	}
	e.call(http.MethodPut, "/transferencias/po3", pix.TransferRequest{Valor: "500.00", Chave: "seller@example.com"}, &out)
	if out.Status != "NAO_REALIZADO" || !strings.Contains(out.Motivo, "Saldo") {
		t.Fatalf("without the funds: %+v", out)
	}

	e.faults = func(ev pix.Event) pix.Fault {
		return pix.Fault{Pending: ev.ID == "po4", PendingFor: time.Minute}
	}
	e.call(http.MethodPut, "/transferencias/po4", pix.TransferRequest{Valor: "1.00", Chave: "seller@example.com"}, &out)
	if out.Status != "EM_PROCESSAMENTO" {
		t.Fatalf("held: %+v", out)
	}
	e.clock.Advance(time.Minute)
	e.call(http.MethodGet, "/transferencias/po4", nil, &out)
	if out.Status != "REALIZADO" {
		t.Fatalf("after the hold: %+v", out)
	}
}

func TestScopes(t *testing.T) {
	e := newEnv(t)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, e.server.URL+"/oauth/token",
		strings.NewReader(url.Values{"grant_type": {"client_credentials"}, "scope": {"cob.read"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(clientID, secret)
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	_ = resp.Body.Close()
	if code := e.callWith(e.client, out.AccessToken, http.MethodPut, "/cob/"+txid, map[string]any{}, nil); code != http.StatusForbidden {
		t.Fatalf("writing with a read scope: %d", code)
	}
	req, _ = http.NewRequestWithContext(t.Context(), http.MethodPost, e.server.URL+"/oauth/token",
		strings.NewReader(url.Values{"grant_type": {"client_credentials"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(clientID, "wrong")
	resp, err = e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a wrong secret: %d", resp.StatusCode)
	}
}

func TestTheAdminControlsAnswerLoopbackJSONOnly(t *testing.T) {
	e := newEnv(t)
	admin := httptest.NewServer(e.sim.AdminHandler())
	t.Cleanup(admin.Close)
	post := func(host, contentType string) int {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, admin.URL+"/admin/pay", strings.NewReader(`{"key": "x", "amount": "1.00"}`))
		req.Host = host
		req.Header.Set("Content-Type", contentType)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if code := post("evil.example:8587", "application/json"); code != http.StatusForbidden {
		t.Errorf("a rebinding host: %d", code)
	}
	if code := post("127.0.0.1:8587", "text/plain"); code != http.StatusUnsupportedMediaType {
		t.Errorf("a form post: %d", code)
	}
	if code := post("127.0.0.1:8587", "application/json"); code != http.StatusUnprocessableEntity {
		t.Errorf("a loopback JSON request: %d", code)
	}
}
