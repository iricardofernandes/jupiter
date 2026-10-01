package pix

import (
	"fmt"
	"net/http"
	"time"

	"github.com/iricardofernandes/jupiter/pkg/pixapi"
)

// Statements: each client's account lists every movement the SPI settled in it, a line a
// movement, by its day in Brasília. A test can lose a line, write it twice, or put it on
// the next day (Fault's Drop, Duplicate and Delay, for events of kind "statement").

// bookLocked writes a statement line for a movement of a client's account.
func (s *Sim) bookLocked(acct *account, kind string, amount int64, reference, e2eid, description string) {
	day := s.now().In(brasilia)
	f := s.fault(Event{Kind: "statement", Client: acct.ID, E2EID: e2eid, ID: reference})
	if f.Drop {
		return
	}
	if f.Delay {
		day = day.AddDate(0, 0, 1)
	}
	for range 1 + btoi(f.Duplicate) {
		acct.entries++
		acct.statement = append(acct.statement, pixapi.StatementLine{
			ID: fmt.Sprintf("SPI%09d", acct.entries), Data: day.Format(time.DateOnly), Tipo: kind, Valor: pixapi.FormatValor(amount),
			Referencia: reference, EndToEndID: e2eid, Descricao: description,
		})
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// statementRoute answers GET /extrato?data=YYYY-MM-DD: the client's lines of that day.
func (s *Sim) statementRoute(mux *http.ServeMux) {
	mux.HandleFunc("GET /extrato", s.authorized(pixapi.ScopePixRead, func(w http.ResponseWriter, r *http.Request) {
		day := r.URL.Query().Get("data")
		if _, err := time.Parse(time.DateOnly, day); err != nil {
			problemf(w, http.StatusBadRequest, "RequisicaoInvalida", "Consulta inválida.", "data é obrigatória, AAAA-MM-DD.")
			return
		}
		s.settleDue(r.Context())
		s.mu.Lock()
		out := pixapi.Statement{Lancamentos: []pixapi.StatementLine{}}
		for _, l := range s.clients[clientOf(r)].statement {
			if l.Data == day {
				out.Lancamentos = append(out.Lancamentos, l)
			}
		}
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, out)
	}))
}
