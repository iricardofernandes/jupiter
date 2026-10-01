package slc

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/iricardofernandes/jupiter/pkg/slcapi"
)

// Handler serves the settlement system's API to its participants, each with its token as
// a bearer token.
func (s *Sim) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/grades", s.auth(func(w http.ResponseWriter, r *http.Request, token string) {
		var g Grade
		if !decode(w, r, &g) {
			return
		}
		out, err := s.Submit(token, g)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}))
	mux.HandleFunc("GET /v1/grades/{date}", s.auth(func(w http.ResponseWriter, r *http.Request, token string) {
		g, err := s.GradeOf(token, r.PathValue("date"))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, g)
	}))
	mux.HandleFunc("POST /v1/anticipation-reports", s.auth(func(w http.ResponseWriter, r *http.Request, token string) {
		var body slcapi.ReportsRequest
		if !decode(w, r, &body) {
			return
		}
		if err := s.ReportAnticipations(token, body.Reports); err != nil {
			fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("GET /v1/compliance", s.auth(func(w http.ResponseWriter, _ *http.Request, token string) {
		_, late := s.Reports(token)
		if late == nil {
			late = []Late{}
		}
		writeJSON(w, http.StatusOK, slcapi.ComplianceResponse{Late: late})
	}))
	return mux
}

func (s *Sim) auth(h func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" || s.byToken[token] == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unknown participant"})
			return
		}
		h(w, r, token)
	}
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 32<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body: " + err.Error()})
		return false
	}
	return true
}

func fail(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, ErrConflict):
		status = http.StatusConflict
	case errors.Is(err, ErrInvalid):
		status = http.StatusUnprocessableEntity
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// AdminHandler serves the operator's controls, unauthenticated: POST /admin/tick runs the
// settlement window now.
func (s *Sim) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/tick", func(w http.ResponseWriter, _ *http.Request) {
		s.Tick()
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}
