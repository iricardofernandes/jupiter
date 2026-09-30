package pix

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/iricardofernandes/jupiter/pkg/brcode"
	"github.com/iricardofernandes/jupiter/pkg/pixapi"
)

// Pix Automático, the receiver's side (Manual de Padrões, Anexo IV): recurrences (rec)
// the payer authorizes, by a request pushed to their bank (journey 1, solicrec) or by
// reading a QR code (journey 2), and recurring charges (cobr) sent under them.

const (
	recCreated  = "CRIADA"
	recApproved = "APROVADA"
	recRejected = "REJEITADA"
	recExpired  = "EXPIRADA"
	recCanceled = "CANCELADA"

	journeyPending = "AGUARDANDO_DEFINICAO"
	journeyRequest = "JORNADA_1"
	journeyQR      = "JORNADA_2"

	retriesNone  = "NAO_PERMITE"
	retries3R7D  = "PERMITE_3R_7D"
	maxRetries   = 3
	retryWindow  = 7
	requestSent  = "ENVIADA"
	requestSeen  = "RECEBIDA"
	requestTaken = "ACEITA"

	// The reasons Jupiter's simulator gives when a recurrence ends. The codes are the
	// specification's; which one a real PSP uses for each case is not stated in what the
	// research could read, so these choices are the simulator's.
	codePayerRefused  = "AP13"
	codeByReceiver    = "SLCR"
	codeByPayer       = "SLDB"
	codeChargeByPayer = "SLBD"
)

var periods = []string{"SEMANAL", "MENSAL", "TRIMESTRAL", "SEMESTRAL", "ANUAL"}

type statusAt struct {
	status string
	at     time.Time
}

type recurrence struct {
	client    string
	id        string
	contract  string
	object    string
	debtor    pixapi.Pessoa
	start     string
	end       string
	period    string
	fixed     int64
	minimum   int64
	retry     string
	status    string
	history   []statusAt
	journey   string
	journeyTx string
	loc       *recLocation
	payerISPB string
	ending    *pixapi.Encerramento
	requests  []*solicRec
	created   time.Time
}

type recLocation struct {
	id      int64
	token   string
	created time.Time
	rec     string
}

type solicRec struct {
	id      string
	rec     string
	expires time.Time
	to      pixapi.Destinatario
	status  string
	history []statusAt
	motive  string
}

func (r *recurrence) set(status string, at time.Time) {
	r.status = status
	r.history = append(r.history, statusAt{status, at})
}

func (q *solicRec) set(status string, at time.Time) {
	q.status = status
	q.history = append(q.history, statusAt{status, at})
}

func (r *recurrence) payerTaxID() string {
	if r.debtor.Cpf != nil {
		return *r.debtor.Cpf
	}
	if r.debtor.Cnpj != nil {
		return *r.debtor.Cnpj
	}
	return ""
}

func (s *Sim) recurrenceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /locrec", s.authorized(pixapi.ScopePayloadLocationRecWrite, s.createRecLocation))
	mux.HandleFunc("POST /rec", s.authorized(pixapi.ScopeRecWrite, s.createRecurrence))
	mux.HandleFunc("GET /rec", s.authorized(pixapi.ScopeRecRead, s.listRecurrences))
	mux.HandleFunc("GET /rec/{idRec}", s.authorized(pixapi.ScopeRecRead, func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		rec := s.recs[r.PathValue("idRec")]
		var out pixapi.RecCompleta
		if rec != nil && rec.client == clientOf(r) {
			out = s.renderRec(rec)
		}
		s.mu.Unlock()
		if out.IdRec == "" {
			problemf(w, http.StatusNotFound, "NaoEncontrado", "Recorrência não encontrada.", "Nenhuma recorrência %s.", r.PathValue("idRec"))
			return
		}
		writeJSON(w, http.StatusOK, out)
	}))
	mux.HandleFunc("PATCH /rec/{idRec}", s.authorized(pixapi.ScopeRecWrite, s.reviseRecurrence))
	mux.HandleFunc("POST /solicrec", s.authorized(pixapi.ScopeSolicRecWrite, s.createRecurrenceRequest))
	mux.HandleFunc("GET /solicrec/{id}", s.authorized(pixapi.ScopeSolicRecRead, func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		q := s.solicRecs[r.PathValue("id")]
		var out pixapi.SolicRecCompleta
		if q != nil && s.recs[q.rec].client == clientOf(r) {
			out = s.renderRequest(q)
		}
		s.mu.Unlock()
		if out.IdSolicRec == "" {
			problemf(w, http.StatusNotFound, "NaoEncontrado", "Solicitação não encontrada.", "Nenhuma solicitação %s.", r.PathValue("id"))
			return
		}
		writeJSON(w, http.StatusOK, out)
	}))
	mux.HandleFunc("PATCH /solicrec/{id}", s.authorized(pixapi.ScopeSolicRecWrite, s.cancelRecurrenceRequest))
	mux.HandleFunc("PUT /webhookrec", s.authorized(pixapi.ScopeWebhookRecWrite, func(w http.ResponseWriter, r *http.Request) {
		s.registerWebhook(w, r, func(a *account, u string) { a.recWebhook = u })
	}))
	mux.HandleFunc("PUT /webhookcobr", s.authorized(pixapi.ScopeWebhookCobRWrite, func(w http.ResponseWriter, r *http.Request) {
		s.registerWebhook(w, r, func(a *account, u string) { a.cobrWebhook = u })
	}))
	mux.HandleFunc("GET /qr/v2/rec/{token}", s.serveRecPayload)
}

func (s *Sim) registerWebhook(w http.ResponseWriter, r *http.Request, set func(*account, string)) {
	var body pixapi.WebhookSolicitado
	if err := decodeBody(r, &body); err != nil || !httpsURL(body.WebhookUrl) {
		problemf(w, http.StatusBadRequest, "WebhookOperacaoInvalida", "Webhook inválido.", "webhookUrl deve ser uma URL https.")
		return
	}
	s.mu.Lock()
	set(s.clients[clientOf(r)], body.WebhookUrl)
	s.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (s *Sim) createRecLocation(w http.ResponseWriter, _ *http.Request) {
	now := s.now()
	s.mu.Lock()
	s.nextRecLoc++
	loc := &recLocation{id: s.nextRecLoc, token: randomAlnum(32), created: now}
	s.recLocs[loc.id] = loc
	s.recTokens[loc.token] = loc
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, pixapi.PayloadLocationRecGerada{
		Id: loc.id, Location: pixapi.Ptr(s.recLocationURL(loc)), Criacao: pixapi.Ptr(now),
	})
}

func (s *Sim) recLocationURL(loc *recLocation) string {
	return s.cfg.Host + "/qr/v2/rec/" + loc.token
}

// recID makes an idRec (Anexo IV §2.1.1): R, whether retries are allowed, the ISPB, the
// day, and 11 letters and digits.
func (s *Sim) recID(retry string, at time.Time) string {
	allows := "N"
	if retry == retries3R7D {
		allows = "R"
	}
	return "R" + allows + s.cfg.ISPB + at.In(brasilia).Format("20060102") + randomAlnum(11)
}

func (s *Sim) createRecurrence(w http.ResponseWriter, r *http.Request) {
	var body pixapi.RecSolicitada
	if err := decodeBody(r, &body); err != nil {
		problemf(w, http.StatusBadRequest, "RecOperacaoInvalida", "Recorrência inválida.", "%v", err)
		return
	}
	rec, err := s.newRecurrence(clientOf(r), body)
	if err != nil {
		problemf(w, http.StatusBadRequest, "RecOperacaoInvalida", "Recorrência inválida.", "%v", err)
		return
	}
	s.mu.Lock()
	out := s.renderRec(rec)
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, out)
}

func (s *Sim) newRecurrence(client string, body pixapi.RecSolicitada) (*recurrence, error) {
	now := s.now()
	rec := &recurrence{
		client: client, contract: body.Vinculo.Contrato, debtor: body.Vinculo.Devedor, start: body.Calendario.DataInicial.String(),
		period: string(body.Calendario.Periodicidade), retry: string(body.PoliticaRetentativa), journey: journeyPending, created: now,
	}
	if body.Vinculo.Objeto != nil {
		rec.object = *body.Vinculo.Objeto
	}
	if body.Calendario.DataFinal != nil {
		rec.end = body.Calendario.DataFinal.String()
	}
	if body.Ativacao != nil && body.Ativacao.DadosJornada != nil && body.Ativacao.DadosJornada.Txid != nil {
		rec.journeyTx = *body.Ativacao.DadosJornada.Txid
	}
	if err := rec.readValue(body); err != nil {
		return nil, err
	}
	switch {
	case rec.contract == "" || len(rec.contract) > 35 || len(rec.object) > 35:
		return nil, invalid("vinculo.contrato é obrigatório, e ele e vinculo.objeto têm até 35 caracteres")
	case (rec.debtor.Cpf == nil) == (rec.debtor.Cnpj == nil) || rec.debtor.Nome == nil || *rec.debtor.Nome == "":
		return nil, invalid("vinculo.devedor deve ter nome e um CPF ou um CNPJ")
	case !slices.Contains(periods, rec.period):
		return nil, invalid("calendario.periodicidade inválida")
	case rec.retry != retriesNone && rec.retry != retries3R7D:
		return nil, invalid("politicaRetentativa inválida")
	case rec.start < dateOf(now).Format(time.DateOnly):
		return nil, invalid("calendario.dataInicial no passado")
	case rec.end != "" && rec.end < rec.start:
		return nil, invalid("calendario.dataFinal anterior à dataInicial")
	case rec.journeyTx != "":
		return nil, invalid("a jornada 3 não é simulada")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if body.Loc != nil {
		loc := s.recLocs[*body.Loc]
		if loc == nil || loc.rec != "" {
			return nil, invalid("loc %d inexistente ou já usada", *body.Loc)
		}
		rec.loc = loc
	}
	rec.id = s.recID(rec.retry, now)
	rec.set(recCreated, now)
	if rec.loc != nil {
		rec.loc.rec = rec.id
	}
	s.recs[rec.id] = rec
	return rec, nil
}

// readValue reads a recurrence's fixed amount, or the least its charges may be.
func (r *recurrence) readValue(body pixapi.RecSolicitada) error {
	v := body.Valor
	if v == nil {
		return nil
	}
	var err error
	if v.ValorRec != nil {
		if r.fixed, err = pixapi.ParseValor(*v.ValorRec); err != nil || r.fixed <= 0 {
			return invalid("valor.valorRec inválido")
		}
	}
	if v.ValorMinimoRecebedor != nil {
		if r.minimum, err = pixapi.ParseValor(*v.ValorMinimoRecebedor); err != nil {
			return invalid("valor.valorMinimoRecebedor inválido")
		}
	}
	return nil
}

// listRecurrences answers GET /rec: a client's recurrences created in a period, by the
// payer's CPF or CNPJ if asked.
func (s *Sim) listRecurrences(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	start, err1 := time.Parse(time.RFC3339, q.Get("inicio"))
	end, err2 := time.Parse(time.RFC3339, q.Get("fim"))
	if err1 != nil || err2 != nil {
		problemf(w, http.StatusBadRequest, "RequisicaoInvalida", "Consulta inválida.", "inicio e fim são obrigatórios (RFC 3339).")
		return
	}
	s.mu.Lock()
	var found []*recurrence
	for _, rec := range s.recs {
		taxID := q.Get("cpf") + q.Get("cnpj")
		if rec.client == clientOf(r) && !rec.created.Before(start) && rec.created.Before(end) && (taxID == "" || rec.payerTaxID() == taxID) {
			found = append(found, rec)
		}
	}
	slices.SortFunc(found, func(a, b *recurrence) int { return a.created.Compare(b.created) })
	recs := make([]pixapi.RecCompleta, 0, len(found))
	for _, rec := range found {
		recs = append(recs, s.renderRec(rec))
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"recs": recs})
}

// reviseRecurrence answers PATCH /rec/{idRec}: the receiver cancels it, or, before the
// payer authorized it, gives it a location (Anexo IV §4.1.1).
func (s *Sim) reviseRecurrence(w http.ResponseWriter, r *http.Request) {
	var body pixapi.RecRevisada
	if err := decodeBody(r, &body); err != nil {
		problemf(w, http.StatusBadRequest, "RecOperacaoInvalida", "Revisão inválida.", "%v", err)
		return
	}
	var cobrs []*recurringCharge
	s.mu.Lock()
	rec := s.recs[r.PathValue("idRec")]
	if rec == nil || rec.client != clientOf(r) {
		s.mu.Unlock()
		problemf(w, http.StatusNotFound, "NaoEncontrado", "Recorrência não encontrada.", "Nenhuma recorrência %s.", r.PathValue("idRec"))
		return
	}
	var problem string
	switch {
	case rec.status != recCreated && rec.status != recApproved:
		problem = "A recorrência está " + rec.status + " e não pode ser alterada."
	case body.Status != nil && *body.Status == "CANCELADA":
		cobrs = s.cancelRecurrenceLocked(rec, "USUARIO_RECEBEDOR", codeByReceiver, "Cancelada pelo usuário recebedor.")
	case body.Loc != nil && rec.status == recCreated:
		loc := s.recLocs[*body.Loc]
		if loc == nil || loc.rec != "" {
			problem = "loc inexistente ou já usada."
			break
		}
		loc.rec, rec.loc = rec.id, loc
	default:
		problem = "Somente o cancelamento, ou a location antes da aprovação, podem ser alterados."
	}
	var out pixapi.RecCompleta
	if problem == "" {
		out = s.renderRec(rec)
	}
	s.mu.Unlock()
	if problem != "" {
		problemf(w, http.StatusBadRequest, "RecOperacaoInvalida", "Revisão inválida.", "%s", problem)
		return
	}
	writeJSON(w, http.StatusOK, out)
	ctx := context.WithoutCancel(r.Context())
	s.notifyRec(ctx, rec)
	for _, c := range cobrs {
		s.notifyCobr(ctx, c)
	}
}

// cancelRecurrenceLocked cancels a recurrence, its pending requests, and the charges
// under it not yet due to be debited, and returns those charges.
func (s *Sim) cancelRecurrenceLocked(rec *recurrence, by, code, reason string) []*recurringCharge {
	now := s.now()
	rec.set(recCanceled, now)
	rec.ending = &pixapi.Encerramento{}
	cancel := pixapi.New(&rec.ending.Cancelamento)
	cancel.Solicitante = pixapi.Ptr(pixapi.EncerramentoCancelamentoSolicitante(by))
	cancel.Codigo, cancel.Descricao = pixapi.Ptr(code), pixapi.Ptr(reason)
	for _, q := range rec.requests {
		if q.status == recCreated || q.status == requestSent || q.status == requestSeen {
			q.set(recCanceled, now)
		}
	}
	var canceled []*recurringCharge
	today := dateOf(now).Format(time.DateOnly)
	for _, c := range s.cobrs {
		if c.rec == rec.id && c.cancelable(today) {
			code := codeByReceiver
			if by == "USUARIO_PAGADOR" {
				code = codeChargeByPayer
			}
			c.cancel(now, by, code, "A recorrência foi cancelada.")
			canceled = append(canceled, c)
		}
	}
	return canceled
}

func (s *Sim) createRecurrenceRequest(w http.ResponseWriter, r *http.Request) {
	var body pixapi.SolicRecBase
	if err := decodeBody(r, &body); err != nil {
		problemf(w, http.StatusBadRequest, "SolicRecOperacaoInvalida", "Solicitação inválida.", "%v", err)
		return
	}
	now := s.now()
	s.mu.Lock()
	rec := s.recs[body.IdRec]
	var problem string
	switch {
	case rec == nil || rec.client != clientOf(r):
		problem = "Recorrência " + body.IdRec + " não encontrada."
	case rec.status != recCreated:
		problem = "A recorrência está " + rec.status + "."
	case !body.Calendario.DataExpiracaoSolicitacao.After(now):
		problem = "calendario.dataExpiracaoSolicitacao deve ser futura."
	case (body.Destinatario.Cpf == nil) == (body.Destinatario.Cnpj == nil):
		problem = "destinatario deve ter um CPF ou um CNPJ."
	}
	if problem != "" {
		s.mu.Unlock()
		problemf(w, http.StatusBadRequest, "SolicRecOperacaoInvalida", "Solicitação inválida.", "%s", problem)
		return
	}
	q := &solicRec{
		id: "SC" + s.cfg.ISPB + now.In(brasilia).Format("20060102") + randomAlnum(11), rec: rec.id,
		expires: body.Calendario.DataExpiracaoSolicitacao, to: body.Destinatario,
	}
	q.set(recCreated, now)
	q.set(requestSent, now)
	if body.Destinatario.IspbParticipante != PayerISPB {
		// No payer's bank answers at that ISPB.
		q.set(recRejected, now)
		q.motive = "DADOS_BANCARIOS_INVALIDOS"
	} else {
		q.set(requestSeen, now)
	}
	rec.requests = append(rec.requests, q)
	s.solicRecs[q.id] = q
	out := s.renderRequest(q)
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, out)
}

func (s *Sim) cancelRecurrenceRequest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Status string `json:"status"`
	}
	if err := decodeBody(r, &body); err != nil || body.Status != recCanceled {
		problemf(w, http.StatusBadRequest, "SolicRecOperacaoInvalida", "Revisão inválida.", "Só o cancelamento é permitido.")
		return
	}
	s.mu.Lock()
	q := s.solicRecs[r.PathValue("id")]
	if q == nil || s.recs[q.rec].client != clientOf(r) || (q.status != requestSent && q.status != requestSeen) {
		s.mu.Unlock()
		problemf(w, http.StatusBadRequest, "SolicRecOperacaoInvalida", "Revisão inválida.", "Nenhuma solicitação pendente com esse id.")
		return
	}
	q.set(recCanceled, s.now())
	out := s.renderRequest(q)
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, out)
}

// DecideRequest is the payer answering a recurrence request in their bank's app: the
// recurrence is approved by journey 1, or rejected.
func (s *Sim) DecideRequest(ctx context.Context, id string, accept bool) error {
	now := s.now()
	s.mu.Lock()
	q := s.solicRecs[id]
	if q == nil || q.status != requestSeen {
		s.mu.Unlock()
		return errors.New("no request waiting for the payer with that id")
	}
	rec := s.recs[q.rec]
	if accept {
		q.set(requestTaken, now)
		rec.journey, rec.payerISPB = journeyRequest, q.to.IspbParticipante
		rec.set(recApproved, now)
	} else {
		q.set(recRejected, now)
		rec.set(recRejected, now)
		rec.ending = &pixapi.Encerramento{}
		rej := pixapi.New(&rec.ending.Rejeicao)
		rej.Codigo, rej.Descricao = pixapi.Ptr(codePayerRefused), pixapi.Ptr("Recusada pelo usuário pagador.")
	}
	s.mu.Unlock()
	s.notifyRec(ctx, rec)
	return nil
}

// PendingRequests lists the recurrence requests a payer's bank is showing them.
func (s *Sim) PendingRequests(payerTaxID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for _, q := range s.solicRecs {
		if q.status == requestSeen && s.recs[q.rec].payerTaxID() == payerTaxID {
			ids = append(ids, q.id)
		}
	}
	return ids
}

// ApprovedRecurrences lists the recurrences a payer has authorized.
func (s *Sim) ApprovedRecurrences(payerTaxID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for _, rec := range s.recs {
		if rec.status == recApproved && rec.payerTaxID() == payerTaxID {
			ids = append(ids, rec.id)
		}
	}
	return ids
}

// CancelAsPayer is the payer revoking their authorization, in their bank's app.
func (s *Sim) CancelAsPayer(ctx context.Context, idRec string) error {
	s.mu.Lock()
	rec := s.recs[idRec]
	if rec == nil || rec.status != recApproved {
		s.mu.Unlock()
		return errors.New("no approved recurrence with that id")
	}
	cobrs := s.cancelRecurrenceLocked(rec, "USUARIO_PAGADOR", codeByPayer, "Cancelada pelo usuário pagador.")
	s.mu.Unlock()
	s.notifyRec(ctx, rec)
	for _, c := range cobrs {
		s.notifyCobr(ctx, c)
	}
	return nil
}

// authorizeByQR is the payer approving a recurrence by reading its QR code (journey 2):
// the payer's bank fetches the signed payload, checks it names them, and confirms.
func (s *Sim) authorizeByQR(ctx context.Context, code brcode.Pix, p Payment) (PaymentResult, error) {
	body, err := s.fetchSigned(ctx, code.RecurrenceURL, "")
	if err != nil {
		return PaymentResult{}, err
	}
	var payload pixapi.RecPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return PaymentResult{}, refused("an unreadable recurrence")
	}
	now := s.now()
	s.mu.Lock()
	rec := s.recs[payload.IdRec]
	switch {
	case rec == nil || rec.status != recCreated:
		s.mu.Unlock()
		return PaymentResult{}, refused("the recurrence is not waiting for authorization")
	case rec.payerTaxID() != p.PayerTaxID:
		s.mu.Unlock()
		return PaymentResult{}, refused("the recurrence is for another payer")
	}
	rec.journey, rec.payerISPB = journeyQR, PayerISPB
	rec.set(recApproved, now)
	for _, q := range rec.requests {
		if q.status == requestSent || q.status == requestSeen {
			q.set(recCanceled, now) // confirmed by another journey (Anexo IV §4.1.2)
		}
	}
	s.mu.Unlock()
	s.notifyRec(ctx, rec)
	return PaymentResult{Authorized: rec.id}, nil
}

func (s *Sim) serveRecPayload(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	loc := s.recTokens[r.PathValue("token")]
	var rec *recurrence
	if loc != nil {
		rec = s.recs[loc.rec]
	}
	var payload pixapi.RecPayload
	gone := rec == nil || rec.status != recCreated
	if !gone {
		payload = s.recPayload(rec)
	}
	s.mu.Unlock()
	if gone {
		problemf(w, http.StatusGone, "RecPayloadNaoEncontrado", "Recorrência indisponível.", "Nenhuma recorrência a autorizar neste endereço.")
		return
	}
	jws, err := s.sign(payload)
	if err != nil {
		problemf(w, http.StatusInternalServerError, "ErroInternoDoServidor", "Erro interno.", "Falha ao assinar o payload.")
		return
	}
	w.Header().Set("Content-Type", "application/jose")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(jws))
}

func (s *Sim) recPayload(rec *recurrence) pixapi.RecPayload {
	out := pixapi.RecPayload{IdRec: rec.id, PoliticaRetentativa: pixapi.RecPayloadPoliticaRetentativa(rec.retry)}
	out.Vinculo.Contrato, out.Vinculo.Devedor = rec.contract, rec.debtor
	if rec.object != "" {
		out.Vinculo.Objeto = pixapi.Ptr(rec.object)
	}
	_ = out.Calendario.DataInicial.UnmarshalText([]byte(rec.start))
	out.Calendario.Periodicidade = pixapi.RecPayloadCalendarioPeriodicidade(rec.period)
	if rec.end != "" {
		_ = pixapi.New(&out.Calendario.DataFinal).UnmarshalText([]byte(rec.end))
	}
	out.Valor = s.recValue(rec)
	acct := s.clients[rec.client]
	out.Recebedor = pixapi.RecebedorRecorrencia{Cnpj: pixapi.Ptr(acct.TaxID), Nome: pixapi.Ptr(acct.Name), IspbParticipante: pixapi.Ptr(s.cfg.ISPB)}
	for _, h := range rec.history {
		u := pixapi.Push(&out.Atualizacao)
		u.Status, u.Data = pixapi.RecPayloadAtualizacaoStatus(h.status), h.at
	}
	return out
}

func (s *Sim) recValue(rec *recurrence) *struct {
	ValorMinimoRecebedor *string `json:"valorMinimoRecebedor,omitempty"`
	ValorRec             *string `json:"valorRec,omitempty"`
} {
	if rec.fixed == 0 && rec.minimum == 0 {
		return nil
	}
	v := &struct {
		ValorMinimoRecebedor *string `json:"valorMinimoRecebedor,omitempty"`
		ValorRec             *string `json:"valorRec,omitempty"`
	}{}
	if rec.fixed > 0 {
		v.ValorRec = pixapi.Ptr(pixapi.FormatValor(rec.fixed))
	}
	if rec.minimum > 0 {
		v.ValorMinimoRecebedor = pixapi.Ptr(pixapi.FormatValor(rec.minimum))
	}
	return v
}

// recQR is the composite BR Code of a recurrence alone (journey 2, manual §2.8.1).
func (s *Sim) recQR(rec *recurrence) string {
	name := s.clients[rec.client].Name
	if len([]rune(name)) > 25 {
		name = string([]rune(name)[:25])
	}
	code, err := brcode.Pix{RecurrenceURL: s.recLocationURL(rec.loc), MerchantName: name, MerchantCity: merchantCity}.Encode()
	if err != nil {
		s.cfg.Logger.Error("encoding a recurrence's BR Code", "idRec", rec.id, "error", err)
	}
	return code
}

func (s *Sim) renderRec(rec *recurrence) pixapi.RecCompleta {
	out := pixapi.RecCompleta{
		IdRec: rec.id, Status: pixapi.RecCompletaStatus(rec.status), PoliticaRetentativa: pixapi.RecCompletaPoliticaRetentativa(rec.retry),
		Encerramento: rec.ending, Valor: s.recValue(rec),
	}
	out.Vinculo.Contrato, out.Vinculo.Devedor = rec.contract, rec.debtor
	if rec.object != "" {
		out.Vinculo.Objeto = pixapi.Ptr(rec.object)
	}
	_ = out.Calendario.DataInicial.UnmarshalText([]byte(rec.start))
	out.Calendario.Periodicidade = pixapi.RecCompletaCalendarioPeriodicidade(rec.period)
	if rec.end != "" {
		_ = pixapi.New(&out.Calendario.DataFinal).UnmarshalText([]byte(rec.end))
	}
	acct := s.clients[rec.client]
	out.Recebedor = pixapi.RecebedorRecorrencia{Cnpj: pixapi.Ptr(acct.TaxID), Nome: pixapi.Ptr(acct.Name)}
	for _, h := range rec.history {
		u := pixapi.Push(&out.Atualizacao)
		u.Status, u.Data = pixapi.RecCompletaAtualizacaoStatus(h.status), h.at
	}
	act := pixapi.New(&out.Ativacao)
	act.TipoJornada = pixapi.RecCompletaAtivacaoTipoJornada(rec.journey)
	if rec.payerISPB != "" {
		pixapi.New(&out.Pagador).IspbParticipante = pixapi.Ptr(rec.payerISPB)
	}
	if rec.loc != nil {
		loc := pixapi.PayloadLocationRecCompleta{Id: rec.loc.id, IdRec: pixapi.Ptr(rec.id), Location: pixapi.Ptr(s.recLocationURL(rec.loc)), Criacao: pixapi.Ptr(rec.loc.created)}
		out.Loc = &loc
		if rec.status == recCreated {
			qr := pixapi.New(&out.DadosQR)
			qr.Jornada, qr.PixCopiaECola = pixapi.Ptr(pixapi.RecCompletaDadosQRJornada(journeyQR)), pixapi.Ptr(s.recQR(rec))
		}
	}
	for _, q := range rec.requests {
		pixapi.Append(&out.Solicitacao)
		(*out.Solicitacao)[len(*out.Solicitacao)-1] = s.renderRequest(q)
	}
	return out
}

func (s *Sim) renderRequest(q *solicRec) pixapi.SolicRecCompleta {
	out := pixapi.SolicRecCompleta{IdSolicRec: q.id, IdRec: q.rec, Status: pixapi.SolicRecCompletaStatus(q.status), Destinatario: q.to}
	out.Calendario.DataExpiracaoSolicitacao = q.expires
	for _, h := range q.history {
		u := pixapi.Push(&out.Atualizacao)
		u.Status, u.Data = pixapi.SolicRecCompletaAtualizacaoStatus(h.status), h.at
	}
	if q.motive != "" {
		enc := pixapi.New(&out.Encerramento)
		pixapi.New(&enc.Rejeicao).Motivo = pixapi.SolicRecCompletaEncerramentoRejeicaoMotivo(q.motive)
	}
	if rec := s.recs[q.rec]; rec != nil {
		payload := s.recPayload(rec)
		out.RecPayload = &payload
	}
	return out
}

func (s *Sim) notifyRec(ctx context.Context, rec *recurrence) {
	s.mu.Lock()
	base := s.clients[rec.client].recWebhook
	full := s.renderRec(rec)
	s.mu.Unlock()
	if base == "" {
		return
	}
	n := pixapi.RecNotification{IdRec: full.IdRec, Status: pixapi.RecNotificationStatus(full.Status), Encerramento: full.Encerramento}
	for _, u := range full.Atualizacao {
		h := pixapi.Push(&n.Atualizacao)
		h.Status, h.Data = pixapi.RecNotificationAtualizacaoStatus(u.Status), u.Data
	}
	if full.Ativacao != nil {
		pixapi.New(&n.Ativacao).TipoJornada = pixapi.RecNotificationAtivacaoTipoJornada(full.Ativacao.TipoJornada)
	}
	s.post(ctx, Event{Kind: "webhook", Client: rec.client, ID: rec.id}, base+"/rec", rec.id, pixapi.WebhookRecBody{Recs: &[]pixapi.RecNotification{n}})
}

func httpsURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil
}
