package pix

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"time"

	"github.com/iricardofernandes/jupiter/pkg/pixapi"
)

// Payload locations: the URL a dynamic BR Code carries, which a payer fetches over HTTPS
// to learn what to pay. The answer is the charge's payload as a JWS (RFC 7515, compact
// serialization, content type application/jose), signed by the receiving PSP. The
// Manual de Segurança do SFN, which fixes the algorithm and headers, was not available to
// this project: the simulator signs with PS256 and names its key set in a jku header on
// the same host, which the payer checks before trusting the payload.

const (
	jwsAlgorithm = "PS256"
	jwksPath     = "/jwks"
)

func (s *Sim) locationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /qr/v2/cobv/{token}", func(w http.ResponseWriter, r *http.Request) {
		s.servePayload(w, r, true)
	})
	mux.HandleFunc("GET /qr/v2/{token}", func(w http.ResponseWriter, r *http.Request) {
		s.servePayload(w, r, false)
	})
	mux.HandleFunc("GET "+jwksPath, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"keys": []map[string]string{s.jwk()}})
	})
}

func (s *Sim) jwk() map[string]string {
	pub := s.signing.PublicKey
	return map[string]string{
		"kty": "RSA", "use": "sig", "alg": jwsAlgorithm, "kid": s.keyID,
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}

// servePayload answers a payer's GET of a location. A charge that is no longer payable,
// paid, removed or expired, is gone (410), as the Manual de Padrões §2.7.3 allows. For a
// cobv the amount depends on the date the payer means to pay (DPP), today if absent.
func (s *Sim) servePayload(w http.ResponseWriter, r *http.Request, due bool) {
	now := s.now()
	date := dateOf(now)
	if dpp := r.URL.Query().Get("DPP"); dpp != "" {
		d, err := time.ParseInLocation(time.DateOnly, dpp, brasilia)
		if err != nil || d.Before(date) {
			problemf(w, http.StatusBadRequest, "CobPayloadOperacaoInvalida", "DPP inválida.", "DPP deve ser uma data a partir de hoje.")
			return
		}
		date = d
	}
	s.mu.Lock()
	ch := s.locations[r.PathValue("token")]
	var payload any
	var gone bool
	if ch != nil && ch.due == due {
		gone = ch.status != statusActive || ch.expired(now)
		if !gone {
			payload = s.payloadLocked(ch, date)
		}
	}
	s.mu.Unlock()
	switch {
	case ch == nil || ch.due != due:
		problemf(w, http.StatusNotFound, "CobPayloadNaoEncontrado", "Payload não encontrado.", "Nenhuma cobrança neste endereço.")
		return
	case gone:
		problemf(w, http.StatusGone, "CobPayloadNaoEncontrado", "Cobrança indisponível.", "A cobrança foi concluída, removida ou expirou.")
		return
	}
	jws, err := s.sign(payload)
	if err != nil {
		problemf(w, http.StatusInternalServerError, "ErroInternoDoServidor", "Erro interno.", "Falha ao assinar o payload.")
		return
	}
	w.Header().Set("Content-Type", "application/jose")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(jws)) //nolint:gosec // a signed JSON payload served as application/jose, never rendered as HTML
}

func (s *Sim) payloadLocked(ch *charge, date time.Time) any {
	now := s.now()
	var debtor []byte
	if ch.debtor != nil {
		debtor, _ = json.Marshal(ch.debtor)
	}
	if !ch.due {
		out := pixapi.CobPayload{
			Chave: ch.key, Txid: ch.txid, Revisao: pixapi.Ptr(ch.revisao), Status: pixapi.CobStatus(ch.status),
			Valor: pixapi.CobPayloadValor{Original: pixapi.FormatValor(ch.original)},
		}
		out.Calendario.Criacao, out.Calendario.Apresentacao, out.Calendario.Expiracao = ch.created, now, ch.expiracao
		if ch.changeable {
			out.Valor.ModalidadeAlteracao = pixapi.Ptr(int32(1))
		}
		if ch.request != "" {
			out.SolicitacaoPagador = pixapi.Ptr(ch.request)
		}
		if debtor != nil {
			_ = pixapi.New(&out.Devedor).UnmarshalJSON(debtor)
		}
		return out
	}
	c := ch.amountOn(date)
	out := pixapi.CobVPayload{
		Chave: ch.key, Txid: ch.txid, Revisao: pixapi.Ptr(ch.revisao), Status: pixapi.CobVStatus(ch.status),
		Recebedor: *s.receiver(ch.client),
		Valor:     pixapi.CobVPayloadValor{Original: pixapi.Ptr(pixapi.FormatValor(c.original)), Final: pixapi.FormatValor(c.total())},
	}
	out.Calendario.Criacao, out.Calendario.Apresentacao, out.Calendario.ValidadeAposVencimento = ch.created, now, ch.validity
	_ = out.Calendario.DataDeVencimento.UnmarshalText([]byte(ch.dueDate))
	for field, v := range map[**string]int64{&out.Valor.Multa: c.fine, &out.Valor.Juros: c.interest, &out.Valor.Desconto: c.discount, &out.Valor.Abatimento: c.abatement} {
		if v > 0 {
			*field = pixapi.Ptr(pixapi.FormatValor(v))
		}
	}
	if ch.request != "" {
		out.SolicitacaoPagador = pixapi.Ptr(ch.request)
	}
	_ = out.Devedor.UnmarshalJSON(debtor)
	return out
}

// sign makes the compact JWS of a payload.
func (s *Sim) sign(payload any) (string, error) {
	header, _ := json.Marshal(map[string]string{
		"alg": jwsAlgorithm, "typ": "JOSE", "kid": s.keyID, "jku": "https://" + s.cfg.Host + jwksPath,
	})
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	digest := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPSS(rand.Reader, s.signing, crypto.SHA256, digest[:], &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
	if err != nil {
		return "", err
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

var errSignature = errors.New("the payload's signature does not check out")

// verifyJWS checks a compact JWS against a key set and returns its payload. The key set
// must be the one the header names: payers fetch it from the jku, on the location's host.
func verifyJWS(jws string, keys []map[string]string) ([]byte, map[string]string, error) {
	parts := splitJWS(jws)
	if parts == nil {
		return nil, nil, errSignature
	}
	rawHeader, err1 := base64.RawURLEncoding.DecodeString(parts[0])
	body, err2 := base64.RawURLEncoding.DecodeString(parts[1])
	sig, err3 := base64.RawURLEncoding.DecodeString(parts[2])
	var header map[string]string
	if err1 != nil || err2 != nil || err3 != nil || json.Unmarshal(rawHeader, &header) != nil || header["alg"] != jwsAlgorithm {
		return nil, nil, errSignature
	}
	for _, k := range keys {
		if k["kid"] != header["kid"] || k["kty"] != "RSA" {
			continue
		}
		n, errN := base64.RawURLEncoding.DecodeString(k["n"])
		e, errE := base64.RawURLEncoding.DecodeString(k["e"])
		if errN != nil || errE != nil {
			return nil, nil, fmt.Errorf("%w: a malformed key", errSignature)
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		if pub.N.BitLen() < 2048 || pub.E != 65537 {
			return nil, nil, fmt.Errorf("%w: a weak key", errSignature)
		}
		digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		if rsa.VerifyPSS(pub, crypto.SHA256, digest[:], sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash}) != nil {
			return nil, nil, errSignature
		}
		return body, header, nil
	}
	return nil, nil, fmt.Errorf("%w: no key %q", errSignature, header["kid"])
}

func splitJWS(jws string) []string {
	var parts []string
	start := 0
	for i := range len(jws) {
		if jws[i] == '.' {
			parts = append(parts, jws[start:i])
			start = i + 1
		}
	}
	parts = append(parts, jws[start:])
	if len(parts) != 3 {
		return nil
	}
	return parts
}
