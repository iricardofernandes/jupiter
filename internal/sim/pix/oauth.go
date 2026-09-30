package pix

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/iricardofernandes/jupiter/pkg/pixapi"
)

// The authorization server: OAuth 2.0 client credentials over mutual TLS, issuing
// certificate-bound access tokens (RFC 8705 §3). A token is good only on a connection
// made with the certificate it was issued to, which the resource server checks on every
// request, as the Manual de Padrões requires of PSPs.

type token struct {
	client     string
	scopes     []string
	thumbprint string
	expires    time.Time
}

var allScopes = []string{
	pixapi.ScopeCobWrite, pixapi.ScopeCobRead, pixapi.ScopeCobVWrite, pixapi.ScopeCobVRead,
	pixapi.ScopePixWrite, pixapi.ScopePixRead, pixapi.ScopeWebhookWrite, pixapi.ScopeWebhookRead,
	pixapi.ScopeRecWrite, pixapi.ScopeRecRead, pixapi.ScopeSolicRecWrite, pixapi.ScopeSolicRecRead,
	pixapi.ScopeCobRWrite, pixapi.ScopeCobRRead, pixapi.ScopeWebhookRecWrite, pixapi.ScopeWebhookRecRead,
	pixapi.ScopeWebhookCobRWrite, pixapi.ScopeWebhookCobRRead, pixapi.ScopePayloadLocationRecWrite, pixapi.ScopePayloadLocationRecRead,
}

// thumbprint is the certificate's x5t#S256: the base64url SHA-256 of its DER encoding.
func thumbprint(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
		return ""
	}
	sum := sha256.Sum256(r.TLS.PeerCertificates[0].Raw)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (s *Sim) issueToken(w http.ResponseWriter, r *http.Request) {
	bound := thumbprint(r)
	if bound == "" {
		oauthError(w, http.StatusUnauthorized, "invalid_client", "a client certificate is required")
		return
	}
	// RFC 6749 §2.3.1: the client id and secret are form-encoded before Basic encoding.
	rawID, rawSecret, ok := r.BasicAuth()
	id, errID := url.QueryUnescape(rawID)
	secret, errSecret := url.QueryUnescape(rawSecret)
	ok = ok && errID == nil && errSecret == nil
	s.mu.Lock()
	client, known := s.clients[id]
	s.mu.Unlock()
	if !ok || !known || subtle.ConstantTimeCompare([]byte(secret), []byte(client.Secret)) != 1 {
		oauthError(w, http.StatusUnauthorized, "invalid_client", "unknown client or wrong secret")
		return
	}
	if r.PostFormValue("grant_type") != "client_credentials" {
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "only client_credentials")
		return
	}
	scopes := allScopes
	if requested := strings.Fields(r.PostFormValue("scope")); len(requested) > 0 {
		for _, sc := range requested {
			if !slices.Contains(allScopes, sc) {
				oauthError(w, http.StatusBadRequest, "invalid_scope", "unknown scope "+sc)
				return
			}
		}
		scopes = requested
	}
	access := randomAlnum(40)
	s.mu.Lock()
	s.tokens[access] = token{client: id, scopes: scopes, thumbprint: bound, expires: s.now().Add(s.cfg.TokenTTL)}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": access, "token_type": "Bearer", "expires_in": int(s.cfg.TokenTTL.Seconds()),
		"scope": strings.Join(scopes, " "),
	})
}

func oauthError(w http.ResponseWriter, status int, code, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": description})
}

type clientKey struct{}

// authorized wraps an API route: a bearer token, unexpired, presented on a connection
// made with the certificate it is bound to, carrying scope.
func (s *Sim) authorized(scope string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		s.mu.Lock()
		t, known := s.tokens[bearer]
		s.mu.Unlock()
		presented := thumbprint(r)
		switch {
		case !ok || !known || !s.now().Before(t.expires):
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			writeProblem(w, http.StatusUnauthorized, "AcessoNegado", "Acesso Negado", "Token ausente, inválido ou expirado.")
			return
		case presented == "" || subtle.ConstantTimeCompare([]byte(presented), []byte(t.thumbprint)) != 1:
			// RFC 8705 §3: a token presented with another certificate is refused.
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			writeProblem(w, http.StatusUnauthorized, "AcessoNegado", "Acesso Negado", "O token não pertence ao certificado desta conexão.")
			return
		case !slices.Contains(t.scopes, scope):
			w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope", scope="`+scope+`"`)
			writeProblem(w, http.StatusForbidden, "AcessoNegado", "Acesso Negado", "O token não tem o escopo "+scope+".")
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), clientKey{}, t.client)))
	}
}

func clientOf(r *http.Request) string {
	id, _ := r.Context().Value(clientKey{}).(string)
	return id
}

// writeProblem answers with an RFC 7807 problem, typed as the API Pix types them.
func writeProblem(w http.ResponseWriter, status int, kind, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(pixapi.Problema{Type: pixapi.ErrorType(kind), Title: title, Status: status, Detail: &detail})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
