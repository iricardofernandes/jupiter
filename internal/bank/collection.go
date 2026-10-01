package bank

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/bank/db"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/pkg/cnab240"
)

// The fixed terms of the boletos Jupiter issues: simple collection (carteira 1),
// registered with the bank (cadastramento 1), a commercial bill (espécie 02, duplicata
// mercantil), not accepted (aceite N), issued and sent by the company (2), in reais (09).
const (
	portfolio    = 1
	registration = 1
	billKind     = 2
	issuedBy     = 2
	currencyReal = 9
	writeOff     = 1 // baixar/devolver
)

func (c *Connector) now() pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: c.cfg.Now().UTC(), Valid: true}
}

// Remit sends the bank what it has not been sent: new boletos, and boletos to write off,
// in a remittance kept as built, so that one whose answer was lost is sent again the
// same. Boletos canceled before they were sent are written off at once.
func (c *Connector) Remit(ctx context.Context, p *payments.Service) (int, error) {
	err := postgres.InTx(ctx, c.cfg.Pool, func(tx pgx.Tx) error {
		if err := c.withdrawUnsent(ctx, tx, p); err != nil {
			return err
		}
		return c.buildRemittance(ctx, tx)
	})
	if err != nil {
		return 0, err
	}
	q := db.New(c.cfg.Pool)
	unsent, err := q.UnsentRemittances(ctx, c.cfg.Livemode)
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, r := range unsent {
		if _, _, err := c.call(ctx, http.MethodPost, "/v1/collection/remittances", "text/plain", r.Data, nil); err != nil {
			return sent, err
		}
		if err := q.MarkSent(ctx, db.MarkSentParams{Livemode: c.cfg.Livemode, Sequence: r.Sequence}); err != nil {
			return sent, err
		}
		sent++
	}
	return sent, nil
}

func (c *Connector) withdrawUnsent(ctx context.Context, tx pgx.Tx, p *payments.Service) error {
	withdrawn, err := db.New(tx).WithdrawnBeforeRemitting(ctx, db.WithdrawnBeforeRemittingParams{Livemode: c.cfg.Livemode, Now: c.now()})
	if err != nil {
		return err
	}
	for _, attemptID := range withdrawn {
		if err := p.ApplyBoletoEvent(ctx, tx, payments.BoletoEvent{Attempt: attemptID, Livemode: c.cfg.Livemode, Kind: payments.BoletoWrittenOff}); err != nil &&
			!errors.Is(err, payments.ErrNotWaiting) {
			return err
		}
	}
	return nil
}

func (c *Connector) buildRemittance(ctx context.Context, tx pgx.Tx) error {
	q := db.New(tx)
	titles, err := q.TitlesToRemit(ctx, c.cfg.Livemode)
	if err != nil || len(titles) == 0 {
		return err
	}
	sequence, err := q.NextRemittance(ctx, c.cfg.Livemode)
	if err != nil {
		return err
	}
	now := c.cfg.Now()
	f := cnab240.RemittanceFile{
		Header: cnab240.FileHeader{
			Bank: c.cfg.Profile.Code(), DocType: cnab240.CNPJ, Doc: c.cfg.TaxID, Agreement: c.cfg.Agreement, Account: c.cfg.Account,
			CompanyName: upper(c.cfg.Name, 30), BankName: upper(c.cfg.Profile.Name(), 30), Generated: now, Sequence: sequence,
		},
		Batches: []cnab240.RemittanceBatch{{Header: cnab240.BatchHeader{
			DocType: cnab240.CNPJ, Doc: c.cfg.TaxID, Agreement: c.cfg.Agreement, Account: c.cfg.Account, CompanyName: upper(c.cfg.Name, 30),
			Number: sequence, Recorded: now,
		}}},
	}
	attempts := make([]string, 0, len(titles))
	for _, t := range titles {
		f.Batches[0].Titles = append(f.Batches[0].Titles, c.title(t, now))
		attempts = append(attempts, t.AttemptID)
	}
	data, err := f.Bytes()
	if err != nil {
		return err
	}
	if err := q.InsertRemittance(ctx, db.InsertRemittanceParams{Livemode: c.cfg.Livemode, Sequence: sequence, Data: data, Now: c.now()}); err != nil {
		return err
	}
	return q.MarkRemitted(ctx, db.MarkRemittedParams{Attempts: attempts, Remittance: pgtype.Int8{Int64: sequence, Valid: true}, Now: c.now()})
}

// title is a boleto's segments: its entry, or, registered and to be canceled, a request
// to write it off.
func (c *Connector) title(t db.BankTitle, now time.Time) cnab240.RemittanceTitle {
	movement := cnab240.Entry
	if t.Status == "registered" {
		movement = cnab240.WriteOffRequest
	}
	docType := cnab240.CNPJ
	if len(t.PayerTaxID) == 11 {
		docType = cnab240.CPF
	}
	rt := cnab240.RemittanceTitle{
		P: cnab240.SegmentP{
			Movement: movement, Account: c.cfg.Account, OurNumber: t.OurNumber, Portfolio: portfolio, Registration: registration,
			DocumentType: "1", IssuedBy: issuedBy, Distribution: "2", DocumentNumber: strconv.FormatInt(t.Sequence, 10),
			Due: t.Due.Time, Amount: t.Amount, Kind: billKind, Accept: "N", Issued: now, CompanyUse: t.AttemptID[:min(len(t.AttemptID), 25)],
			WriteOffCode: writeOff, WriteOffDays: int64(t.DaysAfterDue), Currency: currencyReal,
		},
		Q: cnab240.SegmentQ{Movement: movement, Payer: cnab240.Party{DocType: docType, Doc: t.PayerTaxID, Name: t.PayerName}},
	}
	if t.Hybrid && movement == cnab240.Entry {
		rt.P.Distribution = "P"
	}
	return rt
}

func upper(s string, n int) string {
	b := []byte(s)
	for i, r := range b {
		if r >= 'a' && r <= 'z' {
			b[i] = r - 'a' + 'A'
		}
	}
	if len(b) > n {
		b = b[:n]
	}
	return string(b)
}

// maxReturns is how many return files one pass reads.
const maxReturns = 100

// ImportReturns reads the bank's return files Jupiter has not read, in order, each in a
// transaction of its own, and tells payments what each says of its boletos.
func (c *Connector) ImportReturns(ctx context.Context, pool *pgxpool.Pool, p *payments.Service) (int, error) {
	last, err := db.New(pool).LastReturn(ctx, c.cfg.Livemode)
	if err != nil {
		return 0, err
	}
	var list struct {
		Files []int64 `json:"files"`
	}
	if _, _, err := c.call(ctx, http.MethodGet, "/v1/collection/returns?after="+strconv.FormatInt(last, 10), "", nil, &list); err != nil {
		return 0, err
	}
	imported := 0
	for _, sequence := range list.Files[:min(len(list.Files), maxReturns)] {
		// Files come in order, one after another: a gap or a jump would skip the ones
		// between for good.
		if sequence != last+1 {
			return imported, fmt.Errorf("bank: return %d listed after %d", sequence, last)
		}
		data, _, err := c.call(ctx, http.MethodGet, "/v1/collection/returns/"+strconv.FormatInt(sequence, 10), "", nil, nil)
		if err != nil {
			return imported, err
		}
		f, err := cnab240.ParseReturn(data)
		if err != nil {
			return imported, fmt.Errorf("bank: return %d: %w", sequence, err)
		}
		if f.Header.Sequence != sequence {
			return imported, fmt.Errorf("bank: return %d says it is %d", sequence, f.Header.Sequence)
		}
		if err := postgres.InTx(ctx, pool, func(tx pgx.Tx) error { return c.apply(ctx, tx, p, sequence, f) }); err != nil {
			return imported, err
		}
		imported++
		last = sequence
	}
	return imported, nil
}

func (c *Connector) apply(ctx context.Context, tx pgx.Tx, p *payments.Service, sequence int64, f cnab240.ReturnFile) error {
	q := db.New(tx)
	n := 0
	for _, b := range f.Batches {
		n += len(b.Titles)
	}
	inserted, err := q.InsertReturn(ctx, db.InsertReturnParams{Livemode: c.cfg.Livemode, Sequence: sequence, Titles: int32(n), Now: c.now()})
	if err != nil || inserted == 0 {
		return err // read already
	}
	line := int32(0)
	for _, b := range f.Batches {
		for _, rt := range b.Titles {
			line++
			if err := q.InsertReturnRecord(ctx, db.InsertReturnRecordParams{
				Livemode: c.cfg.Livemode, ReturnSequence: sequence, Line: line, OurNumber: rt.T.OurNumber, Occurrence: int32(rt.T.Occurrence), //nolint:gosec // two digits
				Paid: rt.U.Paid, OccurredOn: dateOf(rt.U.OccurredOn), CreditOn: dateOf(rt.U.CreditOn), ImportedOn: dateOf(day(c.cfg.Now())),
			}); err != nil {
				return err
			}
			if err := c.occurrence(ctx, tx, p, sequence, rt); err != nil {
				return err
			}
		}
	}
	return nil
}

func dateOf(t time.Time) pgtype.Date {
	return pgtype.Date{Time: time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC), Valid: !t.IsZero()}
}

// day is the calendar day t falls on in Brasília.
func day(t time.Time) time.Time {
	y, m, d := t.In(brasilia).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

var brasilia = time.FixedZone("BRT", -3*60*60)

// moves are the title statuses an occurrence may follow; a return that would take a
// title anywhere else is out of order or repeated.
var moves = map[string][]string{
	"registered":  {"issued", "remitted"},
	"rejected":    {"issued", "remitted"},
	"paid":        {"issued", "remitted", "registered", "written_off"},
	"written_off": {"issued", "remitted", "registered"},
}

// occurrence applies one title's occurrence. What it cannot apply, and might be money, it
// records for an operator.
func (c *Connector) occurrence(ctx context.Context, tx pgx.Tx, p *payments.Service, sequence int64, rt cnab240.ReturnTitle) error {
	q := db.New(tx)
	exception := func(attempt, kind string, amount int64, detail string) error {
		c.cfg.Logger.ErrorContext(ctx, "a return Jupiter could not apply", "return", sequence, "our_number", rt.T.OurNumber, "kind", kind)
		return q.InsertException(ctx, db.InsertExceptionParams{
			Livemode: c.cfg.Livemode, ReturnSequence: sequence, OurNumber: rt.T.OurNumber, AttemptID: attempt, Kind: kind,
			Amount: amount, Detail: detail, Now: c.now(),
		})
	}
	if rt.T.Account != c.cfg.Account {
		return exception("", "foreign_account", rt.U.Paid, fmt.Sprintf("occurrence %d", rt.T.Occurrence))
	}
	t, err := q.LockTitleByOurNumber(ctx, db.LockTitleByOurNumberParams{Livemode: c.cfg.Livemode, OurNumber: rt.T.OurNumber})
	if errors.Is(err, pgx.ErrNoRows) {
		return exception("", "unknown_title", rt.U.Paid, fmt.Sprintf("occurrence %d", rt.T.Occurrence))
	}
	if err != nil {
		return err
	}
	e, status, pix := c.event(t, rt)
	if status == "" {
		return nil // instructions confirmed or rejected, charges: nothing for the payment
	}
	if e.Kind == payments.BoletoPaid && (rt.T.Amount != t.Amount || rt.U.Paid != t.Amount) {
		// Jupiter's boletos carry no interest, fine or discount: a payment of another
		// amount is not one it can take as is.
		return exception(t.AttemptID, "amount_mismatch", rt.U.Paid, fmt.Sprintf("boleto of %d, title of %d", t.Amount, rt.T.Amount))
	}
	if !slices.Contains(moves[status], t.Status) {
		if e.Kind == payments.BoletoPaid {
			return exception(t.AttemptID, "paid_not_waiting", rt.U.Paid, "title "+t.Status)
		}
		c.cfg.Logger.WarnContext(ctx, "a return out of order", "attempt", t.AttemptID, "from", t.Status, "to", status)
		return nil
	}
	if err := q.SetTitleStatus(ctx, db.SetTitleStatusParams{AttemptID: t.AttemptID, Status: status, PixCode: pix, Now: c.now()}); err != nil {
		return err
	}
	if e.Kind == payments.BoletoPaid {
		if err := q.SetTitleCredit(ctx, db.SetTitleCreditParams{AttemptID: t.AttemptID, CreditOn: dateOf(rt.U.CreditOn)}); err != nil {
			return err
		}
	}
	err = p.ApplyBoletoEvent(ctx, tx, e)
	switch {
	case errors.Is(err, payments.ErrNotWaiting) && e.Kind == payments.BoletoPaid:
		return exception(t.AttemptID, "paid_not_waiting", rt.U.Paid, "the payment no longer waited")
	case errors.Is(err, payments.ErrNotWaiting):
		c.cfg.Logger.WarnContext(ctx, "a return about a boleto no longer waited on", "attempt", t.AttemptID, "occurrence", int(rt.T.Occurrence))
		return nil
	}
	return err
}

// event is what an occurrence tells payments, and the title's status after it; no status
// for an occurrence that changes nothing for the payment.
func (c *Connector) event(t db.BankTitle, rt cnab240.ReturnTitle) (payments.BoletoEvent, string, string) {
	e := payments.BoletoEvent{Attempt: t.AttemptID, Livemode: c.cfg.Livemode, Reason: string(rt.T.Reasons[0])}
	pix := t.PixCode
	switch o := rt.T.Occurrence; {
	case o == cnab240.EntryConfirmed:
		if rt.Y03 != nil && rt.Y03.Key != "" {
			pix = c.pixCode(rt.Y03.Key)
			e.PixCode = pix
		}
		e.Kind = payments.BoletoRegistered
		return e, "registered", pix
	case o == cnab240.EntryRejected:
		e.Kind = payments.BoletoRejected
		return e, "rejected", pix
	case o.Paid():
		e.Kind, e.Paid, e.Channel = payments.BoletoPaid, rt.U.Paid, "bank"
		if rt.T.Reasons[0] == cnab240.ChannelPix {
			e.Channel = "pix"
		}
		return e, "paid", pix
	case o == cnab240.WrittenOff:
		e.Kind = payments.BoletoWrittenOff
		return e, "written_off", pix
	}
	return e, "", pix
}
