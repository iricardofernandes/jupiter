package pix

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/iricardofernandes/jupiter/pkg/pixapi"
)

// MED (Mecanismo Especial de Devolução, as MED 2.0 has it since February 2026). A payer
// contests a Pix in its bank's app; its bank reports the infraction, which reaches the
// receiving bank within 30 minutes. The receiving bank blocks what of the amount its
// client's account holds, for up to 11 days, and tells its client, who analyses it:
// agreeing returns the money to the payer (a devolução of nature MED_FRAUDE), disagreeing
// ends the block. A client whose money was returned may contest the return, within 80
// days since IN BCB 766/2026; the payer's bank upholds or rejects it.
//
// The relay is the simulator's (pkg/pixapi/med.go): the DICT's own interface is between
// participants and was not read. The windows are those the research found.

const (
	medRequestWindow      = 80 * 24 * time.Hour
	medBlock              = 11 * 24 * time.Hour
	medContestationWindow = 80 * 24 * time.Hour
)

type infraction struct {
	id, client, e2eid string
	amount            int64
	details           string
	contestedAt       time.Time
	created, modified time.Time
	status, result    string
	analysisDetails   string
	blocked           int64
	blockUntil        time.Time
	refund            *devolucao
	trace             []pixapi.TraceHop
	contestation      *pixapi.Contestation
	// upheld scripts what the payer's bank does with a contestation.
	upheld bool
}

// InfractionParams report an infraction against a Pix a client received: Amount of it
// (all, when zero), contested by the payer at ContestedAt (now, when zero). Upheld says
// what the payer's bank will do with a contestation of the return.
type InfractionParams struct {
	EndToEndID  string    `json:"endToEndId"`
	Amount      int64     `json:"amount"`
	Details     string    `json:"details"`
	ContestedAt time.Time `json:"contestedAt"`
	Upheld      bool      `json:"upheld"`
}

var errInfraction = errors.New("pix sim: infraction refused")

// ReportInfraction reports an infraction as the payer's bank would: the receiving bank
// blocks what it can of the amount and tells its client.
func (s *Sim) ReportInfraction(ctx context.Context, p InfractionParams) (pixapi.InfractionReport, error) {
	s.mu.Lock()
	now := s.now()
	pix := s.pix[p.EndToEndID]
	if p.ContestedAt.IsZero() {
		p.ContestedAt = now
	}
	switch {
	case pix == nil:
		s.mu.Unlock()
		return pixapi.InfractionReport{}, fmt.Errorf("%w: no Pix %s", errInfraction, p.EndToEndID)
	case p.ContestedAt.Sub(pix.at) > medRequestWindow:
		s.mu.Unlock()
		return pixapi.InfractionReport{}, fmt.Errorf("%w: contested more than 80 days after the Pix", errInfraction)
	}
	left := pix.amount
	for _, d := range pix.returns {
		if d.status != returnFailed {
			left -= d.amount
		}
	}
	if p.Amount == 0 {
		p.Amount = left
	}
	if p.Amount <= 0 || p.Amount > left {
		s.mu.Unlock()
		return pixapi.InfractionReport{}, fmt.Errorf("%w: %d can be claimed", errInfraction, left)
	}
	acct := s.clients[pix.client]
	inf := &infraction{
		id: "INF" + randomAlnum(29), client: pix.client, e2eid: pix.e2eid, amount: p.Amount, details: p.Details,
		contestedAt: p.ContestedAt, created: now, modified: now, status: pixapi.InfractionOpen,
		blocked: min(p.Amount, max(acct.balance-acct.blocked, 0)), blockUntil: now.Add(medBlock), upheld: p.Upheld,
	}
	acct.blocked += inf.blocked
	s.infractions[inf.id] = inf
	out := inf.render()
	base, registered := acct.webhooks[pix.key]
	s.mu.Unlock()
	if registered {
		s.post(ctx, Event{Kind: "infraction", Client: inf.client, E2EID: inf.e2eid, ID: inf.id}, base+"/infracoes", inf.id,
			pixapi.InfractionReports{InfractionReports: []pixapi.InfractionReport{out}})
	}
	return out, nil
}

func (inf *infraction) render() pixapi.InfractionReport {
	out := pixapi.InfractionReport{
		ID: inf.id, EndToEndID: inf.e2eid, Reason: pixapi.ReasonFraud, ReportDetails: inf.details, Valor: pixapi.FormatValor(inf.amount),
		Status: inf.status, AnalysisResult: inf.result, AnalysisDetails: inf.analysisDetails, ContestedAt: inf.contestedAt,
		CreationTime: inf.created, LastModified: inf.modified, FundsTrace: inf.trace, Contestation: inf.contestation,
	}
	if d := inf.refund; d != nil {
		out.RefundID, out.RefundStatus, out.RefundValor = d.id, d.status, pixapi.FormatValor(d.amount)
	}
	return out
}

// releaseLocked ends what is left of an infraction's block.
func (s *Sim) releaseLocked(inf *infraction) {
	s.clients[inf.client].blocked -= inf.blocked
	inf.blocked = 0
}

func (s *Sim) infractionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /infracoes/{id}", s.authorized(pixapi.ScopeInfractionRead, func(w http.ResponseWriter, r *http.Request) {
		s.settleDue(r.Context())
		s.mu.Lock()
		inf := s.infractions[r.PathValue("id")]
		var out pixapi.InfractionReport
		if inf != nil && inf.client == clientOf(r) {
			out = inf.render()
		}
		s.mu.Unlock()
		if out.ID == "" {
			problemf(w, http.StatusNotFound, "NaoEncontrado", "Infração não encontrada.", "Nenhuma infração %s.", r.PathValue("id"))
			return
		}
		writeJSON(w, http.StatusOK, out)
	}))
	mux.HandleFunc("GET /infracoes", s.authorized(pixapi.ScopeInfractionRead, func(w http.ResponseWriter, r *http.Request) {
		s.settleDue(r.Context())
		start, err1 := time.Parse(time.RFC3339, r.URL.Query().Get("inicio"))
		end, err2 := time.Parse(time.RFC3339, r.URL.Query().Get("fim"))
		if err1 != nil || err2 != nil || !start.Before(end) {
			problemf(w, http.StatusBadRequest, "RequisicaoInvalida", "Consulta inválida.", "inicio e fim são obrigatórios (RFC 3339), inicio antes de fim.")
			return
		}
		s.mu.Lock()
		out := pixapi.InfractionReports{InfractionReports: []pixapi.InfractionReport{}}
		for _, inf := range s.infractions {
			if inf.client == clientOf(r) && !inf.created.Before(start) && inf.created.Before(end) {
				out.InfractionReports = append(out.InfractionReports, inf.render())
			}
		}
		s.mu.Unlock()
		slices.SortFunc(out.InfractionReports, func(a, b pixapi.InfractionReport) int {
			if c := a.CreationTime.Compare(b.CreationTime); c != 0 {
				return c
			}
			return compareStrings(a.ID, b.ID)
		})
		writeJSON(w, http.StatusOK, out)
	}))
	mux.HandleFunc("POST /infracoes/{id}/analise", s.authorized(pixapi.ScopeInfractionWrite, s.analyse))
	mux.HandleFunc("POST /infracoes/{id}/contestacao", s.authorized(pixapi.ScopeInfractionWrite, s.contest))
}

// analyse answers POST /infracoes/{id}/analise: AGREED returns up to what the claim
// blocked, or more if the account has it, to the payer; DISAGREED ends the block. The
// same answer again is answered with the report as it is.
func (s *Sim) analyse(w http.ResponseWriter, r *http.Request) {
	var body pixapi.InfractionAnalysis
	if err := decodeBody(r, &body); err != nil {
		problemf(w, http.StatusBadRequest, "RequisicaoInvalida", "Análise inválida.", "%v", err)
		return
	}
	s.mu.Lock()
	inf := s.infractions[r.PathValue("id")]
	if inf == nil || inf.client != clientOf(r) {
		s.mu.Unlock()
		problemf(w, http.StatusNotFound, "NaoEncontrado", "Infração não encontrada.", "Nenhuma infração %s.", r.PathValue("id"))
		return
	}
	if inf.status == pixapi.InfractionClosed {
		out, same := inf.render(), inf.result == body.AnalysisResult
		s.mu.Unlock()
		if !same {
			problemf(w, http.StatusConflict, "InfracaoEncerrada", "Infração encerrada.", "A infração já foi analisada.")
			return
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	settle, problem := s.applyAnalysisLocked(inf, body)
	out := inf.render()
	p := s.pix[inf.e2eid]
	s.mu.Unlock()
	if problem != "" {
		problemf(w, http.StatusBadRequest, "RequisicaoInvalida", "Análise inválida.", "%s", problem)
		return
	}
	if settle != nil {
		s.settleReturn(r.Context(), p, settle)
		s.mu.Lock()
		out = inf.render()
		s.mu.Unlock()
	}
	writeJSON(w, http.StatusOK, out)
}

// applyAnalysisLocked closes an open infraction as analysed, and returns the MED return
// to settle when the client agreed.
func (s *Sim) applyAnalysisLocked(inf *infraction, body pixapi.InfractionAnalysis) (*devolucao, string) {
	var settle *devolucao
	switch body.AnalysisResult {
	case pixapi.AnalysisAgreed:
		amount, err := pixapi.ParseValor(body.Valor)
		if err != nil || amount <= 0 || amount > inf.amount || !pixapi.ValidID(body.RefundID) {
			return nil, "valor de 0,01 até o reclamado e refundId de 1 a 35 letras e dígitos são obrigatórios."
		}
		now := s.now()
		settle = &devolucao{
			id: body.RefundID, amount: amount, nature: string(pixapi.DevolucaoNaturezaMEDFRAUDE), status: returnProcessing,
			rtrID: endToEndID("D", s.cfg.ISPB, now), requested: now, description: "MED " + inf.id,
		}
		s.pix[inf.e2eid].returns = append(s.pix[inf.e2eid].returns, settle)
		inf.refund = settle
		if f := s.fault(Event{Kind: "return", Client: inf.client, E2EID: inf.e2eid, ID: settle.id}); f.Pending {
			settle.pendingUntil = now.Add(f.PendingFor)
			settle = nil // settled when due, its block kept until then
		}
	case pixapi.AnalysisDisagreed:
		s.releaseLocked(inf)
	default:
		return nil, "analysisResult é AGREED ou DISAGREED."
	}
	inf.status, inf.result, inf.analysisDetails, inf.modified = pixapi.InfractionClosed, body.AnalysisResult, body.AnalysisDetails, s.now()
	inf.trace = body.FundsTrace
	return settle, ""
}

// releaseForReturnLocked ends the block of the infraction a MED return pays: the return
// is paid out of what it held.
func (s *Sim) releaseForReturnLocked(d *devolucao) {
	for _, inf := range s.infractions {
		if inf.refund == d {
			s.releaseLocked(inf)
			return
		}
	}
}

// contest answers POST /infracoes/{id}/contestacao: the client contests a MED return
// within 80 days of it, and the payer's bank decides at once, giving the money back if it
// upholds the contestation.
func (s *Sim) contest(w http.ResponseWriter, r *http.Request) {
	var body pixapi.InfractionContestation
	if err := decodeBody(r, &body); err != nil {
		problemf(w, http.StatusBadRequest, "RequisicaoInvalida", "Contestação inválida.", "%v", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	inf := s.infractions[r.PathValue("id")]
	switch {
	case inf == nil || inf.client != clientOf(r):
		problemf(w, http.StatusNotFound, "NaoEncontrado", "Infração não encontrada.", "Nenhuma infração %s.", r.PathValue("id"))
		return
	case inf.contestation != nil:
		writeJSON(w, http.StatusOK, inf.render())
		return
	case inf.refund == nil || inf.refund.status != returnDone:
		problemf(w, http.StatusBadRequest, "RequisicaoInvalida", "Contestação inválida.", "Só uma devolução MED realizada pode ser contestada.")
		return
	case s.now().Sub(inf.refund.settled) > medContestationWindow:
		problemf(w, http.StatusBadRequest, "RequisicaoInvalida", "Contestação inválida.", "O prazo de 80 dias para contestar passou.")
		return
	}
	inf.contestation = &pixapi.Contestation{Status: pixapi.ContestationRejected, Details: body.Details}
	if inf.upheld {
		inf.contestation.Status = pixapi.ContestationUpheld
		inf.contestation.ReversalEndToEndID = endToEndID("E", PayerISPB, s.now())
		s.clients[inf.client].balance += inf.refund.amount
		s.bookLocked(s.clients[inf.client], pixapi.StatementCredit, inf.refund.amount, inf.refund.id, inf.contestation.ReversalEndToEndID, "CONTESTACAO MED ACOLHIDA")
	}
	inf.modified = s.now()
	writeJSON(w, http.StatusOK, inf.render())
}

// releaseLapsedLocked ends the blocks of infractions still open past 11 days.
func (s *Sim) releaseLapsedLocked(now time.Time) {
	for _, inf := range s.infractions {
		if inf.status == pixapi.InfractionOpen && inf.blocked > 0 && !now.Before(inf.blockUntil) {
			s.releaseLocked(inf)
			inf.modified = now
		}
	}
}

// Infractions lists every infraction's report, for tests.
func (s *Sim) Infractions() []pixapi.InfractionReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]pixapi.InfractionReport, 0, len(s.infractions))
	for _, inf := range s.infractions {
		out = append(out, inf.render())
	}
	slices.SortFunc(out, func(a, b pixapi.InfractionReport) int { return compareStrings(a.ID, b.ID) })
	return out
}

// Blocked is what MED claims block of a client's account.
func (s *Sim) Blocked(client string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.clients[client]; a != nil {
		return a.blocked
	}
	return 0
}
