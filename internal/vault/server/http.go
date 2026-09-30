package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/iricardofernandes/jupiter/internal/vault"
)

const maxBody = 8 << 10

var errInvalidRequest = errors.New("vault: invalid request")

// InternalHandler serves Jupiter's API and worker. It must be served only over the mTLS
// listener, which admits only their identities; each route then checks which of them
// may call it.
func (s *Service) InternalHandler() http.Handler {
	mux := http.NewServeMux()
	api := []string{vault.APIIdentity}
	both := []string{vault.APIIdentity, vault.WorkerIdentity}
	handle := func(pattern string, callers []string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if !slices.Contains(callers, callerOf(r)) {
				s.writeError(w, r, vault.WireError{Code: vault.CodeForbidden, Message: "This caller may not use this route."})
				return
			}
			h(w, r)
		})
	}
	handle("POST "+vault.CardsPath, api, func(w http.ResponseWriter, r *http.Request) {
		var body vault.TokenizeBody
		if !s.decode(w, r, &body) {
			return
		}
		c, err := s.Tokenize(r.Context(), vault.TokenizeRequest{Owner: body.Owner, RequestKey: body.RequestKey, Card: body.Card.Data()})
		s.answer(w, r, c, err)
	})
	handle("GET "+vault.CardsPath+"/{token}", both, func(w http.ResponseWriter, r *http.Request) {
		c, err := s.Card(r.Context(), r.PathValue("token"))
		s.answer(w, r, c, err)
	})
	handle("POST "+vault.CardsPath+"/{token}/claim", both, func(w http.ResponseWriter, r *http.Request) {
		var body vault.OwnerBody
		if !s.decode(w, r, &body) {
			return
		}
		c, err := s.Claim(r.Context(), r.PathValue("token"), body.Owner)
		s.answer(w, r, c, err)
	})
	handle("POST "+vault.CardsPath+"/{token}/detokenize", both, func(w http.ResponseWriter, r *http.Request) {
		var body vault.OwnerBody
		if !s.decode(w, r, &body) {
			return
		}
		c, err := s.Detokenize(r.Context(), r.PathValue("token"), body.Owner)
		// The audit trail of who read which card: never the card itself.
		s.cfg.Logger.InfoContext(r.Context(), "card detokenized", "caller", callerOf(r),
			"token", r.PathValue("token"), "owner", body.Owner, "succeeded", err == nil)
		s.answer(w, r, vault.WireCardOf(c), err)
	})
	return s.logged(mux)
}

// callerOf is the identity in the client certificate the TLS handshake verified.
func callerOf(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return ""
	}
	for _, uri := range r.TLS.PeerCertificates[0].URIs {
		if id := uri.String(); id == vault.APIIdentity || id == vault.WorkerIdentity {
			return id
		}
	}
	return ""
}

// Publishable keys are checked for shape only; Jupiter checks that the key is real, and
// whose, when the merchant's server claims the token.
var publishableKey = regexp.MustCompile(`^pk_(test|live)_[0-9A-Za-z]{43}$`)

// PublicHandler serves the one route a web page calls: it takes a card and returns a
// token. CORS is open because merchants embed the form in their own pages; the token is
// worthless until the merchant's server, with its secret key, claims it through Jupiter.
func (s *Service) PublicHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("OPTIONS "+vault.PublicTokensPath, func(w http.ResponseWriter, _ *http.Request) {
		allowCORS(w)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST "+vault.PublicTokensPath, func(w http.ResponseWriter, r *http.Request) {
		allowCORS(w)
		if !s.limiter.allow(r) {
			s.writeError(w, r, vault.WireError{Code: vault.CodeRateLimited, Message: "Too many cards from this address. Slow down."})
			return
		}
		key, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !publishableKey.MatchString(key) {
			s.writeError(w, r, vault.WireError{Code: vault.CodeUnauthorized, Message: "A publishable key is required as a bearer token."})
			return
		}
		var body vault.WireCard
		if !s.decode(w, r, &body) {
			return
		}
		c, err := s.TokenizePublic(r.Context(), key, body.Data())
		s.answer(w, r, vault.PublicToken{
			ID: c.Token, Object: "token", Brand: c.Brand, Last4: c.Last4, ExpMonth: c.ExpMonth, ExpYear: c.ExpYear,
		}, err)
	})
	return s.logged(mux)
}

func allowCORS(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "POST")
	h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	h.Set("Access-Control-Max-Age", "600")
}

func (s *Service) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if err == nil {
		if _, trailing := dec.Token(); !errors.Is(trailing, io.EOF) {
			err = errors.New("trailing data")
		}
	}
	if err != nil {
		s.writeError(w, r, vault.WireError{Code: vault.CodeInvalidRequest, Message: "The body must be a JSON object with the documented fields."})
		return false
	}
	return true
}

func (s *Service) answer(w http.ResponseWriter, r *http.Request, body any, err error) {
	if err != nil {
		s.writeError(w, r, s.wireError(r.Context(), err))
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// wireError never repeats what the caller sent: a message about a card carries only
// which field is wrong.
func (s *Service) wireError(ctx context.Context, err error) vault.WireError {
	var cardErr *vault.CardError
	switch {
	case errors.As(err, &cardErr):
		return vault.WireError{Code: cardErr.Code, Param: cardErr.Param, Message: "The card's " + cardErr.Param + " is invalid."}
	case errors.Is(err, vault.ErrNotFound):
		return vault.WireError{Code: vault.CodeNotFound, Message: "No such card."}
	case errors.Is(err, vault.ErrConflict):
		return vault.WireError{Code: vault.CodeConflict, Message: "This request key was used with another card."}
	case errors.Is(err, errInvalidRequest):
		return vault.WireError{Code: vault.CodeInvalidRequest, Message: strings.TrimPrefix(err.Error(), errInvalidRequest.Error()+": ")}
	default:
		s.cfg.Logger.ErrorContext(ctx, "vault request failed", "error", err)
		return vault.WireError{Code: vault.CodeInternal, Message: "The vault failed to process the request."}
	}
}

func (s *Service) writeError(w http.ResponseWriter, _ *http.Request, e vault.WireError) {
	writeJSON(w, vault.StatusOf(e.Code), vault.ErrorBody{Error: e})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(status int) {
	s.status = status
	s.ResponseWriter.WriteHeader(status)
}

// logged records each request's method, route and status, and nothing of its body.
func (s *Service) logged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		began := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.cfg.Logger.InfoContext(r.Context(), "request", "method", r.Method, "path", r.URL.Path,
			"status", rec.status, "duration_ms", time.Since(began).Milliseconds())
	})
}
