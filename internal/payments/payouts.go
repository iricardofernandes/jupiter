package payments

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/payments/db"
	"github.com/iricardofernandes/jupiter/internal/platform/page"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
)

// Payouts move money out of a merchant's balance to a Pix key. The amount is held on the
// ledger before the bank is asked, so no two payouts spend the same balance, and posted
// or released once the bank says how the transfer ended. A transfer whose answer is
// lost is asked about again, never abandoned: the money may have left.

var PayoutPrefix = id.MustPrefix("po")

var ErrInsufficientFunds = errors.New("payments: the balance does not cover the payout")

type PayoutStatus string

const (
	PayoutPending PayoutStatus = "pending"
	PayoutPaid    PayoutStatus = "paid"
	PayoutFailed  PayoutStatus = "failed"
)

type Payout struct {
	ID             id.ID
	Owner          Owner
	Amount         money.Amount
	PixKey         string
	Description    string
	Status         PayoutStatus
	FailureCode    string
	FailureMessage string
	// EndToEndID identifies the Pix across the SPI; RecipientName is who DICT says owns
	// the key, once paid.
	EndToEndID    string
	RecipientName string
	ArrivedAt     time.Time
	CreatedAt     time.Time
	// Recipient is the recipient whose balance paid it; empty for the merchant's.
	Recipient string
}

type PayoutParams struct {
	Amount      money.Amount
	PixKey      string
	Description string
	// Recipient pays out a recipient's available balance instead of the merchant's, to
	// the recipient's own Pix key.
	Recipient string
}

// RecipientBalances are recipients' balances, which payouts can be paid from.
type RecipientBalances interface {
	// PayoutSource is the recipient's available balance, and its Pix key; an error if it
	// may not be paid out.
	PayoutSource(ctx context.Context, tx pgx.Tx, owner Owner, recipientID string, currency money.Currency) (id.ID, string, error)
	// PaidOut records that a payout left the recipient's balance.
	PaidOut(ctx context.Context, tx pgx.Tx, owner Owner, recipientID, payoutID string, amount money.Amount) error
}

const maxPayoutDescription = 140

// CreatePayout holds the amount on the merchant's balance, or the recipient's, and
// records the payout, before the bank is asked.
func (s *Service) CreatePayout(ctx context.Context, tx pgx.Tx, owner Owner, p PayoutParams) (Payout, error) {
	if !p.Amount.IsPositive() || p.Amount.Currency() != money.BRL {
		return Payout{}, fmt.Errorf("%w: a payout is a positive amount in BRL", ErrInvalid)
	}
	var recipientAccount id.ID
	if p.Recipient != "" {
		if s.cfg.Balances == nil {
			return Payout{}, fmt.Errorf("%w: recipients are not available", ErrInvalid)
		}
		account, key, err := s.cfg.Balances.PayoutSource(ctx, tx, owner, p.Recipient, p.Amount.Currency())
		if err != nil {
			return Payout{}, err
		}
		// A recipient's money goes only where the recipient was verified to be paid.
		if key == "" || (p.PixKey != "" && p.PixKey != key) {
			return Payout{}, fmt.Errorf("%w: a recipient is paid out to its own Pix key, set as its payout_destination", ErrInvalid)
		}
		recipientAccount, p.PixKey = account, key
	}
	switch {
	case !ValidPixKey(p.PixKey):
		return Payout{}, fmt.Errorf("%w: destination[pix_key] is not a Pix key: a CPF, a CNPJ, an e-mail, a phone number (+55...) or a random key", ErrInvalid)
	case len([]rune(p.Description)) > maxPayoutDescription:
		return Payout{}, fmt.Errorf("%w: description is longer than %d characters", ErrInvalid, maxPayoutDescription)
	}
	if _, err := s.pixRail(owner.Livemode); err != nil {
		return Payout{}, err
	}
	q := db.New(tx)
	if err := q.LockPayouts(ctx, owner.Merchant.String()+"/"+strconv.FormatBool(owner.Livemode)+"/"+p.Amount.Currency().Code()+"/"+p.Recipient); err != nil {
		return Payout{}, err
	}
	source, available, err := s.payoutFunds(ctx, tx, owner, p, recipientAccount)
	if err != nil {
		return Payout{}, err
	}
	if available < p.Amount.Minor() {
		return Payout{}, fmt.Errorf("%w: %d available, %d asked for", ErrInsufficientFunds, available, p.Amount.Minor())
	}
	pix, err := s.pixAccounts(ctx, tx, owner.Livemode, p.Amount.Currency())
	if err != nil {
		return Payout{}, err
	}
	payoutID := PayoutPrefix.New().String()
	hold, err := s.cfg.Ledger.Hold(ctx, tx, ledger.Hold{
		Description: "payout " + payoutID, Debit: source, Credit: pix.settlement, Amount: p.Amount,
	})
	if err != nil {
		return Payout{}, fmt.Errorf("holding the payout: %w", err)
	}
	if err := q.InsertPayout(ctx, db.InsertPayoutParams{
		ID: payoutID, MerchantID: owner.Merchant.String(), Livemode: owner.Livemode, Amount: p.Amount.Minor(),
		Currency: p.Amount.Currency().Code(), PixKey: p.PixKey, Description: p.Description, LedgerHold: hold.ID.String(),
		RecipientID: p.Recipient, Now: ts(s.cfg.Now().UTC()),
	}); err != nil {
		return Payout{}, fmt.Errorf("recording payout: %w", err)
	}
	if err := s.publish(ctx, tx, owner, events.TypePayoutCreated, payoutID); err != nil {
		return Payout{}, err
	}
	return s.Payout(ctx, tx, owner, mustPayoutID(payoutID))
}

// payoutFunds is the account a payout is paid from, the recipient's or else the
// merchant's, and what it has available: for the merchant's, less refunds in flight,
// which post only once confirmed and must not be paid out before.
func (s *Service) payoutFunds(ctx context.Context, tx pgx.Tx, owner Owner, p PayoutParams, recipientAccount id.ID) (id.ID, int64, error) {
	source, refunding := recipientAccount, int64(0)
	if p.Recipient == "" {
		accts, err := s.ledgerAccounts(ctx, tx, owner, p.Amount.Currency())
		if err != nil {
			return id.ID{}, 0, err
		}
		source = accts.merchantBalance
		if refunding, err = db.New(tx).RefundsInFlight(ctx, db.RefundsInFlightParams{
			MerchantID: owner.Merchant.String(), Livemode: owner.Livemode, Currency: p.Amount.Currency().Code(),
		}); err != nil {
			return id.ID{}, 0, err
		}
	}
	balance, err := s.cfg.Ledger.Balance(ctx, tx, source)
	if err != nil {
		return id.ID{}, 0, err
	}
	available, err := balance.Available()
	if err != nil {
		return id.ID{}, 0, err
	}
	return source, available.Minor() - refunding, nil
}

// SendPayout asks the bank for the transfer. It runs outside any transaction and may be
// repeated: the transfer is named by the payout.
func (s *Service) SendPayout(ctx context.Context, q db.DBTX, owner Owner, payoutID id.ID) (PixTransfer, error) {
	row, err := db.New(q).GetPayout(ctx, db.GetPayoutParams{ID: payoutID.String(), MerchantID: owner.Merchant.String(), Livemode: owner.Livemode})
	if err != nil {
		return PixTransfer{}, notFoundOr(err, payoutID)
	}
	if row.Status != "sending" && row.Status != "unknown" {
		return PixTransfer{}, nil
	}
	return s.transfer(ctx, row), nil
}

func (s *Service) transfer(ctx context.Context, row db.PaymentsPayout) PixTransfer {
	rail, err := s.pixRail(row.Livemode)
	if err != nil {
		return PixTransfer{}
	}
	amount, err := money.New(row.Amount, mustCurrency(row.Currency))
	if err != nil {
		return PixTransfer{}
	}
	t, err := rail.Transfer(ctx, PixTransferRequest{ID: bankID(row.ID), Amount: amount, Key: row.PixKey, Description: row.Description})
	return transferOutcome(t, err)
}

// transferOutcome folds errors into a transfer: a refusal fails it, anything else leaves
// it unknown.
func transferOutcome(t PixTransfer, err error) PixTransfer {
	switch {
	case errors.Is(err, ErrPixRefused):
		return PixTransfer{Status: PixTransferFailed, Reason: "The bank refused the transfer."}
	case err != nil:
		return PixTransfer{}
	}
	return t
}

// FinishPayout applies how the transfer ended: paid posts the held amount, failed
// releases it, anything else leaves the payout pending for the resolver.
func (s *Service) FinishPayout(ctx context.Context, tx pgx.Tx, owner Owner, payoutID id.ID, t PixTransfer) (Payout, error) {
	q := db.New(tx)
	row, err := q.LockPayoutByID(ctx, payoutID.String())
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (row.MerchantID != owner.Merchant.String() || row.Livemode != owner.Livemode)) {
		return Payout{}, fmt.Errorf("%w: %s", ErrNotFound, payoutID)
	}
	if err != nil {
		return Payout{}, err
	}
	if row.Status != "sending" && row.Status != "unknown" {
		return payoutFromRow(row)
	}
	now := s.cfg.Now().UTC()
	switch t.Status {
	case PixTransferPaid:
		err = s.payoutPaid(ctx, tx, owner, &row, t)
	case PixTransferFailed:
		err = s.payoutFailed(ctx, tx, owner, &row, t)
	default:
		row.Status = "unknown"
		if !row.UnknownSince.Valid {
			row.UnknownSince = ts(now)
		}
	}
	if err != nil {
		return Payout{}, err
	}
	row.UpdatedAt = ts(now)
	if err := savePayout(ctx, q, row); err != nil {
		return Payout{}, err
	}
	return payoutFromRow(row)
}

// payoutPaid posts the held amount: the money has left.
func (s *Service) payoutPaid(ctx context.Context, tx pgx.Tx, owner Owner, row *db.PaymentsPayout, t PixTransfer) error {
	hold, err := ledger.TransactionPrefix.Parse(row.LedgerHold)
	if err != nil {
		return err
	}
	amount, err := money.New(row.Amount, mustCurrency(row.Currency))
	if err != nil {
		return err
	}
	if _, err := s.cfg.Ledger.PostPending(ctx, tx, hold, amount); err != nil {
		return fmt.Errorf("posting payout: %w", err)
	}
	if row.RecipientID != "" {
		if s.cfg.Balances == nil {
			return fmt.Errorf("payments: payout %s is a recipient's, and recipients are not available", row.ID)
		}
		if err := s.cfg.Balances.PaidOut(ctx, tx, owner, row.RecipientID, row.ID, amount); err != nil {
			return err
		}
	}
	row.Status, row.E2eID, row.RecipientName, row.UnknownSince = "paid", t.E2EID, t.Recipient, pgtype.Timestamptz{}
	row.ArrivedAt = ts(t.SettledAt)
	if t.SettledAt.IsZero() {
		row.ArrivedAt = ts(s.cfg.Now().UTC())
	}
	return s.publish(ctx, tx, owner, events.TypePayoutPaid, row.ID)
}

// payoutFailed releases the held amount back to the balance.
func (s *Service) payoutFailed(ctx context.Context, tx pgx.Tx, owner Owner, row *db.PaymentsPayout, t PixTransfer) error {
	hold, err := ledger.TransactionPrefix.Parse(row.LedgerHold)
	if err != nil {
		return err
	}
	if _, err := s.cfg.Ledger.Void(ctx, tx, hold); err != nil && !errors.Is(err, ledger.ErrAlreadyResolved) {
		return fmt.Errorf("releasing payout: %w", err)
	}
	row.Status, row.FailureCode, row.FailureMessage, row.UnknownSince = "failed", "pix_transfer_failed", t.Reason, pgtype.Timestamptz{}
	return s.publish(ctx, tx, owner, events.TypePayoutFailed, row.ID)
}

// ResolvePayouts asks the bank about payouts whose transfer has no answer yet, sending
// again the ones it never received.
func (s *Service) ResolvePayouts(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	due, err := db.New(pool).PayoutsToResolve(ctx, db.PayoutsToResolveParams{
		Before: ts(s.cfg.Now().UTC().Add(-s.cfg.ResolveAfter)), MaxCount: resolveBatch,
	})
	if err != nil {
		return 0, fmt.Errorf("finding payouts to resolve: %w", err)
	}
	resolved := 0
	var failures []error
	for _, payoutID := range due {
		ok, err := s.resolvePayout(ctx, pool, payoutID)
		if err != nil {
			failures = append(failures, fmt.Errorf("resolving %s: %w", payoutID, err))
		}
		if ok {
			resolved++
		}
	}
	return resolved, errors.Join(failures...)
}

func (s *Service) resolvePayout(ctx context.Context, pool *pgxpool.Pool, payoutID string) (bool, error) {
	row, err := db.New(pool).GetPayoutByID(ctx, payoutID)
	if err != nil {
		return false, err
	}
	rail, err := s.pixRail(row.Livemode)
	if err != nil {
		return false, err
	}
	t, err := rail.TransferStatus(ctx, bankID(row.ID))
	if errors.Is(err, ErrPixNotFound) {
		t = s.transfer(ctx, row)
	} else {
		t = transferOutcome(t, err)
	}
	merchant, err := id.Parse(row.MerchantID)
	if err != nil {
		return false, err
	}
	err = postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := s.FinishPayout(ctx, tx, Owner{Merchant: merchant, Livemode: row.Livemode}, mustPayoutID(row.ID), t); err != nil {
			return err
		}
		locked, err := db.New(tx).LockPayoutByID(ctx, row.ID)
		if err != nil || locked.Status != "unknown" {
			return err
		}
		locked.Resolutions++
		locked.UpdatedAt = ts(s.cfg.Now().UTC())
		return savePayout(ctx, db.New(tx), locked)
	})
	return t.Status == PixTransferPaid || t.Status == PixTransferFailed, err
}

func (s *Service) Payout(ctx context.Context, q db.DBTX, owner Owner, payoutID id.ID) (Payout, error) {
	row, err := db.New(q).GetPayout(ctx, db.GetPayoutParams{ID: payoutID.String(), MerchantID: owner.Merchant.String(), Livemode: owner.Livemode})
	if err != nil {
		return Payout{}, notFoundOr(err, payoutID)
	}
	return payoutFromRow(row)
}

func (s *Service) Payouts(ctx context.Context, q db.DBTX, owner Owner, r page.Request) ([]Payout, bool, error) {
	rows, err := db.New(q).ListPayouts(ctx, db.ListPayoutsParams{
		MerchantID: owner.Merchant.String(), Livemode: owner.Livemode,
		StartingAfter: r.StartingAfter, EndingBefore: r.EndingBefore, MaxCount: r.Fetch(),
	})
	if err != nil {
		return nil, false, fmt.Errorf("listing payouts: %w", err)
	}
	out := make([]Payout, 0, len(rows))
	for _, row := range rows {
		p, err := payoutFromRow(row)
		if err != nil {
			return nil, false, err
		}
		out = append(out, p)
	}
	items, more := page.Trim(out, r)
	return items, more, nil
}

func payoutFromRow(row db.PaymentsPayout) (Payout, error) {
	payoutID, err := PayoutPrefix.Parse(row.ID)
	if err != nil {
		return Payout{}, err
	}
	merchant, err := id.Parse(row.MerchantID)
	if err != nil {
		return Payout{}, err
	}
	amount, err := money.New(row.Amount, mustCurrency(row.Currency))
	if err != nil {
		return Payout{}, err
	}
	status := PayoutPending
	if row.Status == "paid" || row.Status == "failed" {
		status = PayoutStatus(row.Status)
	}
	return Payout{
		ID: payoutID, Owner: Owner{Merchant: merchant, Livemode: row.Livemode}, Amount: amount, PixKey: row.PixKey,
		Description: row.Description, Status: status, FailureCode: row.FailureCode, FailureMessage: row.FailureMessage,
		EndToEndID: row.E2eID, RecipientName: row.RecipientName, ArrivedAt: row.ArrivedAt.Time, CreatedAt: row.CreatedAt.Time,
		Recipient: row.RecipientID,
	}, nil
}

func savePayout(ctx context.Context, q *db.Queries, row db.PaymentsPayout) error {
	return q.SavePayout(ctx, db.SavePayoutParams{
		ID: row.ID, Status: row.Status, FailureCode: row.FailureCode, FailureMessage: row.FailureMessage, E2eID: row.E2eID,
		RecipientName: row.RecipientName, UnknownSince: row.UnknownSince, Resolutions: row.Resolutions,
		ArrivedAt: row.ArrivedAt, UpdatedAt: row.UpdatedAt,
	})
}

func mustPayoutID(s string) id.ID {
	payoutID, err := PayoutPrefix.Parse(s)
	if err != nil {
		panic(err)
	}
	return payoutID
}

var (
	emailKey = regexp.MustCompile(`^[a-zA-Z0-9.!#$%&'*+/=?^_{|}~-]+@[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$`)
	phoneKey = regexp.MustCompile(`^\+[1-9]\d{1,14}$`)
	evpKey   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// ValidPixKey reports whether s is a Pix key as DICT writes them (Manual de Padrões
// §2.5.1): a CPF or CNPJ, an e-mail of at most 77 characters, a phone number in E.164
// (+5561912345678), or a random key (EVP) as a lower-case UUID.
func ValidPixKey(s string) bool {
	switch {
	case ValidTaxID(s), phoneKey.MatchString(s), evpKey.MatchString(s):
		return true
	case len(s) <= 77 && emailKey.MatchString(s):
		return true
	}
	return false
}
