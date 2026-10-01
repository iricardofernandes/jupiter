package bank

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
)

const maxBody = 16 << 20

// Handler serves the bank's API to its clients, each with its token as a bearer token.
func (s *Sim) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/collection/remittances", s.auth(s.remit))
	mux.HandleFunc("GET /v1/collection/returns", s.auth(s.listReturns))
	mux.HandleFunc("GET /v1/collection/returns/{sequence}", s.auth(s.returnFile))
	mux.HandleFunc("POST /v1/transfers", s.auth(s.transfer))
	mux.HandleFunc("GET /v1/transfers/{id}", s.auth(s.getTransfer))
	mux.HandleFunc("GET /v1/statements", s.auth(s.statement))
	return mux
}

func (s *Sim) auth(h func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" || s.byToken[token] == nil {
			problem(w, http.StatusUnauthorized, "unknown client")
			return
		}
		h(w, r, token)
	}
}

func (s *Sim) remit(w http.ResponseWriter, r *http.Request, token string) {
	data, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(data) > maxBody {
		problem(w, http.StatusRequestEntityTooLarge, "the remittance is too large")
		return
	}
	n, err := s.Remit(token, data)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]int{"titles": n})
}

func (s *Sim) listReturns(w http.ResponseWriter, r *http.Request, token string) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	files, err := s.Returns(token, after)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]int64{"files": files})
}

func (s *Sim) returnFile(w http.ResponseWriter, r *http.Request, token string) {
	sequence, err := strconv.ParseInt(r.PathValue("sequence"), 10, 64)
	if err != nil {
		problem(w, http.StatusNotFound, "no such return")
		return
	}
	data, err := s.ReturnFile(token, sequence)
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write(data)
}

func (s *Sim) transfer(w http.ResponseWriter, r *http.Request, token string) {
	var req TransferRequest
	if !decode(w, r, &req) {
		return
	}
	t, err := s.Transfer(token, req)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Sim) getTransfer(w http.ResponseWriter, r *http.Request, token string) {
	t, err := s.TransferStatusOf(token, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Sim) statement(w http.ResponseWriter, r *http.Request, token string) {
	entries, err := s.Statement(token, r.URL.Query().Get("date"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]Entry{"entries": entries})
}

// AdminHandler is the payers' and the operator's side, for loopback only: paying a
// boleto by its line or its Pix QR code, and closing the day.
func (s *Sim) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/boletos/pay", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Line    string `json:"line"`
			PixCode string `json:"pix_code"`
		}
		if !decode(w, r, &body) {
			return
		}
		var ourNumber string
		var err error
		if body.PixCode != "" {
			ourNumber, err = s.PayPix(body.PixCode)
		} else {
			ourNumber, err = s.Pay(body.Line)
		}
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"our_number": ourNumber})
	})
	mux.HandleFunc("POST /admin/tick", func(w http.ResponseWriter, _ *http.Request) {
		s.Tick()
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
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
	case errors.Is(err, ErrConflict):
		status = http.StatusConflict
	case errors.Is(err, ErrInvalid):
		status = http.StatusUnprocessableEntity
	}
	problem(w, status, err.Error())
}

func problem(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
