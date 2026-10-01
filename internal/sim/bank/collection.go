package bank

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/iricardofernandes/jupiter/pkg/bizday"
	"github.com/iricardofernandes/jupiter/pkg/boleto"
	"github.com/iricardofernandes/jupiter/pkg/brcode"
	"github.com/iricardofernandes/jupiter/pkg/cnab240"
	"github.com/iricardofernandes/jupiter/pkg/taxid"
)

var (
	ErrInvalid  = errors.New("bank: invalid request")
	ErrNotFound = errors.New("bank: not found")
	ErrConflict = errors.New("bank: conflict")
)

type titleStatus string

const (
	registered titleStatus = "registered"
	paid       titleStatus = "paid"
	writtenOff titleStatus = "written_off"
)

type title struct {
	p        cnab240.SegmentP
	q        cnab240.SegmentQ
	status   titleStatus
	barcode  boleto.Barcode
	location string // the Pix QR code's location, for a hybrid boleto
	txid     string
}

// Remit takes a collection remittance. A remittance is named by its sequence (NSA): the
// same one again changes nothing, another with the same number is refused.
func (s *Sim) Remit(token string, data []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.byToken[token]
	if c == nil {
		return 0, fmt.Errorf("%w: unknown client", ErrNotFound)
	}
	f, err := cnab240.ParseRemittance(data)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if f.Header.Bank != Code || f.Header.Doc != fmt.Sprintf("%014s", c.TaxID) {
		return 0, fmt.Errorf("%w: the remittance is not for this bank or this client", ErrInvalid)
	}
	if seen, ok := c.remittances[f.Header.Sequence]; ok {
		if !bytes.Equal(seen, data) {
			return 0, fmt.Errorf("%w: remittance %d was another", ErrConflict, f.Header.Sequence)
		}
		return 0, nil
	}
	c.remittances[f.Header.Sequence] = bytes.Clone(data)
	n := 0
	for _, b := range f.Batches {
		for _, t := range b.Titles {
			c.pending = append(c.pending, s.instruct(c, t))
			n++
		}
	}
	return n, nil
}

// instruct carries out one title's movement and answers with its occurrence.
func (s *Sim) instruct(c *client, t cnab240.RemittanceTitle) cnab240.ReturnTitle {
	switch t.P.Movement { //nolint:exhaustive // the other instructions are refused below
	case cnab240.Entry:
		return s.register(c, t)
	case cnab240.WriteOffRequest:
		ti := c.titles[t.P.OurNumber]
		if ti == nil || ti.status != registered {
			return s.answer(c, t.P, t.Q, cnab240.InstructionRejected, cnab240.InvalidOurNumber)
		}
		ti.status = writtenOff
		return s.answer(c, ti.p, ti.q, cnab240.WrittenOff, cnab240.WrittenOffByFile)
	}
	return s.answer(c, t.P, t.Q, cnab240.InstructionRejected, cnab240.InvalidMovement)
}

// register checks a new title and registers it, with a Pix QR code when it is hybrid.
func (s *Sim) register(c *client, t cnab240.RemittanceTitle) cnab240.ReturnTitle {
	if reason := s.check(c, t); reason != "" {
		return s.answer(c, t.P, t.Q, cnab240.EntryRejected, reason)
	}
	free, _ := s.profile.FreeField(c.Account, t.P.OurNumber)
	factor, _ := boleto.Factor(t.P.Due)
	barcode, _ := boleto.Boleto{Bank: Code, Currency: boleto.Real, Factor: factor, Amount: t.P.Amount, FreeField: free}.Barcode()
	ti := &title{p: t.P, q: t.Q, status: registered, barcode: barcode}
	c.titles[t.P.OurNumber] = ti
	ret := s.answer(c, t.P, t.Q, cnab240.EntryConfirmed)
	if t.P.Distribution == "P" || t.P.Distribution == "Q" || t.Y03 != nil {
		token := randomToken()
		ti.location = s.cfg.Host + "/qr/v2/cobv/" + token
		ti.txid = strings.ToUpper(token)
		if t.Y03 != nil && t.Y03.TxID != "" {
			ti.txid = t.Y03.TxID
		}
		c.byLocation[ti.location] = ti
		ret.T.Reasons[0] = cnab240.RegisteredWithPix
		ret.Y03 = &cnab240.SegmentY03{Movement: cnab240.PixQRCodeMaintenance, Key: ti.location, TxID: ti.txid}
	}
	return ret
}

func (s *Sim) check(c *client, t cnab240.RemittanceTitle) cnab240.Reason {
	want, _ := s.profile.OurNumber(sequenceOf(t.P.OurNumber))
	switch {
	case want != t.P.OurNumber:
		return cnab240.InvalidOurNumber
	case c.titles[t.P.OurNumber] != nil:
		return cnab240.DuplicateOurNumber
	case t.P.Due.IsZero():
		return cnab240.InvalidDueDate
	case t.P.Due.Before(s.today()):
		return cnab240.PastDueDate
	case t.P.Amount <= 0 || t.P.Amount > boleto.MaxAmount:
		return cnab240.InvalidAmount
	case !validDoc(t.Q.Payer):
		return cnab240.InvalidPayerDoc
	case strings.TrimSpace(t.Q.Payer.Name) == "":
		return cnab240.InvalidPayerName
	}
	if _, err := boleto.Factor(t.P.Due); err != nil {
		return cnab240.InvalidDueDate
	}
	return ""
}

// validDoc checks a payer's CPF or CNPJ, right-aligned in its 15 positions.
func validDoc(p cnab240.Party) bool {
	n := 14
	if p.DocType == cnab240.CPF {
		n = 11
	}
	if p.DocType != cnab240.CPF && p.DocType != cnab240.CNPJ || len(p.Doc) < n {
		return false
	}
	cut := len(p.Doc) - n
	return strings.Trim(p.Doc[:cut], "0") == "" && taxid.Valid(p.Doc[cut:])
}

// sequenceOf is the sequence a nosso número was made from: its digits but the check
// digit; 0 when it is not one.
func sequenceOf(ourNumber string) int64 {
	if len(ourNumber) != 12 {
		return 0
	}
	var n int64
	for _, r := range ourNumber[:11] {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int64(r-'0')
	}
	return n
}

// answer is a title's occurrence as a return reports it.
func (s *Sim) answer(c *client, p cnab240.SegmentP, q cnab240.SegmentQ, o cnab240.Occurrence, reasons ...cnab240.Reason) cnab240.ReturnTitle {
	t := cnab240.ReturnTitle{
		T: cnab240.SegmentT{
			Occurrence: o, Account: c.Account, OurNumber: p.OurNumber, Portfolio: p.Portfolio, DocumentNumber: p.DocumentNumber,
			Due: p.Due, Amount: p.Amount, CollectingBank: Code, CompanyUse: p.CompanyUse, Currency: 9, Payer: q.Payer,
		},
		U: cnab240.SegmentU{Occurrence: o, OccurredOn: s.today()},
	}
	copy(t.T.Reasons[:], reasons)
	return t
}

// Pay is a payer paying a boleto by its typed line or barcode at its bank's internet
// banking, for its amount.
func (s *Sim) Pay(code string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	barcode := boleto.Barcode(code)
	if len(code) != 44 {
		var err error
		if barcode, err = boleto.ParseLine(code); err != nil {
			return "", fmt.Errorf("%w: %w", ErrInvalid, err)
		}
	}
	for _, c := range s.byToken {
		for _, t := range c.titles {
			if t.barcode == barcode {
				return t.p.OurNumber, s.settle(c, t, cnab240.ChannelInternet)
			}
		}
	}
	return "", fmt.Errorf("%w: no boleto with that code", ErrNotFound)
}

// PayPix is a payer paying a hybrid boleto's Pix QR code.
func (s *Sim) PayPix(code string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pix, err := brcode.Parse(code)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	for _, c := range s.byToken {
		if t := c.byLocation[pix.URL]; t != nil {
			return t.p.OurNumber, s.settle(c, t, cnab240.ChannelPix)
		}
	}
	return "", fmt.Errorf("%w: no boleto with that Pix QR code", ErrNotFound)
}

// settle pays a registered title in full: the payment is credited to the account on the
// next business day.
func (s *Sim) settle(c *client, t *title, channel cnab240.Reason) error {
	if t.status != registered {
		return fmt.Errorf("%w: the boleto is %s", ErrConflict, t.status)
	}
	t.status = paid
	ret := s.answer(c, t.p, t.q, cnab240.Settled, channel)
	credit := bizday.Next(s.today().AddDate(0, 0, 1))
	ret.U.Paid, ret.U.Credited, ret.U.CreditOn = t.p.Amount, t.p.Amount, credit
	s.queue(c, ret)
	s.book(c, Entry{Date: credit.Format(time.DateOnly), Kind: "credit", Amount: t.p.Amount, Reference: t.p.OurNumber, Description: "LIQUIDACAO BOLETO"})
	return nil
}

// writeOffDue writes off the titles past their due date by more than the days their
// remittance gave.
func (s *Sim) writeOffDue(c *client) {
	today := s.today()
	for _, t := range c.titles {
		if t.status == registered && today.After(t.p.Due.AddDate(0, 0, int(t.p.WriteOffDays))) {
			t.status = writtenOff
			s.queue(c, s.answer(c, t.p, t.q, cnab240.WrittenOff, cnab240.WrittenOffByBank))
		}
	}
}

// flush writes what happened since the last return file into a new one.
func (s *Sim) flush(c *client) {
	c.release()
	if len(c.pending) == 0 {
		return
	}
	sequence := int64(len(c.returns) + 1)
	f := cnab240.ReturnFile{
		Header: cnab240.FileHeader{
			Bank: Code, DocType: cnab240.CNPJ, Doc: c.TaxID, Agreement: c.Agreement, Account: c.Account, CompanyName: upper(c.Name, 30),
			BankName: "BANCO SIMULADO", Generated: s.cfg.Now().In(brasilia), Sequence: sequence,
		},
		Batches: []cnab240.ReturnBatch{{
			Header: cnab240.BatchHeader{
				DocType: cnab240.CNPJ, Doc: c.TaxID, Agreement: c.Agreement, Account: c.Account, CompanyName: upper(c.Name, 30),
				Number: sequence, Recorded: s.today(),
			},
			Titles: c.pending,
		}},
	}
	data, err := f.Bytes()
	if err != nil {
		s.cfg.Logger.Error("writing a return file", "error", err)
		return
	}
	c.returns = append(c.returns, returnFile{sequence: sequence, created: s.cfg.Now(), data: data})
	c.pending = nil
}

func upper(s string, n int) string {
	s = strings.ToUpper(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

// Returns lists a client's return files after a sequence.
func (s *Sim) Returns(token string, after int64) ([]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.byToken[token]
	if c == nil {
		return nil, fmt.Errorf("%w: unknown client", ErrNotFound)
	}
	out := []int64{}
	for _, r := range c.returns {
		if r.sequence > after {
			out = append(out, r.sequence)
		}
	}
	return out, nil
}

// ReturnFile is a client's return file.
func (s *Sim) ReturnFile(token string, sequence int64) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.byToken[token]
	if c == nil || sequence < 1 || sequence > int64(len(c.returns)) {
		return nil, fmt.Errorf("%w: return %d", ErrNotFound, sequence)
	}
	return bytes.Clone(c.returns[sequence-1].data), nil
}

// Boleto is how a registered boleto looks to its payer.
type Boleto struct {
	OurNumber string
	Line      string
	PixCode   string
	Status    string
}

// Boletos lists a client's boletos, for tests and the operator.
func (s *Sim) Boletos(token string) []Boleto {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.byToken[token]
	if c == nil {
		return nil
	}
	var out []Boleto
	for _, t := range c.titles {
		b := Boleto{OurNumber: t.p.OurNumber, Line: t.barcode.Line(), Status: string(t.status)}
		if t.location != "" {
			b.PixCode, _ = brcode.Pix{PointOfInitiation: brcode.SingleUse, URL: t.location, MerchantName: upper(c.Name, 25), MerchantCity: "SAO PAULO"}.Encode()
		}
		out = append(out, b)
	}
	return out
}
