package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/iricardofernandes/jupiter/pkg/registryapi"
)

const (
	maxBody  = 4 << 20
	maxUnits = 1000
)

// Handler serves the registry's API. Every request carries a participant's token as a
// bearer token; accreditors register units, settle them and pass on merchants' opt-ins,
// financiers place and end contracts and read the agendas they were authorized to see.
func (s *Sim) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/units", s.as(Accreditor, s.putUnits))
	mux.HandleFunc("GET /v1/units", s.as("", s.listUnits))
	mux.HandleFunc("GET /v1/instructions", s.as(Accreditor, s.instructions))
	mux.HandleFunc("POST /v1/settlements", s.as(Accreditor, s.settle))
	mux.HandleFunc("POST /v1/opt-ins", s.as(Accreditor, s.optIn(true)))
	mux.HandleFunc("POST /v1/opt-ins/revoke", s.as(Accreditor, s.optIn(false)))
	mux.HandleFunc("GET /v1/holders", s.as(Accreditor, s.holders))
	mux.HandleFunc("GET /v1/compliance", s.as(Accreditor, s.compliance))
	mux.HandleFunc("POST /v1/contracts", s.as(Financier, s.accept))
	mux.HandleFunc("POST /v1/contracts/{id}/end", s.as(Financier, s.end))
	return mux
}

type handler func(http.ResponseWriter, *http.Request, Participant)

func (s *Sim) as(role Role, h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		p, known := s.byTok[token]
		switch {
		case !ok || !known:
			problem(w, http.StatusUnauthorized, "unknown participant")
		case role != "" && p.Role != role:
			problem(w, http.StatusForbidden, fmt.Sprintf("only a %s may do that", role))
		default:
			h(w, r, p)
		}
	}
}

func (s *Sim) putUnits(w http.ResponseWriter, r *http.Request, p Participant) {
	var req registryapi.UnitsRequest
	if !decode(w, r, &req) {
		return
	}
	if len(req.Units) > maxUnits {
		problem(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("at most %d units at a time", maxUnits))
		return
	}
	writeJSON(w, http.StatusOK, registryapi.UnitsResponse{Results: s.SetUnits(p.TaxID, req.Units)})
}

func (s *Sim) listUnits(w http.ResponseWriter, r *http.Request, p Participant) {
	q := r.URL.Query()
	var settled *bool
	if v := q.Get("settled"); v != "" {
		b := v == "true"
		settled = &b
	}
	units, err := s.Positions(p, q.Get("holder"), q.Get("from"), q.Get("to"), settled)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, registryapi.PositionsResponse{Units: units})
}

func (s *Sim) instructions(w http.ResponseWriter, r *http.Request, p Participant) {
	q := r.URL.Query()
	out, err := s.Instructions(UnitKey{Accreditor: p.TaxID, Holder: q.Get("holder"), Arrangement: q.Get("arrangement"), Date: q.Get("settlement_date")})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, registryapi.InstructionsResponse{Payments: out})
}

func (s *Sim) settle(w http.ResponseWriter, r *http.Request, p Participant) {
	var n registryapi.Settlement
	if !decode(w, r, &n) {
		return
	}
	out, err := s.Settle(p.TaxID, n)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, registryapi.SettlementResponse{Payments: out})
}

func (s *Sim) optIn(on bool) handler {
	return func(w http.ResponseWriter, r *http.Request, p Participant) {
		var o registryapi.OptIn
		if !decode(w, r, &o) {
			return
		}
		if o.Holder == "" || o.Financier == "" {
			problem(w, http.StatusBadRequest, "an opt-in needs a holder and a financier")
			return
		}
		if err := s.SetOptIn(p.TaxID, o, on); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, o)
	}
}

func (s *Sim) holders(w http.ResponseWriter, _ *http.Request, p Participant) {
	writeJSON(w, http.StatusOK, registryapi.HoldersResponse{Holders: s.HoldersWithContracts(p.TaxID)})
}

func (s *Sim) compliance(w http.ResponseWriter, _ *http.Request, p Participant) {
	writeJSON(w, http.StatusOK, registryapi.ComplianceResponse{Late: s.Late(p.TaxID)})
}

func (s *Sim) accept(w http.ResponseWriter, r *http.Request, p Participant) {
	var c registryapi.Contract
	if !decode(w, r, &c) {
		return
	}
	if err := s.Accept(p.TaxID, c); err != nil {
		fail(w, err)
		return
	}
	c.Beneficiary = p.TaxID
	writeJSON(w, http.StatusCreated, c)
}

func (s *Sim) end(w http.ResponseWriter, r *http.Request, p Participant) {
	if err := s.End(p.TaxID, r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		problem(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return false
	}
	return true
}

func fail(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, ErrForbidden):
		status = http.StatusForbidden
	case errors.Is(err, ErrSettled):
		status = http.StatusConflict
	case errors.Is(err, ErrInvalid), errors.Is(err, ErrBlock):
		status = http.StatusUnprocessableEntity
	}
	problem(w, status, err.Error())
}

func problem(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, registryapi.Problem{Error: msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
