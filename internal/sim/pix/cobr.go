package pix

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/iricardofernandes/jupiter/pkg/pixapi"
)

// Recurring charges (cobr), sent under an approved recurrence and debited from the payer
// on their date without the payer doing anything (Manual de Padrões, Anexo IV §2.3, §3.3,
// §4.3). A charge has two levels of status: its own, and each attempt's (tentativa): the
// first (AGND), retries after the due date (NTAG). The bank moves them as days pass, in
// Tick.

const (
	cobrCreated   = "CRIADA"
	cobrActive    = "ATIVA"
	cobrCompleted = "CONCLUIDA"
	cobrExpired   = "EXPIRADA"
	cobrRejected  = "REJEITADA"
	cobrCanceled  = "CANCELADA"

	attemptRequested = "SOLICITADA"
	attemptScheduled = "AGENDADA"
	attemptPaid      = "PAGA"
	attemptCanceled  = "CANCELADA"
	attemptRejected  = "REJEITADA"
	attemptExpired   = "EXPIRADA"

	kindFirst = "AGND"
	kindRetry = "NTAG"

	// A charge reaches the payer's bank from 10 days before its date, and must be made
	// at least 2 days before: the window the BCB's Pix Automático FAQ gives, as the
	// research summarized it.
	scheduleAhead = 10
	minimumAhead  = 2

	// Rejections the payer's bank gives, from the specification's codes: the ISO 20022
	// meanings of AC05 (closed debtor account) and AM09 (wrong amount).
	codeClosedAccount = "AC05"
	codeWrongAmount   = "AM09"
)

type recurringCharge struct {
	client   string
	txid     string
	rec      string
	due      string
	amount   int64
	info     string
	account  pixapi.DadosBancariosRecebedor
	adjust   bool
	policy   string
	status   string
	history  []statusAt
	created  time.Time
	attempts []*chargeAttempt
	ending   *pixapi.Encerramento
	pix      []string
}

type chargeAttempt struct {
	date      string
	kind      string
	status    string
	e2eid     string
	history   []statusAt
	rejection string
}

func (c *recurringCharge) set(status string, at time.Time) {
	c.status = status
	c.history = append(c.history, statusAt{status, at})
}

func (a *chargeAttempt) set(status string, at time.Time) {
	a.status = status
	a.history = append(a.history, statusAt{status, at})
}

func (c *recurringCharge) pending() *chargeAttempt {
	for _, a := range c.attempts {
		if a.status == attemptRequested || a.status == attemptScheduled {
			return a
		}
	}
	return nil
}

// firstDate is when the charge is first to be debited: its due date, moved past a
// weekend when the receiver asked for business days. The simulator knows no holidays.
func (c *recurringCharge) firstDate() string {
	d, _ := time.Parse(time.DateOnly, c.due)
	if c.adjust {
		for d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
			d = d.AddDate(0, 0, 1)
		}
	}
	return d.Format(time.DateOnly)
}

// cancelable: the receiver may cancel a charge until the day before its first debit
// (Anexo IV §4.3.2).
func (c *recurringCharge) cancelable(today string) bool {
	return (c.status == cobrCreated || c.status == cobrActive) && today < c.firstDate()
}

func (c *recurringCharge) cancel(at time.Time, by, code, reason string) {
	if a := c.pending(); a != nil {
		a.set(attemptCanceled, at)
	}
	c.set(cobrCanceled, at)
	c.ending = &pixapi.Encerramento{}
	cancel := pixapi.New(&c.ending.Cancelamento)
	cancel.Solicitante = pixapi.Ptr(pixapi.EncerramentoCancelamentoSolicitante(by))
	cancel.Codigo, cancel.Descricao = pixapi.Ptr(code), pixapi.Ptr(reason)
}

func (s *Sim) recurringChargeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("PUT /cobr/{txid}", s.authorized(pixapi.ScopeCobRWrite, func(w http.ResponseWriter, r *http.Request) {
		s.createRecurringCharge(w, r, r.PathValue("txid"))
	}))
	mux.HandleFunc("POST /cobr", s.authorized(pixapi.ScopeCobRWrite, func(w http.ResponseWriter, r *http.Request) {
		s.createRecurringCharge(w, r, randomAlnum(32))
	}))
	mux.HandleFunc("GET /cobr/{txid}", s.authorized(pixapi.ScopeCobRRead, func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		c := s.cobrs[clientOf(r)+"/"+r.PathValue("txid")]
		var out pixapi.CobRCompleta
		if c != nil {
			out = s.renderCobr(c)
		}
		s.mu.Unlock()
		if c == nil {
			problemf(w, http.StatusNotFound, "NaoEncontrado", "Cobrança recorrente não encontrada.", "Nenhuma cobrança recorrente %s.", r.PathValue("txid"))
			return
		}
		writeJSON(w, http.StatusOK, out)
	}))
	mux.HandleFunc("PATCH /cobr/{txid}", s.authorized(pixapi.ScopeCobRWrite, s.cancelRecurringCharge))
	mux.HandleFunc("POST /cobr/{txid}/retentativa/{data}", s.authorized(pixapi.ScopeCobRWrite, s.retryRecurringCharge))
}

func (s *Sim) createRecurringCharge(w http.ResponseWriter, r *http.Request, txid string) {
	var body pixapi.CobRSolicitada
	if err := decodeBody(r, &body); err != nil {
		problemf(w, http.StatusBadRequest, "CobROperacaoInvalida", "Cobrança recorrente inválida.", "%v", err)
		return
	}
	c, err := s.newRecurringCharge(clientOf(r), txid, body)
	if err != nil {
		problemf(w, http.StatusBadRequest, "CobROperacaoInvalida", "Cobrança recorrente inválida.", "%v", err)
		return
	}
	s.mu.Lock()
	out := s.renderCobr(c)
	s.mu.Unlock()
	if s.fault(Event{Kind: "charge", Client: c.client, TxID: c.txid}).LoseResponse {
		panic(http.ErrAbortHandler)
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Sim) newRecurringCharge(client, txid string, body pixapi.CobRSolicitada) (*recurringCharge, error) {
	now := s.now()
	today := dateOf(now)
	c := &recurringCharge{
		client: client, txid: txid, rec: body.IdRec, due: body.Calendario.DataDeVencimento.String(), account: body.Recebedor,
		adjust: body.AjusteDiaUtil, created: now,
	}
	if body.InfoAdicional != nil {
		c.info = *body.InfoAdicional
	}
	var err error
	if c.amount, err = pixapi.ParseValor(body.Valor.Original); err != nil || c.amount <= 0 {
		return nil, invalid("valor.original deve ser positivo")
	}
	due, err := time.ParseInLocation(time.DateOnly, c.due, brasilia)
	if err != nil {
		return nil, invalid("calendario.dataDeVencimento inválida")
	}
	if !pixapi.ValidTxID(txid) {
		return nil, invalid("txid deve ter de 26 a 35 letras e dígitos")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkRecurringChargeLocked(c, due, today); err != nil {
		return nil, err
	}
	c.policy = s.recs[c.rec].retry
	c.set(cobrCreated, now)
	s.cobrs[client+"/"+txid] = c
	return c, nil
}

// checkRecurringChargeLocked validates a charge against its recurrence (Anexo IV §2.3.3,
// §4.3.1): the receiver's own account, the recurrence's amount and validity, the minimum
// notice, and one charge per cycle.
func (s *Sim) checkRecurringChargeLocked(c *recurringCharge, due, today time.Time) error {
	rec := s.recs[c.rec]
	acct := s.clients[c.client]
	switch {
	case rec == nil || rec.client != c.client:
		return invalid("recorrência %s não encontrada", c.rec)
	case rec.status != recApproved:
		return invalid("a recorrência está %s", rec.status)
	case c.account.Conta != acct.Account || (c.account.Agencia != nil && *c.account.Agencia != acct.Branch):
		return invalid("o recebedor não é titular da conta informada")
	case rec.fixed > 0 && c.amount != rec.fixed:
		return invalid("a recorrência é de valor fixo %s", pixapi.FormatValor(rec.fixed))
	case rec.minimum > 0 && c.amount < rec.minimum:
		return invalid("valor abaixo do mínimo da recorrência")
	case due.Before(today.AddDate(0, 0, minimumAhead)):
		return invalid("a data de vencimento deve ser de pelo menos %d dias a partir de hoje", minimumAhead)
	case c.due < rec.start || (rec.end != "" && c.due > rec.end):
		return invalid("a data de vencimento está fora da vigência da recorrência")
	case s.cobrs[c.client+"/"+c.txid] != nil || s.charges[c.client+"/"+c.txid] != nil:
		return invalid("txid %s já utilizado", c.txid)
	}
	cycle := cycleOf(rec, c.due)
	for _, other := range s.cobrs {
		if other.rec == rec.id && other.status != cobrCanceled && other.status != cobrRejected && cycleOf(rec, other.due) == cycle {
			return invalid("a recorrência já tem uma cobrança neste ciclo")
		}
	}
	return nil
}

// cycleOf numbers the cycle of a recurrence a date falls in. Cycles start on the first
// date and repeat by the period; a date a month lacks becomes the last day it has
// (Anexo IV §4.3.1).
func cycleOf(rec *recurrence, date string) int {
	start, _ := time.Parse(time.DateOnly, rec.start)
	d, _ := time.Parse(time.DateOnly, date)
	n := 0
	for !addPeriods(start, rec.period, n+1).After(d) {
		n++
	}
	return n
}

// addPeriods is the start of cycle n.
func addPeriods(start time.Time, period string, n int) time.Time {
	months := map[string]int{"MENSAL": 1, "TRIMESTRAL": 3, "SEMESTRAL": 6, "ANUAL": 12}[period]
	if months == 0 {
		return start.AddDate(0, 0, 7*n)
	}
	y, m := start.Year(), int(start.Month())-1+months*n
	y, m = y+m/12, m%12
	first := time.Date(y, time.Month(m+1), 1, 0, 0, 0, 0, time.UTC)
	last := first.AddDate(0, 1, -1).Day()
	return time.Date(y, time.Month(m+1), min(start.Day(), last), 0, 0, 0, 0, time.UTC)
}

func (s *Sim) cancelRecurringCharge(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Status string `json:"status"`
	}
	if err := decodeBody(r, &body); err != nil || body.Status != cobrCanceled {
		problemf(w, http.StatusBadRequest, "CobROperacaoInvalida", "Revisão inválida.", "Só o cancelamento é permitido.")
		return
	}
	now := s.now()
	s.mu.Lock()
	c := s.cobrs[clientOf(r)+"/"+r.PathValue("txid")]
	switch {
	case c == nil:
		s.mu.Unlock()
		problemf(w, http.StatusNotFound, "NaoEncontrado", "Cobrança recorrente não encontrada.", "Nenhuma cobrança recorrente %s.", r.PathValue("txid"))
		return
	case !c.cancelable(dateOf(now).Format(time.DateOnly)):
		s.mu.Unlock()
		problemf(w, http.StatusBadRequest, "CobROperacaoInvalida", "Revisão inválida.",
			"A cobrança está %s ou já passou da véspera da data prevista para liquidação.", c.status)
		return
	}
	c.cancel(now, "USUARIO_RECEBEDOR", codeByReceiver, "Cancelada pelo usuário recebedor.")
	out := s.renderCobr(c)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
	s.notifyCobr(context.WithoutCancel(r.Context()), c)
}

// retryRecurringCharge answers POST /cobr/{txid}/retentativa/{data}: under a recurrence
// that allows them, up to three retries on different days within seven calendar days of
// the first date, each asked for before the day it is for (Anexo IV §4.1.4).
func (s *Sim) retryRecurringCharge(w http.ResponseWriter, r *http.Request) {
	date := r.PathValue("data")
	now := s.now()
	today := dateOf(now).Format(time.DateOnly)
	s.mu.Lock()
	c := s.cobrs[clientOf(r)+"/"+r.PathValue("txid")]
	if c == nil {
		s.mu.Unlock()
		problemf(w, http.StatusNotFound, "NaoEncontrado", "Cobrança recorrente não encontrada.", "Nenhuma cobrança recorrente %s.", r.PathValue("txid"))
		return
	}
	if problem := c.retryProblem(date, today); problem != "" {
		s.mu.Unlock()
		problemf(w, http.StatusBadRequest, "RequisicaoInvalidaCobRTentativa", "Retentativa inválida.", "%s", problem)
		return
	}
	a := &chargeAttempt{date: date, kind: kindRetry, e2eid: endToEndID("E", s.cfg.ISPB, now)}
	a.set(attemptRequested, now)
	a.set(attemptScheduled, now)
	c.attempts = append(c.attempts, a)
	out := s.renderCobr(c)
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, out)
	s.notifyCobr(context.WithoutCancel(r.Context()), c)
}

// retryProblem says why a retry on date cannot be made, or "" when it can.
func (c *recurringCharge) retryProblem(date, today string) string {
	first, _ := time.Parse(time.DateOnly, c.firstDate())
	retries := 0
	usedDate := false
	for _, a := range c.attempts {
		if a.kind == kindRetry {
			retries++
		}
		usedDate = usedDate || a.date == date
	}
	d, err := time.Parse(time.DateOnly, date)
	switch {
	case err != nil:
		return "data inválida"
	case c.policy != retries3R7D:
		return "a recorrência não permite retentativas"
	case c.status != cobrActive || c.pending() != nil:
		return "a cobrança não aguarda uma retentativa"
	case retries >= maxRetries:
		return "as três retentativas já foram usadas"
	case !d.After(first) || d.After(first.AddDate(0, 0, retryWindow)):
		return "a retentativa deve ser em até sete dias após a data prevista para liquidação"
	case date <= today:
		return "a retentativa deve ser pedida até a véspera"
	case usedDate:
		return "já houve uma tentativa nessa data"
	}
	return ""
}

// Tick moves Pix Automático as the bank does each day: requests and recurrences past
// their dates expire; charges within the window are sent to the payer's bank and
// scheduled; scheduled attempts are debited on their date if the payer has the money,
// and expire once it passes; a charge with no attempts left expires. It notifies the
// receiver of every change.
func (s *Sim) Tick(ctx context.Context) {
	now := s.now()
	today := dateOf(now).Format(time.DateOnly)
	var recs []*recurrence
	var cobrs []*recurringCharge
	s.mu.Lock()
	for _, q := range s.solicRecs {
		if (q.status == requestSent || q.status == requestSeen) && now.After(q.expires) {
			q.set(recExpired, now)
		}
	}
	for _, rec := range s.recs {
		if (rec.status == recCreated || rec.status == recApproved) && rec.end != "" && today > rec.end {
			rec.set(recExpired, now)
			recs = append(recs, rec)
		}
	}
	for _, c := range s.cobrs {
		if s.advanceLocked(c, now, today) {
			cobrs = append(cobrs, c)
		}
	}
	s.mu.Unlock()
	for _, rec := range recs {
		s.notifyRec(ctx, rec)
	}
	for _, c := range cobrs {
		s.notifyCobr(ctx, c)
	}
}

// advanceLocked moves one charge, and reports whether it changed.
func (s *Sim) advanceLocked(c *recurringCharge, now time.Time, today string) bool {
	if c.status != cobrCreated && c.status != cobrActive {
		return false
	}
	rec := s.recs[c.rec]
	payer := rec.payerTaxID()
	first, _ := time.Parse(time.DateOnly, c.firstDate())
	if c.status == cobrCreated {
		if today < first.AddDate(0, 0, -scheduleAhead).Format(time.DateOnly) {
			return false
		}
		s.scheduleLocked(c, rec, payer, now)
		return true
	}
	a := c.pending()
	switch {
	case a != nil && a.date == today:
		return s.debitLocked(c, a, payer, now)
	case a != nil && a.date < today:
		a.set(attemptExpired, now)
		s.expireIfDoneLocked(c, first, today, now)
		return true
	case a == nil:
		return s.expireIfDoneLocked(c, first, today, now)
	}
	return false
}

// scheduleLocked sends a charge to the payer's bank, which schedules the debit or
// rejects the charge.
func (s *Sim) scheduleLocked(c *recurringCharge, rec *recurrence, payer string, now time.Time) {
	a := &chargeAttempt{date: c.firstDate(), kind: kindFirst, e2eid: endToEndID("E", s.cfg.ISPB, now)}
	a.set(attemptRequested, now)
	c.attempts = append(c.attempts, a)
	c.set(cobrActive, now)
	var code string
	switch {
	case s.closedPayers[payer]:
		code = codeClosedAccount
	case rec.fixed > 0 && c.amount != rec.fixed:
		code = codeWrongAmount
	}
	if code != "" {
		a.rejection = code
		a.set(attemptRejected, now)
		c.set(cobrRejected, now)
		c.ending = &pixapi.Encerramento{}
		rej := pixapi.New(&c.ending.Rejeicao)
		rej.Codigo, rej.Descricao = pixapi.Ptr(code), pixapi.Ptr("Rejeitada pelo PSP pagador.")
		return
	}
	a.set(attemptScheduled, now)
}

// debitLocked is the payer's bank debiting a scheduled attempt on its date. Without the
// money it tries again later the same day; the attempt expires once the day is over.
func (s *Sim) debitLocked(c *recurringCharge, a *chargeAttempt, payer string, now time.Time) bool {
	funds, limited := s.payerFunds[payer]
	if limited && funds < c.amount {
		return false
	}
	if limited {
		s.payerFunds[payer] = funds - c.amount
	}
	p := &received{client: c.client, e2eid: a.e2eid, txid: c.txid, amount: c.amount, at: now, payerTaxID: payer}
	s.pix[p.e2eid] = p
	s.clients[c.client].balance += c.amount
	a.set(attemptPaid, now)
	c.pix = append(c.pix, p.e2eid)
	c.set(cobrCompleted, now)
	return true
}

// expireIfDoneLocked expires an active charge with no attempt left to make: none are
// allowed, or the three retries or the seven days are used up.
func (s *Sim) expireIfDoneLocked(c *recurringCharge, first time.Time, today string, now time.Time) bool {
	retries := 0
	for _, a := range c.attempts {
		if a.kind == kindRetry {
			retries++
		}
	}
	lastDay := first.AddDate(0, 0, retryWindow).Format(time.DateOnly)
	if c.policy == retries3R7D && retries < maxRetries && today < lastDay {
		return false
	}
	c.set(cobrExpired, now)
	return true
}

func (s *Sim) renderCobr(c *recurringCharge) pixapi.CobRCompleta {
	out := pixapi.CobRCompleta{
		IdRec: c.rec, Txid: c.txid, Status: pixapi.CobRCompletaStatus(c.status), Recebedor: c.account, AjusteDiaUtil: c.adjust,
		PoliticaRetentativa: pixapi.CobRCompletaPoliticaRetentativa(c.policy), Encerramento: c.ending,
	}
	out.Valor.Original = pixapi.FormatValor(c.amount)
	_ = out.Calendario.Criacao.UnmarshalText([]byte(dateOf(c.created).Format(time.DateOnly)))
	if c.info != "" {
		out.InfoAdicional = pixapi.Ptr(c.info)
	}
	for _, h := range c.history {
		u := pixapi.Push(&out.Atualizacao)
		u.Status, u.Data = pixapi.CobRCompletaAtualizacaoStatus(h.status), h.at
	}
	for _, a := range c.attempts {
		t := pixapi.Append(&out.Tentativas)
		_ = t.DataLiquidacao.UnmarshalText([]byte(a.date))
		t.Tipo, t.Status, t.EndToEndId = pixapi.CobRCompletaTentativasTipo(a.kind), pixapi.CobRCompletaTentativasStatus(a.status), a.e2eid
		for _, h := range a.history {
			u := pixapi.Push(&t.Atualizacao)
			u.Status, u.Data = pixapi.CobRCompletaTentativasAtualizacaoStatus(h.status), h.at
		}
		if a.rejection != "" {
			rej := pixapi.New(&t.Rejeicao)
			rej.Codigo, rej.Descricao = pixapi.CobRCompletaTentativasRejeicaoCodigo(a.rejection), "Rejeitada pelo PSP pagador."
		}
	}
	for _, e2e := range c.pix {
		p := s.pix[e2e]
		item := pixapi.Append(&out.Pix)
		item.EndToEndId, item.Txid, item.Valor, item.Horario = p.e2eid, p.txid, pixapi.FormatValor(p.amount), p.at
	}
	return out
}

// notifyCobr tells the receiver how a charge stands, at its cobr webhook.
func (s *Sim) notifyCobr(ctx context.Context, c *recurringCharge) {
	s.mu.Lock()
	base := s.clients[c.client].cobrWebhook
	full := s.renderCobr(c)
	s.mu.Unlock()
	if base == "" {
		return
	}
	// The notification carries the same fields as the charge.
	raw, _ := json.Marshal(full)
	var n pixapi.CobRNotification
	_ = json.Unmarshal(raw, &n)
	s.post(ctx, Event{Kind: "webhook", Client: c.client, TxID: c.txid}, base+"/cobr", c.txid, pixapi.WebhookCobRBody{Cobsr: &[]pixapi.CobRNotification{n}})
}

// SetPayerFunds limits what a payer has, in centavos, for recurring debits; a negative
// amount lifts the limit.
func (s *Sim) SetPayerFunds(taxID string, amount int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if amount < 0 {
		delete(s.payerFunds, taxID)
		return
	}
	s.payerFunds[taxID] = amount
}

// RecurringCharges lists the txids of the charges made under a recurrence.
func (s *Sim) RecurringCharges(idRec string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, c := range s.cobrs {
		if c.rec == idRec {
			out = append(out, c.txid)
		}
	}
	return out
}
