package authentication

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/iricardofernandes/jupiter/internal/authentication/db"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/pkg/threeds"
)

// The 3DS server's routes, beside the API on Jupiter's public address. The first two
// are for the customer's browser, the last for the directory server; none takes an API
// key.
const (
	challengePath = "/3ds/v1/challenge/"
	notifyPath    = "/3ds/v1/notify/"
	resultsPath   = "/3ds/v1/results"
)

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+challengePath+"{id}", s.challenge)
	mux.HandleFunc("POST "+notifyPath+"{id}", s.notify)
	mux.HandleFunc("POST "+resultsPath, s.results)
	return mux
}

var challengePage = template.Must(template.New("challenge").Parse(`<!doctype html>
<html lang="pt-BR"><head><meta charset="utf-8"><title>Autenticação</title></head>
<body onload="document.forms[0].submit()">
<form method="post" action="{{.ACSURL}}">
<input type="hidden" name="creq" value="{{.CReq}}">
<input type="hidden" name="threeDSSessionData" value="{{.SessionData}}">
<noscript><button type="submit">Continuar para o banco</button></noscript>
</form>
</body></html>`))

// challenge sends the customer's browser to the issuer's ACS with the CReq.
func (s *Server) challenge(w http.ResponseWriter, r *http.Request) {
	tx, err := db.New(s.cfg.Pool).GetTransaction(r.Context(), r.PathValue("id"))
	if err != nil || tx.TransStatus != threeds.StatusChallenge || tx.ResultStatus != "" {
		http.Error(w, "No challenge is waiting here.", http.StatusNotFound)
		return
	}
	acs, err := url.Parse(tx.AcsUrl)
	if err != nil || !s.acceptableACS(tx.AcsUrl) {
		http.Error(w, "No challenge is waiting here.", http.StatusNotFound)
		return
	}
	creq, err := threeds.EncodeForm(threeds.CReq{
		MessageType: threeds.TypeCReq, MessageVersion: threeds.Version, ThreeDSServerTransID: tx.ServerTransID,
		ACSTransID: tx.AcsTransID, ChallengeWindowSize: "05",
	})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	pageHeaders(w)
	w.Header().Set("Content-Security-Policy",
		fmt.Sprintf("default-src 'none'; script-src 'unsafe-inline'; form-action %s://%s; frame-ancestors 'none'; base-uri 'none'", acs.Scheme, acs.Host))
	_ = challengePage.Execute(w, map[string]any{"ACSURL": tx.AcsUrl, "CReq": creq, "SessionData": tx.ServerTransID})
}

func pageHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

var donePage = template.Must(template.New("done").Parse(`<!doctype html>
<html lang="pt-BR"><head><meta charset="utf-8"><title>Autenticação concluída</title></head>
<body><p>Autenticação concluída. Você já pode voltar à loja.</p></body></html>`))

// notify is where the ACS sends the browser back, with the CRes. The result that counts
// is the directory server's RReq; this only takes the customer back to the merchant.
func (s *Server) notify(w http.ResponseWriter, r *http.Request) {
	tx, err := db.New(s.cfg.Pool).GetTransaction(r.Context(), r.PathValue("id"))
	if err != nil || tx.TransStatus != threeds.StatusChallenge {
		http.Error(w, "Unknown authentication.", http.StatusNotFound)
		return
	}
	var cres threeds.CRes
	if err := threeds.DecodeForm(r.PostFormValue("cres"), &cres); err != nil || cres.ThreeDSServerTransID != tx.ServerTransID {
		http.Error(w, "Invalid CRes.", http.StatusBadRequest)
		return
	}
	// The return URL was checked when the merchant sent it; this is a second look.
	if back, err := url.Parse(tx.ReturnUrl); err == nil && back.Scheme == "https" && back.Host != "" && back.User == nil {
		query := back.Query()
		query.Set("payment_intent", tx.IntentID)
		back.RawQuery = query.Encode()
		http.Redirect(w, r, back.String(), http.StatusSeeOther)
		return
	}
	pageHeaders(w)
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	_ = donePage.Execute(w, nil)
}

// results takes a challenge's result from the directory server, answers it, and resumes
// the payment.
func (s *Server) results(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		http.Error(w, "unreadable", http.StatusBadRequest)
		return
	}
	if err := threeds.Verify(body, r.Header.Get(threeds.SignatureHeader), s.cfg.ResultsSecret, resultsTolerance, s.cfg.Now()); err != nil {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	var rreq threeds.RReq
	if err := json.Unmarshal(body, &rreq); err != nil || rreq.MessageType != threeds.TypeRReq {
		http.Error(w, "not an RReq", http.StatusBadRequest)
		return
	}
	if !validResult(rreq) {
		http.Error(w, "not a challenge result", http.StatusBadRequest)
		return
	}
	owner, intentID, resume, err := s.applyResults(r.Context(), rreq)
	switch {
	case errors.Is(err, errUnknownTransaction):
		http.Error(w, "unknown transaction", http.StatusBadRequest)
		return
	case err != nil:
		// The directory server repeats a result that was not answered.
		s.cfg.Logger.ErrorContext(r.Context(), "applying 3-D Secure results", "transaction", rreq.ThreeDSServerTransID, "error", err)
		http.Error(w, "results not applied", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(threeds.RRes{
		MessageType: threeds.TypeRRes, MessageVersion: threeds.Version, ThreeDSServerTransID: rreq.ThreeDSServerTransID,
		DSTransID: rreq.DSTransID, ACSTransID: rreq.ACSTransID, ResultsStatus: "01",
	})
	if resume && s.resume != nil {
		// The directory server should not wait on the card network. If this stops
		// half-way, the resolver takes the attempt up.
		go func() { //nolint:contextcheck // detached on purpose: the directory server's request is over
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), resumeTimeout)
			defer cancel()
			if err := s.resume(ctx, owner, intentID); err != nil {
				s.cfg.Logger.ErrorContext(ctx, "resuming a payment after 3-D Secure", "payment_intent", intentID, "error", err)
			}
		}()
	}
}

var errUnknownTransaction = errors.New("no such transaction")

// validResult reports whether an RReq carries a challenge's final result: authenticated
// or attempted, with the value that proves it, or refused.
func validResult(rreq threeds.RReq) bool {
	switch rreq.TransStatus {
	case threeds.StatusAuthenticated, threeds.StatusAttempted:
		return rreq.AuthenticationValue != "" && rreq.ECI != ""
	case threeds.StatusNotAuthenticated, threeds.StatusRejected:
		return true
	}
	return false
}

// applyResults records a challenge's result once, and hands it to payments.
func (s *Server) applyResults(ctx context.Context, rreq threeds.RReq) (payments.Owner, string, bool, error) {
	var owner payments.Owner
	var intentID string
	resume := false
	err := postgres.InTx(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		row, err := q.GetTransaction(ctx, rreq.ThreeDSServerTransID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (row.TransStatus != threeds.StatusChallenge || row.DsTransID != rreq.DSTransID || row.AcsTransID != rreq.ACSTransID)) {
			return errUnknownTransaction
		}
		if err != nil {
			return err
		}
		if owner, err = ownerOf(row); err != nil {
			return err
		}
		intentID = row.IntentID
		n, err := q.RecordResult(ctx, db.RecordResultParams{ResultStatus: rreq.TransStatus, ServerTransID: row.ServerTransID, Now: pgtype.Timestamptz{Time: s.cfg.Now().UTC(), Valid: true}})
		if err != nil || n == 0 {
			return err // a repeat of results already applied
		}
		row.TransStatus, row.Eci, row.AuthenticationValue = rreq.TransStatus, rreq.ECI, rreq.AuthenticationValue
		if s.payments == nil {
			return errors.New("authentication: not attached to payments")
		}
		_, step, err := s.payments.CompleteChallenge(ctx, tx, row.ServerTransID, s.authentication(row))
		resume = step == payments.StepConfirm
		return err
	})
	return owner, intentID, resume, err
}
