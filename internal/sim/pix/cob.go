package pix

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/iricardofernandes/jupiter/pkg/brcode"
	"github.com/iricardofernandes/jupiter/pkg/pixapi"
)

// Charges: immediate (cob) and with a due date (cobv), as the API Pix defines them. A
// charge's status is its record's (ATIVA, CONCLUIDA, REMOVIDA_*); expiry is never a
// status, it is derived from the calendar: creation plus expiracao for a cob, the due
// date plus validadeAposVencimento days for a cobv.

const (
	statusActive    = "ATIVA"
	statusCompleted = "CONCLUIDA"
	statusRemoved   = "REMOVIDA_PELO_USUARIO_RECEBEDOR"

	defaultExpiry   = 86400
	defaultValidity = 30
	maxBody         = 64 << 10
	merchantCity    = "SAO PAULO"
)

type charge struct {
	client     string
	txid       string
	due        bool
	revisao    int32
	status     string
	created    time.Time
	expiracao  int32
	dueDate    string // yyyy-mm-dd, in Brasília
	validity   int32
	key        string
	original   int64
	changeable bool
	request    string
	debtor     *pixapi.Pessoa
	fine       *rule
	interest   *rule
	abatement  *rule
	discount   *discount
	loc        int64
	token      string
	locCreated time.Time
	e2eids     []string
}

type rule struct {
	mode  int32
	value int64 // centavos, or hundredths of a percent
}

type discount struct {
	mode  int32
	value int64
	dates []datedValue
}

type datedValue struct {
	date  string
	value int64
}

func (s *Sim) chargeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("PUT /cob/{txid}", s.authorized(pixapi.ScopeCobWrite, func(w http.ResponseWriter, r *http.Request) {
		s.createCob(w, r, r.PathValue("txid"))
	}))
	mux.HandleFunc("POST /cob", s.authorized(pixapi.ScopeCobWrite, func(w http.ResponseWriter, r *http.Request) {
		s.createCob(w, r, randomAlnum(32))
	}))
	mux.HandleFunc("GET /cob/{txid}", s.authorized(pixapi.ScopeCobRead, func(w http.ResponseWriter, r *http.Request) {
		s.getCharge(w, r, false)
	}))
	mux.HandleFunc("PATCH /cob/{txid}", s.authorized(pixapi.ScopeCobWrite, func(w http.ResponseWriter, r *http.Request) {
		s.reviseCharge(w, r, false)
	}))
	mux.HandleFunc("PUT /cobv/{txid}", s.authorized(pixapi.ScopeCobVWrite, s.createCobV))
	mux.HandleFunc("GET /cobv/{txid}", s.authorized(pixapi.ScopeCobVRead, func(w http.ResponseWriter, r *http.Request) {
		s.getCharge(w, r, true)
	}))
	mux.HandleFunc("PATCH /cobv/{txid}", s.authorized(pixapi.ScopeCobVWrite, func(w http.ResponseWriter, r *http.Request) {
		s.reviseCharge(w, r, true)
	}))
}

// invalidError carries the violation a problem reports.
type invalidError struct{ reason string }

func (e invalidError) Error() string { return e.reason }

func invalid(format string, args ...any) error { return invalidError{fmt.Sprintf(format, args...)} }

func decodeBody(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	if err := dec.Decode(v); err != nil {
		return invalid("corpo da requisição inválido: %v", err)
	}
	return nil
}

func (s *Sim) createCob(w http.ResponseWriter, r *http.Request, txid string) {
	var body pixapi.CobSolicitada
	if err := decodeBody(r, &body); err != nil {
		problemf(w, http.StatusBadRequest, "CobOperacaoInvalida", "Cobrança inválida.", "%v", err)
		return
	}
	ch, err := s.newCob(clientOf(r), txid, body)
	if err != nil {
		problemf(w, http.StatusBadRequest, "CobOperacaoInvalida", "Cobrança inválida.", "%v", err)
		return
	}
	s.answerCharge(w, ch)
}

// answerCharge answers a charge just created, unless a fault loses the answer.
func (s *Sim) answerCharge(w http.ResponseWriter, ch *charge) {
	if s.fault(Event{Kind: "charge", Client: ch.client, TxID: ch.txid}).LoseResponse {
		panic(http.ErrAbortHandler)
	}
	writeJSON(w, http.StatusCreated, s.render(ch))
}

func (s *Sim) newCob(client, txid string, body pixapi.CobSolicitada) (*charge, error) {
	ch := &charge{client: client, txid: txid, key: body.Chave, expiracao: defaultExpiry}
	if body.Calendario.Expiracao != nil {
		ch.expiracao = *body.Calendario.Expiracao
	}
	var err error
	if ch.original, err = pixapi.ParseValor(body.Valor.Original); err != nil {
		return nil, invalid("valor.original: %v", err)
	}
	ch.changeable = body.Valor.ModalidadeAlteracao != nil && *body.Valor.ModalidadeAlteracao == 1
	if body.SolicitacaoPagador != nil {
		ch.request = *body.SolicitacaoPagador
	}
	if body.Devedor != nil {
		var debtor pixapi.Pessoa
		raw, _ := json.Marshal(body.Devedor)
		if json.Unmarshal(raw, &debtor) == nil {
			ch.debtor = &debtor
		}
	}
	switch {
	case body.Valor.Retirada != nil:
		return nil, invalid("Pix Saque e Pix Troco não são simulados")
	case ch.expiracao <= 0:
		return nil, invalid("calendario.expiracao deve ser positivo")
	case ch.original <= 0 && !ch.changeable:
		return nil, invalid("valor.original deve ser positivo")
	case len([]rune(ch.request)) > 140:
		return nil, invalid("solicitacaoPagador tem mais de 140 caracteres")
	}
	return ch, s.register(ch)
}

func (s *Sim) createCobV(w http.ResponseWriter, r *http.Request) {
	var body pixapi.CobVSolicitada
	if err := decodeBody(r, &body); err != nil {
		problemf(w, http.StatusBadRequest, "CobVOperacaoInvalida", "Cobrança inválida.", "%v", err)
		return
	}
	ch, err := s.newCobV(clientOf(r), r.PathValue("txid"), body)
	if err != nil {
		problemf(w, http.StatusBadRequest, "CobVOperacaoInvalida", "Cobrança inválida.", "%v", err)
		return
	}
	s.answerCharge(w, ch)
}

func (s *Sim) newCobV(client, txid string, body pixapi.CobVSolicitada) (*charge, error) {
	ch := &charge{
		client: client, txid: txid, due: true, key: body.Chave, dueDate: body.Calendario.DataDeVencimento.String(),
		validity: defaultValidity, debtor: &body.Devedor,
	}
	if body.Calendario.ValidadeAposVencimento != nil {
		ch.validity = *body.Calendario.ValidadeAposVencimento
	}
	if body.SolicitacaoPagador != nil {
		ch.request = *body.SolicitacaoPagador
	}
	var err error
	if ch.original, err = pixapi.ParseValor(body.Valor.Original); err != nil || ch.original <= 0 {
		return nil, invalid("valor.original deve ser um valor positivo")
	}
	if err := ch.valueRules(body); err != nil {
		return nil, err
	}
	due, err := time.ParseInLocation(time.DateOnly, ch.dueDate, brasilia)
	switch {
	case err != nil:
		return nil, invalid("calendario.dataDeVencimento inválida")
	case due.Before(dateOf(s.now())):
		return nil, invalid("calendario.dataDeVencimento anterior à data de criação")
	case ch.validity < 0:
		return nil, invalid("calendario.validadeAposVencimento negativa")
	case (ch.debtor.Cpf == nil) == (ch.debtor.Cnpj == nil) || ch.debtor.Nome == nil || *ch.debtor.Nome == "":
		return nil, invalid("devedor deve ter nome e um CPF ou um CNPJ")
	}
	return ch, s.register(ch)
}

// valueRules reads a cobv's fine, interest, rebate and discount.
func (ch *charge) valueRules(body pixapi.CobVSolicitada) error {
	v := body.Valor
	var err error
	if v.Multa != nil {
		if ch.fine, err = newRule(v.Multa.Modalidade, v.Multa.ValorPerc, 1, 2); err != nil {
			return invalid("valor.multa: %v", err)
		}
	}
	if v.Juros != nil {
		// Modalities 5 to 8 count business days, which the simulator has no calendar for.
		if ch.interest, err = newRule(v.Juros.Modalidade, v.Juros.ValorPerc, 1, 4); err != nil {
			return invalid("valor.juros: %v", err)
		}
	}
	if v.Abatimento != nil {
		if ch.abatement, err = newRule(v.Abatimento.Modalidade, v.Abatimento.ValorPerc, 1, 2); err != nil {
			return invalid("valor.abatimento: %v", err)
		}
	}
	if v.Desconto == nil {
		return nil
	}
	var dates []datedValue
	if v.Desconto.DescontoDataFixa != nil {
		for _, dv := range *v.Desconto.DescontoDataFixa {
			value, err := pixapi.ParseValor(dv.ValorPerc)
			if err != nil {
				return invalid("valor.desconto: %v", err)
			}
			dates = append(dates, datedValue{date: dv.Data.String(), value: value})
		}
	}
	if ch.discount, err = newDiscount(v.Desconto.Modalidade, v.Desconto.ValorPerc, dates, ch.dueDate); err != nil {
		return invalid("valor.desconto: %v", err)
	}
	return nil
}

func newRule(mode int32, value string, lowest, highest int32) (*rule, error) {
	if mode < lowest || mode > highest {
		return nil, fmt.Errorf("modalidade %d não suportada", mode)
	}
	v, err := pixapi.ParseValor(value)
	if err != nil {
		return nil, err
	}
	return &rule{mode: mode, value: v}, nil
}

func newDiscount(mode int32, value *string, dates []datedValue, dueDate string) (*discount, error) {
	d := &discount{mode: mode}
	switch mode {
	case 1, 2:
		if len(dates) == 0 || len(dates) > 3 {
			return nil, errors.New("descontoDataFixa deve ter de uma a três datas")
		}
		for _, dv := range dates {
			if dv.date > dueDate {
				return nil, errors.New("data de desconto posterior ao vencimento")
			}
		}
		d.dates = dates
	case 3, 5:
		if value == nil {
			return nil, errors.New("valorPerc é obrigatório")
		}
		v, err := pixapi.ParseValor(*value)
		if err != nil {
			return nil, err
		}
		d.value = v
	default:
		// 4 and 6 count business days, which the simulator has no calendar for.
		return nil, fmt.Errorf("modalidade %d não suportada", mode)
	}
	return d, nil
}

// register records a new charge and its payload location, once per txid: a txid is never
// used twice for the same receiver, even after its charge is removed.
func (s *Sim) register(ch *charge) error {
	if !pixapi.ValidTxID(ch.txid) {
		return invalid("txid deve ter de 26 a 35 letras e dígitos")
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.dict[ch.key]
	switch {
	case !ok || entry.Account != ch.client:
		return invalid("chave %q não pertence ao usuário recebedor", ch.key)
	case s.charges[ch.client+"/"+ch.txid] != nil:
		return invalid("txid %s já utilizado", ch.txid)
	}
	s.nextLoc++
	ch.status, ch.created, ch.loc, ch.token, ch.locCreated = statusActive, now, s.nextLoc, randomAlnum(32), now
	s.charges[ch.client+"/"+ch.txid] = ch
	s.locations[ch.token] = ch
	return nil
}

func (s *Sim) getCharge(w http.ResponseWriter, r *http.Request, due bool) {
	s.mu.Lock()
	ch := s.charges[clientOf(r)+"/"+r.PathValue("txid")]
	var out any
	if ch != nil && ch.due == due {
		out = s.renderLocked(ch)
	}
	s.mu.Unlock()
	if out == nil {
		problemf(w, http.StatusNotFound, "CobNaoEncontrado", "Cobrança não encontrada.", "Nenhuma cobrança com o txid %s.", r.PathValue("txid"))
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// reviseCharge applies a PATCH: removing an active charge, or changing its amount or
// the payer's message, which counts a revision.
func (s *Sim) reviseCharge(w http.ResponseWriter, r *http.Request, due bool) {
	var body struct {
		Status             *string `json:"status"`
		SolicitacaoPagador *string `json:"solicitacaoPagador"`
		Valor              *struct {
			Original *string `json:"original"`
		} `json:"valor"`
	}
	kind := "CobOperacaoInvalida"
	if due {
		kind = "CobVOperacaoInvalida"
	}
	if err := decodeBody(r, &body); err != nil {
		problemf(w, http.StatusBadRequest, kind, "Revisão inválida.", "%v", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := s.charges[clientOf(r)+"/"+r.PathValue("txid")]
	if ch == nil || ch.due != due {
		problemf(w, http.StatusNotFound, "CobNaoEncontrado", "Cobrança não encontrada.", "Nenhuma cobrança com o txid %s.", r.PathValue("txid"))
		return
	}
	if ch.status != statusActive {
		problemf(w, http.StatusBadRequest, kind, "Revisão inválida.", "A cobrança está %s e não pode ser alterada.", ch.status)
		return
	}
	switch {
	case body.Status != nil && *body.Status != statusRemoved:
		problemf(w, http.StatusBadRequest, kind, "Revisão inválida.", "O usuário recebedor só pode mudar o status para %s.", statusRemoved)
		return
	case body.Status != nil:
		ch.status = statusRemoved
	}
	if body.Valor != nil && body.Valor.Original != nil {
		v, err := pixapi.ParseValor(*body.Valor.Original)
		if err != nil || v <= 0 {
			problemf(w, http.StatusBadRequest, kind, "Revisão inválida.", "valor.original inválido.")
			return
		}
		ch.original = v
	}
	if body.SolicitacaoPagador != nil {
		ch.request = *body.SolicitacaoPagador
	}
	ch.revisao++
	writeJSON(w, http.StatusOK, s.renderLocked(ch))
}

func (s *Sim) render(ch *charge) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.renderLocked(ch)
}

func (s *Sim) renderLocked(ch *charge) any {
	if ch.due {
		return s.renderCobV(ch)
	}
	return s.renderCob(ch)
}

func (s *Sim) location(ch *charge) string {
	if ch.due {
		return s.cfg.Host + "/qr/v2/cobv/" + ch.token
	}
	return s.cfg.Host + "/qr/v2/" + ch.token
}

// copyPaste is the dynamic BR Code of the charge.
func (s *Sim) copyPaste(ch *charge) string {
	name := s.clients[ch.client].Name
	if len([]rune(name)) > 25 {
		name = string([]rune(name)[:25])
	}
	code, err := brcode.Pix{PointOfInitiation: brcode.SingleUse, URL: s.location(ch), MerchantName: name, MerchantCity: merchantCity}.Encode()
	if err != nil {
		s.cfg.Logger.Error("encoding a BR Code", "txid", ch.txid, "error", err)
	}
	return code
}

func (s *Sim) renderCob(ch *charge) pixapi.CobCompleta {
	out := pixapi.CobCompleta{
		Status: pixapi.CobStatus(ch.status), Txid: pixapi.Ptr(ch.txid), Revisao: pixapi.Ptr(ch.revisao), Chave: pixapi.Ptr(ch.key),
		Location: pixapi.Ptr(s.location(ch)), PixCopiaECola: pixapi.Ptr(s.copyPaste(ch)),
	}
	if ch.request != "" {
		out.SolicitacaoPagador = &ch.request
	}
	cal := pixapi.New(&out.Calendario)
	cal.Criacao, cal.Expiracao = ch.created, ch.expiracao
	valor := pixapi.New(&out.Valor)
	valor.Original = pixapi.FormatValor(ch.original)
	if ch.changeable {
		valor.ModalidadeAlteracao = pixapi.Ptr(int32(1))
	}
	loc := pixapi.New(&out.Loc)
	loc.Id, loc.Location, loc.TipoCob, loc.Criacao = pixapi.Ptr(ch.loc), pixapi.Ptr(s.location(ch)), "cob", pixapi.Ptr(ch.locCreated)
	for _, e2e := range ch.e2eids {
		p := s.pix[e2e].render()
		item := pixapi.Append(&out.Pix)
		item.EndToEndId, item.Txid, item.Valor, item.Horario, item.Chave = p.EndToEndId, p.Txid, p.Valor, p.Horario, p.Chave
		item.InfoPagador, item.Devolucoes = p.InfoPagador, p.Devolucoes
	}
	return out
}

func (s *Sim) renderCobV(ch *charge) pixapi.CobVCompleta {
	out := pixapi.CobVCompleta{
		Status: pixapi.CobVStatus(ch.status), Txid: pixapi.Ptr(ch.txid), Revisao: pixapi.Ptr(ch.revisao), Chave: pixapi.Ptr(ch.key),
		PixCopiaECola: pixapi.Ptr(s.copyPaste(ch)), Devedor: ch.debtor, Recebedor: s.receiver(ch.client),
	}
	if ch.request != "" {
		out.SolicitacaoPagador = &ch.request
	}
	cal := pixapi.New(&out.Calendario)
	cal.Criacao, cal.ValidadeAposVencimento = ch.created, ch.validity
	_ = cal.DataDeVencimento.UnmarshalText([]byte(ch.dueDate))
	valor := pixapi.New(&out.Valor)
	valor.Original = pixapi.FormatValor(ch.original)
	if ch.fine != nil {
		m := pixapi.New(&valor.Multa)
		m.Modalidade, m.ValorPerc = ch.fine.mode, pixapi.FormatValor(ch.fine.value)
	}
	if ch.interest != nil {
		j := pixapi.New(&valor.Juros)
		j.Modalidade, j.ValorPerc = ch.interest.mode, pixapi.FormatValor(ch.interest.value)
	}
	if ch.abatement != nil {
		a := pixapi.New(&valor.Abatimento)
		a.Modalidade, a.ValorPerc = ch.abatement.mode, pixapi.FormatValor(ch.abatement.value)
	}
	if ch.discount != nil {
		d := pixapi.New(&valor.Desconto)
		d.Modalidade = ch.discount.mode
		if ch.discount.dates == nil {
			d.ValorPerc = pixapi.Ptr(pixapi.FormatValor(ch.discount.value))
		}
		for _, dv := range ch.discount.dates {
			item := pixapi.Append(&d.DescontoDataFixa)
			_ = item.Data.UnmarshalText([]byte(dv.date))
			item.ValorPerc = pixapi.FormatValor(dv.value)
		}
	}
	loc := pixapi.New(&out.Loc)
	loc.Id, loc.Location, loc.TipoCob, loc.Criacao = pixapi.Ptr(ch.loc), pixapi.Ptr(s.location(ch)), "cobv", pixapi.Ptr(ch.locCreated)
	for _, e2e := range ch.e2eids {
		p := s.pix[e2e].render()
		item := pixapi.Append(&out.Pix)
		item.EndToEndId, item.Txid, item.Valor, item.Horario, item.Chave = p.EndToEndId, p.Txid, p.Valor, p.Horario, p.Chave
		item.InfoPagador, item.Devolucoes = p.InfoPagador, p.Devolucoes
	}
	return out
}

func (s *Sim) receiver(client string) *pixapi.Pessoa {
	a := s.clients[client]
	p := &pixapi.Pessoa{Nome: pixapi.Ptr(a.Name)}
	if len(a.TaxID) == 11 {
		p.Cpf = pixapi.Ptr(a.TaxID)
	} else {
		p.Cnpj = pixapi.Ptr(a.TaxID)
	}
	return p
}

// expired reports whether the charge can no longer be paid at now.
func (ch *charge) expired(now time.Time) bool {
	if !ch.due {
		return !now.Before(ch.created.Add(time.Duration(ch.expiracao) * time.Second))
	}
	due, _ := time.ParseInLocation(time.DateOnly, ch.dueDate, brasilia)
	return dateOf(now).After(due.AddDate(0, 0, int(ch.validity)))
}

// dateOf is the day in Brasília at t, at midnight.
func dateOf(t time.Time) time.Time {
	y, m, d := t.In(brasilia).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, brasilia)
}

// components is how a payment of a charge on a date adds up.
type components struct {
	original, fine, interest, discount, abatement int64
}

func (c components) total() int64 {
	return c.original + c.fine + c.interest - c.discount - c.abatement
}

// amountOn is what a charge asks of a payer who pays on date (a day in Brasília): for a
// cobv, the original amount less its rebate and any discount for paying early, plus
// the fine and interest for paying late. Percentages are hundredths of a percent of the
// original amount, rounded half up to the centavo; interest per month counts 30 days,
// per year 365.
func (ch *charge) amountOn(date time.Time) components {
	c := components{original: ch.original}
	if !ch.due {
		return c
	}
	due, _ := time.ParseInLocation(time.DateOnly, ch.dueDate, brasilia)
	early := int64(due.Sub(date).Hours() / 24)
	c.abatement = ch.fixedOrPercent(ch.abatement)
	if early >= 0 {
		c.discount = ch.discountOn(date, early)
	}
	if late := -early; late > 0 {
		c.fine = ch.fixedOrPercent(ch.fine)
		c.interest = ch.interestFor(late)
	}
	return c
}

// percent is p hundredths of a percent of the original amount for days days, over
// divisor, rounded half up.
func (ch *charge) percent(p, days, divisor int64) int64 {
	return (ch.original*p*days + 5000*divisor) / (10000 * divisor)
}

// fixedOrPercent reads a fine or a rebate: modality 1 is an amount, 2 a percentage.
func (ch *charge) fixedOrPercent(r *rule) int64 {
	switch {
	case r == nil:
		return 0
	case r.mode == 2:
		return ch.percent(r.value, 1, 1)
	}
	return r.value
}

func (ch *charge) discountOn(date time.Time, early int64) int64 {
	d := ch.discount
	if d == nil {
		return 0
	}
	switch d.mode {
	case 3:
		return d.value * early
	case 5:
		return ch.percent(d.value, early, 1)
	}
	for _, dv := range d.dates {
		limit, _ := time.ParseInLocation(time.DateOnly, dv.date, brasilia)
		if date.After(limit) {
			continue
		}
		if d.mode == 2 {
			return ch.percent(dv.value, 1, 1)
		}
		return dv.value
	}
	return 0
}

func (ch *charge) interestFor(late int64) int64 {
	i := ch.interest
	if i == nil {
		return 0
	}
	switch i.mode {
	case 1:
		return i.value * late
	case 2:
		return ch.percent(i.value, late, 1)
	case 3:
		return ch.percent(i.value, late, 30)
	}
	return ch.percent(i.value, late, 365)
}
