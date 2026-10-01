package cardnetwork

import (
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/iricardofernandes/jupiter/pkg/cardnet"
	"github.com/iricardofernandes/jupiter/pkg/threeds"
)

const (
	defaultCreditLimit  = 10_000_000 // R$ 100,000.00 per card
	defaultStandInLimit = 50_000     // R$ 500.00
)

type holdStatus string

const (
	held      holdStatus = "held"
	reversed  holdStatus = "reversed"
	completed holdStatus = "completed"
)

// authorization is what the issuer remembers of one it approved.
type authorization struct {
	NTI          string
	PAN          string
	RRN          string
	AuthCode     string
	AcquirerID   string
	MerchantID   string
	Amount       int64
	Completed    int64
	Refunded     int64
	Installments int
	Status       holdStatus
	StandIn      bool
	ViaToken     bool
	// Authenticated says 3-D Secure vouched for the cardholder: fraud disputes then fall
	// on the issuer.
	Authenticated bool
	At            time.Time
	Disputed      int64
}

type refund struct {
	record   cardnet.ClearingRecord
	auth     *authorization
	reversed bool
}

// issuer is the issuers behind the network, as one: card accounts, holds, and the
// day's clearing records. All of it is guarded by mu.
type issuer struct {
	mu           sync.Mutex
	now          func() time.Time
	creditLimit  int64
	standInLimit int64
	used         map[string]int64
	byNTI        map[string]*authorization
	byOriginal   map[string]*authorization
	refunds      map[string]*refund
	pending      map[string][]cardnet.ClearingRecord
	acquirers    map[string]bool
	sequence     int64
	authKey      []byte
}

func newIssuer(now func() time.Time, authKey []byte) *issuer {
	return &issuer{
		authKey: authKey, now: now, creditLimit: defaultCreditLimit, standInLimit: defaultStandInLimit,
		used: map[string]int64{}, byNTI: map[string]*authorization{}, byOriginal: map[string]*authorization{},
		refunds: map[string]*refund{}, pending: map[string][]cardnet.ClearingRecord{}, acquirers: map[string]bool{},
	}
}

func originalKey(o cardnet.OriginalData) string {
	return o.MTI + "/" + o.STAN + "/" + o.TransmissionDateTime + "/" + o.AcquirerID
}

func answer(req cardnet.Message, rc string) cardnet.Message {
	return cardnet.Message{
		MTI: cardnet.ResponseTo(req.MTI), ProcessingCode: req.ProcessingCode, Amount: req.Amount,
		TransmissionDateTime: req.TransmissionDateTime, STAN: req.STAN, AcquirerID: req.AcquirerID, RRN: req.RRN,
		ResponseCode: rc, TerminalID: req.TerminalID, MerchantID: req.MerchantID, Currency: req.Currency,
	}
}

// signOn remembers an acquirer, which from then on gets a clearing file every day.
func (s *issuer) signOn(acquirer string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acquirers[cardnet.PadAcquirer(acquirer)] = true
}

// authorize decides a 0100, or the authorization half of a 0200 purchase.
func (s *issuer) authorize(req cardnet.Message) cardnet.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, reversedEarly := s.byOriginal[originalKey(cardnet.Original(req))]; reversedEarly {
		return answer(req, cardnet.InvalidTransaction)
	}
	b := behaviourOf(req.PAN)
	if rc := s.check(req, b); rc != cardnet.Approved {
		return answer(req, rc)
	}
	amount, rc := req.Amount, cardnet.Approved
	if b == partialApproval && req.Private != nil && req.Private.PartialApproval == "1" {
		amount, rc = max(1, req.Amount/2), cardnet.PartiallyApproved
	}
	standIn := b == issuerDown
	switch {
	case standIn && amount > s.standInLimit:
		return answer(req, cardnet.IssuerUnavailable)
	case s.used[req.PAN]+amount > s.creditLimit:
		return answer(req, cardnet.InsufficientFunds)
	}
	s.sequence++
	a := &authorization{
		NTI: fmt.Sprintf("8%014d", s.sequence), PAN: req.PAN, RRN: req.RRN, AuthCode: fmt.Sprintf("A%05d", s.sequence%100000),
		AcquirerID: cardnet.PadAcquirer(req.AcquirerID), MerchantID: req.MerchantID, Amount: amount, Status: held, StandIn: standIn,
		ViaToken:      req.Private != nil && req.Private.TokenCryptogram != "",
		Authenticated: req.Private != nil && req.Private.AuthenticationValue != "",
		At:            s.now().UTC(),
	}
	if n, err := strconv.Atoi(req.Installments); err == nil {
		a.Installments = n
	}
	s.used[req.PAN] += amount
	s.byNTI[a.NTI] = a
	s.byOriginal[originalKey(cardnet.Original(req))] = a
	resp := answer(req, rc)
	resp.Amount, resp.AuthorizationCode = amount, a.AuthCode
	resp.Private = &cardnet.PrivateData{NetworkTransactionID: a.NTI}
	if standIn {
		resp.Private.StandIn = "1"
	}
	return resp
}

func (s *issuer) check(req cardnet.Message, b behaviour) string {
	switch {
	case !luhn(req.PAN):
		return cardnet.InvalidCardNumber
	case req.Amount <= 0:
		return cardnet.InvalidAmount
	case req.Currency != cardnet.CurrencyBRL:
		return cardnet.FormatError
	case expired(req.Expiry, s.now()):
		return cardnet.ExpiredCard
	case responseFor(b) != cardnet.Approved:
		return responseFor(b)
	}
	if req.Private != nil && req.Private.AuthenticationValue != "" &&
		req.Private.AuthenticationValue != threeds.AuthenticationValue(s.authKey, req.PAN, req.Amount, req.Private.DSTransID) {
		// An authentication value that does not check out is treated as fraud.
		return cardnet.DoNotHonour
	}
	if req.Private != nil && req.Private.StoredCredential == cardnet.StoredCredentialMerchant {
		first, ok := s.byNTI[req.Private.NetworkTransactionID]
		if !ok || first.PAN != req.PAN {
			return cardnet.NotPermittedToCardholder
		}
	}
	return cardnet.Approved
}

func expired(yymm string, now time.Time) bool {
	if len(yymm) != 4 {
		return true
	}
	year, errY := strconv.Atoi(yymm[:2])
	month, errM := strconv.Atoi(yymm[2:])
	if errY != nil || errM != nil || month < 1 || month > 12 {
		return true
	}
	endOfMonth := time.Date(2000+year, time.Month(month)+1, 1, 0, 0, 0, 0, time.UTC)
	return !now.Before(endOfMonth)
}

// complete applies a completion advice (0220, 0221): the amount the acquirer captured,
// which goes into the day's clearing. A repeat of one already applied is acknowledged.
func (s *issuer) complete(req cardnet.Message) cardnet.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.byNTI[req.NetworkTransactionID()]
	switch {
	case !ok || a.Status == reversed:
		return answer(req, cardnet.InvalidTransaction)
	case a.Status == completed && a.Completed == req.Amount:
		return answer(req, cardnet.Approved)
	case a.Status == completed:
		return answer(req, cardnet.InvalidTransaction)
	case req.Amount <= 0 || req.Amount > a.Amount:
		return answer(req, cardnet.InvalidAmount)
	}
	a.Status, a.Completed = completed, req.Amount
	s.used[a.PAN] -= a.Amount - req.Amount
	s.pending[a.AcquirerID] = append(s.pending[a.AcquirerID], cardnet.ClearingRecord{
		Kind: cardnet.ClearingCompletion, RRN: req.RRN, NetworkTransactionID: a.NTI, Amount: req.Amount,
		Currency: cardnet.CurrencyBRL, Installments: a.Installments, AuthorizationCode: a.AuthCode, MerchantID: a.MerchantID,
	})
	return answer(req, cardnet.Approved)
}

// refund applies a 0200 credit against a completed authorization.
func (s *issuer) refund(req cardnet.Message) cardnet.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.byNTI[req.NetworkTransactionID()]
	switch {
	case !ok || a.Status != completed:
		return answer(req, cardnet.InvalidTransaction)
	case req.Amount <= 0 || a.Refunded+req.Amount > a.Completed:
		return answer(req, cardnet.InvalidAmount)
	}
	key := originalKey(cardnet.Original(req))
	if r, done := s.refunds[key]; done && !r.reversed {
		return answer(req, cardnet.Approved)
	}
	a.Refunded += req.Amount
	s.used[a.PAN] -= req.Amount
	record := cardnet.ClearingRecord{
		Kind: cardnet.ClearingRefund, RRN: req.RRN, NetworkTransactionID: a.NTI, Amount: req.Amount,
		Currency: cardnet.CurrencyBRL, Installments: a.Installments, MerchantID: a.MerchantID,
	}
	s.refunds[key] = &refund{record: record, auth: a}
	s.pending[a.AcquirerID] = append(s.pending[a.AcquirerID], record)
	return answer(req, cardnet.Approved)
}

// reverse applies a reversal (0400, 0420, 0421) of the message DE 90 names. Reversing
// something the issuer never saw, or already reversed, is acknowledged: the acquirer
// reverses whatever it cannot be sure did not happen.
func (s *issuer) reverse(req cardnet.Message) cardnet.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	original, err := cardnet.ParseOriginalData(req.OriginalData)
	if err != nil {
		return answer(req, cardnet.FormatError)
	}
	key := originalKey(original)
	if r, ok := s.refunds[key]; ok {
		return answer(req, s.reverseRefund(r))
	}
	a, ok := s.byOriginal[key]
	switch {
	case !ok:
		// Reversed before it arrived: the original, should it still come, is refused.
		s.byOriginal[key] = &authorization{Status: reversed}
		return answer(req, cardnet.Approved)
	case a.Status == reversed:
		return answer(req, cardnet.Approved)
	case a.Status == completed && req.MTI == cardnet.ReversalRequest:
		return answer(req, cardnet.InvalidTransaction)
	case a.Status == completed:
		// An advice is acknowledged, but a completed payment is no longer reversible.
		return answer(req, cardnet.Approved)
	}
	a.Status = reversed
	s.used[a.PAN] -= a.Amount
	return answer(req, cardnet.Approved)
}

func (s *issuer) reverseRefund(r *refund) string {
	if r.reversed {
		return cardnet.Approved
	}
	records := s.pending[r.auth.AcquirerID]
	for i, rec := range records {
		if rec == r.record {
			s.pending[r.auth.AcquirerID] = append(records[:i:i], records[i+1:]...)
			r.reversed = true
			r.auth.Refunded -= r.record.Amount
			s.used[r.auth.PAN] += r.record.Amount
			return cardnet.Approved
		}
	}
	return cardnet.InvalidTransaction // already cleared
}

func (s *issuer) acquirersSeen() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]bool, len(s.acquirers))
	for a := range s.acquirers {
		out[a] = true
	}
	return out
}

// closeDay takes every acquirer's pending records into that day's files.
func (s *issuer) closeDay(date time.Time) map[string]cardnet.ClearingFile {
	s.mu.Lock()
	defer s.mu.Unlock()
	files := map[string]cardnet.ClearingFile{}
	for acquirer := range s.acquirers {
		files[acquirer] = cardnet.ClearingFile{BusinessDate: date, AcquirerID: acquirer, Records: s.pending[acquirer]}
	}
	s.pending = map[string][]cardnet.ClearingRecord{}
	return files
}

// Hold is an authorization still holding the cardholder's credit.
type Hold struct {
	NetworkTransactionID string
	RRN                  string
	Amount               int64
	StandIn              bool
}

func (s *issuer) holds() []Hold {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Hold
	for _, a := range s.byNTI {
		if a.Status == held {
			out = append(out, Hold{NetworkTransactionID: a.NTI, RRN: a.RRN, Amount: a.Amount, StandIn: a.StandIn})
		}
	}
	return out
}

// Completion is an authorization the acquirer completed.
type Completion struct {
	NetworkTransactionID string
	Authorized           int64
	Completed            int64
	Refunded             int64
	ViaToken             bool
	PAN                  string
}

func (s *issuer) completions() []Completion {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Completion
	for _, a := range s.byNTI {
		if a.Status == completed {
			out = append(out, Completion{
				NetworkTransactionID: a.NTI, Authorized: a.Amount, Completed: a.Completed, Refunded: a.Refunded, ViaToken: a.ViaToken, PAN: a.PAN,
			})
		}
	}
	return out
}
