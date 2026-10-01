package pix

import (
	"net/http"
	"time"

	"github.com/iricardofernandes/jupiter/pkg/pixapi"
)

// Transfers: the bank's clients send Pix to any key. The API Pix does not cover sending,
// so each bank offers its own interface; this one follows the API Pix's conventions: the
// client names each transfer (idEnvio), a PUT with the same id answers with the
// transfer already made, and a GET says how it ended. The bank looks the key up in DICT
// and settles through the SPI at once, unless a fault holds it in processing.

type transfer struct {
	client       string
	id           string
	e2eid        string
	amount       int64
	key          string
	description  string
	status       string
	reason       string
	requested    time.Time
	settled      time.Time
	pendingUntil time.Time
	destination  Entry
}

const (
	transferProcessing = "EM_PROCESSAMENTO"
	transferDone       = "REALIZADO"
	transferFailed     = "NAO_REALIZADO"
)

// TransferRequest is the body of PUT /transferencias/{idEnvio}.
type TransferRequest struct {
	Valor     string  `json:"valor"`
	Chave     string  `json:"chave"`
	Descricao *string `json:"descricao,omitempty"`
}

// Transfer is what the bank says of one.
type Transfer struct {
	IDEnvio    string     `json:"idEnvio"`
	EndToEndID string     `json:"endToEndId"`
	Valor      string     `json:"valor"`
	Chave      string     `json:"chave"`
	Status     string     `json:"status"`
	Motivo     string     `json:"motivo,omitempty"`
	Horario    time.Time  `json:"horario"`
	Liquidacao *time.Time `json:"liquidacao,omitempty"`
	Favorecido *Recipient `json:"favorecido,omitempty"`
}

// Recipient is who a transfer reached, as DICT names them.
type Recipient struct {
	Nome string `json:"nome"`
	ISPB string `json:"ispb"`
}

func (t *transfer) render() Transfer {
	out := Transfer{
		IDEnvio: t.id, EndToEndID: t.e2eid, Valor: pixapi.FormatValor(t.amount), Chave: t.key, Status: t.status,
		Motivo: t.reason, Horario: t.requested,
	}
	if !t.settled.IsZero() {
		out.Liquidacao = pixapi.Ptr(t.settled)
	}
	if t.status == transferDone {
		out.Favorecido = &Recipient{Nome: t.destination.Name, ISPB: t.destination.ISPB}
	}
	return out
}

func (s *Sim) transferRoutes(mux *http.ServeMux) {
	mux.HandleFunc("PUT /transferencias/{id}", s.authorized(pixapi.ScopePixWrite, s.sendTransfer))
	mux.HandleFunc("GET /transferencias/{id}", s.authorized(pixapi.ScopePixRead, func(w http.ResponseWriter, r *http.Request) {
		s.settleDue(r.Context())
		s.mu.Lock()
		t := s.transfers[clientOf(r)+"/"+r.PathValue("id")]
		var out Transfer
		if t != nil {
			out = t.render()
		}
		s.mu.Unlock()
		if t == nil {
			problemf(w, http.StatusNotFound, "NaoEncontrado", "Transferência não encontrada.", "Nenhuma transferência %s.", r.PathValue("id"))
			return
		}
		writeJSON(w, http.StatusOK, out)
	}))
}

func (s *Sim) sendTransfer(w http.ResponseWriter, r *http.Request) {
	var body TransferRequest
	if err := decodeBody(r, &body); err != nil {
		problemf(w, http.StatusBadRequest, "RequisicaoInvalida", "Transferência inválida.", "%v", err)
		return
	}
	amount, err := pixapi.ParseValor(body.Valor)
	id := r.PathValue("id")
	if err != nil || amount <= 0 || !pixapi.ValidID(id) || body.Chave == "" || len(body.Chave) > 77 {
		problemf(w, http.StatusBadRequest, "RequisicaoInvalida", "Transferência inválida.", "valor positivo, chave e idEnvio de 1 a 35 letras e dígitos são obrigatórios.")
		return
	}
	client := clientOf(r)
	s.mu.Lock()
	if t := s.transfers[client+"/"+id]; t != nil {
		out := t.render()
		same := t.amount == amount && t.key == body.Chave
		s.mu.Unlock()
		if !same {
			problemf(w, http.StatusConflict, "RequisicaoInvalida", "Transferência inválida.", "A transferência %s já existe com outros dados.", id)
			return
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	now := s.now()
	t := &transfer{
		client: client, id: id, e2eid: endToEndID("E", s.cfg.ISPB, now), amount: amount, key: body.Chave,
		status: transferProcessing, requested: now,
	}
	if body.Descricao != nil {
		t.description = *body.Descricao
	}
	s.transfers[client+"/"+id] = t
	f := s.fault(Event{Kind: "transfer", Client: client, ID: id})
	if f.Pending {
		t.pendingUntil = now.Add(f.PendingFor)
	} else {
		s.settleTransferLocked(t)
	}
	out := t.render()
	s.mu.Unlock()
	if f.LoseResponse {
		panic(http.ErrAbortHandler)
	}
	writeJSON(w, http.StatusCreated, out)
}

// settleTransferLocked resolves the key and moves the money, or refuses the transfer.
func (s *Sim) settleTransferLocked(t *transfer) {
	acct := s.clients[t.client]
	entry, found := s.dict[t.key]
	switch {
	case !found:
		t.status, t.reason = transferFailed, "Chave não encontrada no DICT."
	case entry.Closed:
		t.status, t.reason = transferFailed, "Conta do favorecido encerrada."
	case acct.balance-acct.blocked < t.amount:
		t.status, t.reason = transferFailed, "Saldo insuficiente."
	default:
		acct.balance -= t.amount
		s.bookLocked(acct, pixapi.StatementDebit, t.amount, t.id, t.e2eid, "PIX ENVIADO")
		if receiver := s.clients[entry.Account]; receiver != nil && entry.ISPB == s.cfg.ISPB {
			receiver.balance += t.amount
			s.bookLocked(receiver, pixapi.StatementCredit, t.amount, t.e2eid, t.e2eid, "PIX RECEBIDO")
		}
		t.status, t.destination = transferDone, entry
	}
	t.settled = s.now()
}

// Transfers returns what the bank's clients have sent, for tests.
func (s *Sim) Transfers() []Transfer {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Transfer, 0, len(s.transfers))
	for _, t := range s.transfers {
		out = append(out, t.render())
	}
	return out
}
