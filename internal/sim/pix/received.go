package pix

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/iricardofernandes/jupiter/pkg/pixapi"
)

// Pix received by the bank's clients, and the returns (devoluções) they make of them.

type received struct {
	client     string
	e2eid      string
	txid       string
	key        string
	amount     int64
	at         time.Time
	message    string
	payerTaxID string
	components *components
	returns    []*devolucao
}

type devolucao struct {
	id           string
	rtrID        string
	amount       int64
	nature       string
	description  string
	status       string
	requested    time.Time
	settled      time.Time
	reason       string
	pendingUntil time.Time
}

const (
	returnProcessing = "EM_PROCESSAMENTO"
	returnDone       = "DEVOLVIDO"
	returnFailed     = "NAO_REALIZADO"

	defaultPageSize = 100
	maxPageSize     = 1000
)

func (p *received) render() pixapi.Pix {
	out := pixapi.Pix{EndToEndId: p.e2eid, Valor: pixapi.FormatValor(p.amount), Horario: p.at, Chave: pixapi.Ptr(p.key)}
	if p.txid != "" {
		out.Txid = pixapi.Ptr(p.txid)
	}
	if p.message != "" {
		out.InfoPagador = pixapi.Ptr(p.message)
	}
	if c := p.components; c != nil {
		parts := map[string]map[string]string{"original": {"valor": pixapi.FormatValor(c.original)}}
		for name, v := range map[string]int64{"multa": c.fine, "juros": c.interest, "desconto": c.discount, "abatimento": c.abatement} {
			if v > 0 {
				parts[name] = map[string]string{"valor": pixapi.FormatValor(v)}
			}
		}
		raw, _ := json.Marshal(parts)
		cv := pixapi.New(&out.ComponentesValor)
		_ = cv.UnmarshalJSON(raw)
	}
	for _, d := range p.returns {
		item := pixapi.Append(&out.Devolucoes)
		item.Id, item.RtrId, item.Valor, item.Status = d.id, d.rtrID, pixapi.FormatValor(d.amount), pixapi.DevolucaoStatus(d.status)
		item.Natureza = pixapi.Ptr(pixapi.DevolucaoNatureza(d.nature))
		item.Horario.Solicitacao = pixapi.Ptr(d.requested)
		if !d.settled.IsZero() {
			item.Horario.Liquidacao = pixapi.Ptr(d.settled)
		}
		if d.description != "" {
			item.Descricao = pixapi.Ptr(d.description)
		}
		if d.reason != "" {
			item.Motivo = pixapi.Ptr(d.reason)
		}
	}
	return out
}

func (s *Sim) pixRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /pix", s.authorized(pixapi.ScopePixRead, s.listPix))
	mux.HandleFunc("GET /pix/{e2eid}", s.authorized(pixapi.ScopePixRead, func(w http.ResponseWriter, r *http.Request) {
		s.settleDue(r.Context())
		s.mu.Lock()
		p := s.pix[r.PathValue("e2eid")]
		var out pixapi.Pix
		if p != nil && p.client == clientOf(r) {
			out = p.render()
		}
		s.mu.Unlock()
		if out.EndToEndId == "" {
			problemf(w, http.StatusNotFound, "PixNaoEncontrado", "Pix não encontrado.", "Nenhum Pix recebido com o endToEndId %s.", r.PathValue("e2eid"))
			return
		}
		writeJSON(w, http.StatusOK, out)
	}))
	mux.HandleFunc("PUT /pix/{e2eid}/devolucao/{id}", s.authorized(pixapi.ScopePixWrite, s.requestReturn))
	mux.HandleFunc("GET /pix/{e2eid}/devolucao/{id}", s.authorized(pixapi.ScopePixRead, func(w http.ResponseWriter, r *http.Request) {
		s.settleDue(r.Context())
		s.mu.Lock()
		out, ok := s.findReturn(clientOf(r), r.PathValue("e2eid"), r.PathValue("id"))
		s.mu.Unlock()
		if !ok {
			problemf(w, http.StatusNotFound, "NaoEncontrado", "Devolução não encontrada.", "Nenhuma devolução %s do Pix %s.", r.PathValue("id"), r.PathValue("e2eid"))
			return
		}
		writeJSON(w, http.StatusOK, out)
	}))
}

// listPix answers GET /pix: the client's Pix received between inicio and fim, oldest
// first, a page at a time.
func (s *Sim) listPix(w http.ResponseWriter, r *http.Request) {
	s.settleDue(r.Context())
	q := r.URL.Query()
	start, err1 := time.Parse(time.RFC3339, q.Get("inicio"))
	end, err2 := time.Parse(time.RFC3339, q.Get("fim"))
	page, _ := strconv.Atoi(q.Get("paginacao.paginaAtual"))
	size := defaultPageSize
	if v := q.Get("paginacao.itensPorPagina"); v != "" {
		size, _ = strconv.Atoi(v)
	}
	if err1 != nil || err2 != nil || !start.Before(end) || page < 0 || size < 1 || size > maxPageSize {
		problemf(w, http.StatusBadRequest, "RequisicaoInvalida", "Consulta inválida.", "inicio e fim são obrigatórios (RFC 3339, inicio antes de fim); a página e o tamanho devem ser válidos.")
		return
	}
	s.mu.Lock()
	var found []*received
	for _, p := range s.pix {
		if p.client == clientOf(r) && !p.at.Before(start) && p.at.Before(end) && (q.Get("txid") == "" || p.txid == q.Get("txid")) {
			found = append(found, p)
		}
	}
	slices.SortFunc(found, func(a, b *received) int {
		if c := a.at.Compare(b.at); c != 0 {
			return c
		}
		return compareStrings(a.e2eid, b.e2eid)
	})
	out := pixapi.PixConsultados{Parametros: pixapi.ParametrosConsultaPix{Inicio: start, Fim: end}} //nolint:misspell // the API Pix's field name
	out.Parametros.Paginacao = pixapi.Paginacao{                                                    //nolint:misspell // the API Pix's field name
		PaginaAtual: page, ItensPorPagina: size, QuantidadeTotalDeItens: len(found), QuantidadeDePaginas: (len(found) + size - 1) / size,
	}
	items := []pixapi.Pix{}
	for i := page * size; i < len(found) && i < (page+1)*size; i++ {
		items = append(items, found[i].render())
	}
	s.mu.Unlock()
	out.Pix = &items
	writeJSON(w, http.StatusOK, out)
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (s *Sim) findReturn(client, e2eid, id string) (pixapi.Devolucao, bool) {
	p := s.pix[e2eid]
	if p == nil || p.client != client {
		return pixapi.Devolucao{}, false
	}
	for i, d := range p.returns {
		if d.id == id {
			return (*p.render().Devolucoes)[i], true
		}
	}
	return pixapi.Devolucao{}, false
}

// requestReturn answers PUT /pix/{e2eid}/devolucao/{id}. The id is the client's: asking
// again with the same id and amount answers with the return already made; the returns of
// a Pix never add up to more than it.
func (s *Sim) requestReturn(w http.ResponseWriter, r *http.Request) {
	var body pixapi.DevolucaoSolicitada
	if err := decodeBody(r, &body); err != nil {
		problemf(w, http.StatusBadRequest, "PixDevolucaoInvalida", "Devolução inválida.", "%v", err)
		return
	}
	amount, err := pixapi.ParseValor(body.Valor)
	e2eid, id := r.PathValue("e2eid"), r.PathValue("id")
	nature := string(pixapi.DevolucaoNaturezaORIGINAL)
	if body.Natureza != nil {
		nature = string(*body.Natureza)
	}
	if err != nil || amount <= 0 || !pixapi.ValidID(id) || nature != string(pixapi.DevolucaoNaturezaORIGINAL) {
		problemf(w, http.StatusBadRequest, "PixDevolucaoInvalida", "Devolução inválida.", "valor positivo, id de 1 a 35 letras e dígitos e natureza ORIGINAL são obrigatórios.")
		return
	}
	d := &devolucao{id: id, amount: amount, nature: nature}
	if body.Descricao != nil {
		d.description = *body.Descricao
	}
	p, out, status, problem := s.recordReturn(clientOf(r), e2eid, d)
	if problem != "" {
		kind := "PixDevolucaoInvalida"
		if status == http.StatusNotFound {
			kind = "PixNaoEncontrado"
		}
		problemf(w, status, kind, "Devolução inválida.", "%s", problem)
		return
	}
	writeJSON(w, status, out)
	if p != nil {
		ctx := context.WithoutCancel(r.Context())
		s.background.Go(func() { s.settleReturn(ctx, p, d) })
	}
}

// recordReturn records a new return of a Pix, or finds the one already asked for with
// the same id. It returns the Pix when the return is new and must be settled now, the
// return as the API shows it, and the status to answer with, or a problem.
func (s *Sim) recordReturn(client, e2eid string, d *devolucao) (settle *received, out pixapi.Devolucao, status int, problem string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pix[e2eid]
	if p == nil || p.client != client {
		return nil, out, http.StatusNotFound, "Nenhum Pix recebido com o endToEndId " + e2eid + "."
	}
	returned := int64(0)
	for _, existing := range p.returns {
		if existing.id == d.id {
			if existing.amount != d.amount {
				return nil, out, http.StatusConflict, "A devolução " + d.id + " já existe com outro valor."
			}
			out, _ = s.findReturn(client, e2eid, d.id)
			return nil, out, http.StatusCreated, ""
		}
		if existing.status != returnFailed {
			returned += existing.amount
		}
	}
	if returned+d.amount > p.amount {
		return nil, out, http.StatusBadRequest, "As devoluções somariam " + pixapi.FormatValor(returned+d.amount) +
			", mais que o Pix de " + pixapi.FormatValor(p.amount) + "."
	}
	now := s.now()
	d.rtrID, d.status, d.requested = endToEndID("D", s.cfg.ISPB, now), returnProcessing, now
	if f := s.fault(Event{Kind: "return", Client: client, E2EID: e2eid, ID: d.id}); f.Pending {
		d.pendingUntil = now.Add(f.PendingFor)
	}
	p.returns = append(p.returns, d)
	out, _ = s.findReturn(client, e2eid, d.id)
	if d.pendingUntil.IsZero() {
		settle = p
	}
	return settle, out, http.StatusCreated, ""
}

// settleReturn sends a return back to the payer through the SPI (a pacs.004) and tells
// the receiver how it ended.
func (s *Sim) settleReturn(ctx context.Context, p *received, d *devolucao) {
	s.mu.Lock()
	if d.status != returnProcessing {
		s.mu.Unlock()
		return
	}
	acct := s.clients[p.client]
	if d.nature == string(pixapi.DevolucaoNaturezaMEDFRAUDE) {
		s.releaseForReturnLocked(d)
	}
	switch {
	case s.closedPayers[p.payerTaxID]:
		d.status, d.reason = returnFailed, "Conta do pagador encerrada."
	case acct.balance-acct.blocked < d.amount:
		// A MED return's block is released to pay it; other returns cannot spend a block.
		d.status, d.reason = returnFailed, "Saldo insuficiente."
	default:
		acct.balance -= d.amount
		d.status = returnDone
	}
	d.settled = s.now()
	s.mu.Unlock()
	s.notify(ctx, p)
}

// settleDue settles the returns and transfers held in processing whose time has come.
func (s *Sim) settleDue(ctx context.Context) {
	now := s.now()
	var due []func()
	s.mu.Lock()
	for _, p := range s.pix {
		for _, d := range p.returns {
			if d.status == returnProcessing && !d.pendingUntil.IsZero() && !now.Before(d.pendingUntil) {
				d.pendingUntil = time.Time{}
				due = append(due, func() { s.settleReturn(ctx, p, d) })
			}
		}
	}
	for _, t := range s.transfers {
		if t.status == transferProcessing && !now.Before(t.pendingUntil) {
			s.settleTransferLocked(t)
		}
	}
	s.releaseLapsedLocked(now)
	s.mu.Unlock()
	for _, f := range due {
		f()
	}
}
