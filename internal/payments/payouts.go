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
	// PayoutHeld waits for an operator to release it.
	PayoutHeld   PayoutStatus = "held"
	PayoutPaid   PayoutStatus = "paid"
	PayoutFailed PayoutStatus = "failed"
	// PayoutReturned was paid, and sent back by the receiving bank: its amount is the
	// balance's again.
	PayoutReturned PayoutStatus = "returned"
)

// Payout methods.
const (
	PayoutPix          = "pix"
	PayoutBankTransfer = "bank_transfer"
)

// The least a payout carries, by method: Jupiter's policy, not a rail's.
var minPayout = map[string]int64{PayoutPix: 1_00, PayoutBankTransfer: 10_00}

// MinPayout is the least a payout by method carries.
func MinPayout(method string) int64 { return minPayout[method] }

type Payout struct {
	ID          id.ID
	Owner       Owner
	Amount      money.Amount
	Destination PayoutDestination
	Description string
	Status      PayoutStatus
	// The failure, or, for a returned payout, why it came back.
	FailureCode    string
	FailureMessage string
	// EndToEndID identifies a Pix across the SPI; RecipientName is who DICT says owns
	// the key, once paid.
	EndToEndID    string
	RecipientName string
	ArrivedAt     time.Time
	ReturnedAt    time.Time
	CreatedAt     time.Time
	// Recipient is the recipient whose balance paid it; empty for the merchant's.
	Recipient string
	// ScheduledOn is the day a recipient's transfer settings made it, if they did.
	ScheduledOn string
}

// PayoutDestination is where a payout goes: a Pix key, or a bank account and its holder.
type PayoutDestination struct {
	Method      string
	PixKey      string
	ISPB        string
	Branch      string
	Account     string
	HolderName  string
	HolderTaxID string
}

type PayoutParams struct {
	Amount      money.Amount
	PixKey      string
	Description string
	// Recipient pays out a recipient's available balance instead of the merchant's, to
	// the recipient's own destination.
	Recipient string
	// ScheduledOn makes it the recipient's scheduled payout of that day: one a day.
	ScheduledOn time.Time
}

// PayoutSource is where a recipient's payout comes from and goes to.
type PayoutSource struct {
	Account     id.ID
	Destination PayoutDestination
	// Held says an operator holds the recipient's payouts.
	Held bool
}

// RecipientBalances are recipients' balances, which payouts can be paid from.
type RecipientBalances interface {
	// PayoutSource is the recipient's available balance and destination; an error if it
	// may not be paid out.
	PayoutSource(ctx context.Context, tx pgx.Tx, owner Owner, recipientID string, currency money.Currency) (PayoutSource, error)
	// PaidOut records that a payout left the recipient's balance; PayoutReturned, that it
	// came back.
	PaidOut(ctx context.Context, tx pgx.Tx, owner Owner, recipientID, payoutID string, amount money.Amount) error
	PayoutReturned(ctx context.Context, tx pgx.Tx, owner Owner, recipientID, payoutID string, amount money.Amount) error
}

// BankTransferRail is the bank that makes Jupiter's transfers. A transfer is named by its
// id: the same id again answers the same transfer. A transfer the receiving bank sends
// back after it was made reads as returned.
type BankTransferRail interface {
	Transfer(ctx context.Context, t BankTransferRequest) (PixTransfer, error)
	TransferStatus(ctx context.Context, id string) (PixTransfer, error)
}

type BankTransferRequest struct {
	ID          string
	Amount      money.Amount
	Destination PayoutDestination
}

func (s *Service) transferRail(livemode bool) (BankTransferRail, error) {
	r := s.cfg.TestTransfers
	if livemode {
		r = s.cfg.LiveTransfers
	}
	if r == nil {
		return nil, fmt.Errorf("%w: no bank for transfers in this mode", ErrRailUnavailable)
	}
	return r, nil
}

const (
	maxPayoutDescription = 140
	// returnWindow is how long after it was paid a bank transfer may still come back.
	returnWindow = 7 * 24 * time.Hour
)

// CreatePayout holds the amount on the merchant's balance, or the recipient's, and
// records the payout, before the bank is asked. A recipient's payout goes to its own
// destination, by Pix or bank transfer; one whose payouts an operator holds waits held.
func (s *Service) CreatePayout(ctx context.Context, tx pgx.Tx, owner Owner, p PayoutParams) (Payout, error) {
	if !p.Amount.IsPositive() || p.Amount.Currency() != money.BRL {
		return Payout{}, fmt.Errorf("%w: a payout is a positive amount in BRL", ErrInvalid)
	}
	src := PayoutSource{Destination: PayoutDestination{Method: PayoutPix, PixKey: p.PixKey}}
	if p.Recipient != "" {
		if s.cfg.Balances == nil {
			return Payout{}, fmt.Errorf("%w: recipients are not available", ErrInvalid)
		}
		var err error
		if src, err = s.cfg.Balances.PayoutSource(ctx, tx, owner, p.Recipient, p.Amount.Currency()); err != nil {
			return Payout{}, err
		}
		// A recipient's money goes only where the recipient was verified to be paid.
		if d := src.Destination; d.Method == "" || (p.PixKey != "" && (d.Method != PayoutPix || p.PixKey != d.PixKey)) {
			return Payout{}, fmt.Errorf("%w: a recipient is paid out to its own payout_destination, and to no other", ErrInvalid)
		}
	}
	if err := s.checkPayout(owner, p, src.Destination); err != nil {
		return Payout{}, err
	}
	q := db.New(tx)
	if err := q.LockPayouts(ctx, owner.Merchant.String()+"/"+strconv.FormatBool(owner.Livemode)+"/"+p.Amount.Currency().Code()+"/"+p.Recipient); err != nil {
		return Payout{}, err
	}
	source, available, err := s.payoutFunds(ctx, tx, owner, p, src.Account)
	if err != nil {
		return Payout{}, err
	}
	if available < p.Amount.Minor() {
		return Payout{}, fmt.Errorf("%w: %d available, %d asked for", ErrInsufficientFunds, available, p.Amount.Minor())
	}
	if err := scheduledOnce(ctx, q, owner, p); err != nil {
		return Payout{}, err
	}
	return s.insertPayout(ctx, tx, owner, p, src, source)
}

// scheduledOnce refuses a recipient's second scheduled payout of a day. It runs under the
// payouts' lock: no other payout of the recipient's is being made.
func scheduledOnce(ctx context.Context, q *db.Queries, owner Owner, p PayoutParams) error {
	if p.ScheduledOn.IsZero() {
		return nil
	}
	made, err := q.ScheduledPayoutMade(ctx, db.ScheduledPayoutMadeParams{
		MerchantID: owner.Merchant.String(), Livemode: owner.Livemode, RecipientID: p.Recipient,
		ScheduledOn: pgtype.Date{Time: p.ScheduledOn, Valid: true},
	})
	if err == nil && made {
		err = fmt.Errorf("%w: the recipient's payout of that day was made", ErrInvalidState)
	}
	return err
}

// checkPayout checks a payout's destination, its description, its minimum, and that the
// mode has the rail it goes by.
func (s *Service) checkPayout(owner Owner, p PayoutParams, d PayoutDestination) error {
	switch {
	case d.Method == PayoutPix && !ValidPixKey(d.PixKey):
		return fmt.Errorf("%w: destination[pix_key] is not a Pix key: a CPF, a CNPJ, an e-mail, a phone number (+55...) or a random key", ErrInvalid)
	case d.Method == PayoutBankTransfer && (d.ISPB == "" || d.Account == "" || d.HolderName == "" || d.HolderTaxID == ""):
		return fmt.Errorf("%w: a bank transfer needs the bank, the account and its holder", ErrInvalid)
	case len([]rune(p.Description)) > maxPayoutDescription:
		return fmt.Errorf("%w: description is longer than %d characters", ErrInvalid, maxPayoutDescription)
	case p.Amount.Minor() < minPayout[d.Method]:
		return fmt.Errorf("%w: a payout by %s is at least %d", ErrInvalid, d.Method, minPayout[d.Method])
	}
	var err error
	if d.Method == PayoutBankTransfer {
		_, err = s.transferRail(owner.Livemode)
	} else {
		_, err = s.pixRail(owner.Livemode)
	}
	return err
}

func (s *Service) insertPayout(ctx context.Context, tx pgx.Tx, owner Owner, p PayoutParams, src PayoutSource, source id.ID) (Payout, error) {
	settlement, err := s.payoutSettlement(ctx, tx, owner.Livemode, p.Amount.Currency(), src.Destination.Method)
	if err != nil {
		return Payout{}, err
	}
	payoutID := PayoutPrefix.New().String()
	hold, err := s.cfg.Ledger.Hold(ctx, tx, ledger.Hold{
		Description: "payout " + payoutID, Debit: source, Credit: settlement, Amount: p.Amount,
	})
	if err != nil {
		return Payout{}, fmt.Errorf("holding the payout: %w", err)
	}
	status := "sending"
	if src.Held {
		status = "held"
	}
	d := src.Destination
	scheduled := pgtype.Date{}
	if !p.ScheduledOn.IsZero() {
		scheduled = pgtype.Date{Time: p.ScheduledOn, Valid: true}
	}
	if err := db.New(tx).InsertPayout(ctx, db.InsertPayoutParams{
		ID: payoutID, MerchantID: owner.Merchant.String(), Livemode: owner.Livemode, Amount: p.Amount.Minor(),
		Currency: p.Amount.Currency().Code(), PixKey: d.PixKey, Description: p.Description, Status: status, LedgerHold: hold.ID.String(),
		RecipientID: p.Recipient, Method: d.Method, BankIspb: d.ISPB, BankBranch: d.Branch, BankAccount: d.Account,
		HolderName: d.HolderName, HolderTaxID: d.HolderTaxID, ScheduledOn: scheduled, SourceAccount: source.String(),
		Now: ts(s.cfg.Now().UTC()),
	}); err != nil {
		if postgres.ErrorCode(err) == "23505" {
			return Payout{}, fmt.Errorf("%w: the recipient's payout of that day was made", ErrInvalidState)
		}
		return Payout{}, fmt.Errorf("recording payout: %w", err)
	}
	if err := s.publish(ctx, tx, owner, events.TypePayoutCreated, payoutID); err != nil {
		return Payout{}, err
	}
	return s.Payout(ctx, tx, owner, mustPayoutID(payoutID))
}

// payoutSettlement is the account a payout leaves by: Jupiter's at its Pix bank, or at
// the bank that makes its transfers.
func (s *Service) payoutSettlement(ctx context.Context, tx pgx.Tx, livemode bool, currency money.Currency, method string) (id.ID, error) {
	if method == PayoutBankTransfer {
		return s.bankAccount(ctx, tx, livemode, currency)
	}
	pix, err := s.pixAccounts(ctx, tx, livemode, currency)
	return pix.settlement, err
}

// ReleaseHeldPayouts lets a recipient's held payouts go: the worker sends them.
func (s *Service) ReleaseHeldPayouts(ctx context.Context, tx pgx.Tx, recipientID string) (int, error) {
	// Dated back past the resolver's wait, so that its next pass sends them.
	released, err := db.New(tx).ReleaseHeldPayouts(ctx, db.ReleaseHeldPayoutsParams{
		RecipientID: recipientID, Now: ts(s.cfg.Now().UTC().Add(-s.cfg.ResolveAfter)),
	})
	return len(released), err
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
	amount, err := money.New(row.Amount, mustCurrency(row.Currency))
	if err != nil {
		return PixTransfer{}
	}
	if row.Method == PayoutBankTransfer {
		rail, err := s.transferRail(row.Livemode)
		if err != nil {
			return PixTransfer{}
		}
		t, err := rail.Transfer(ctx, BankTransferRequest{ID: bankID(row.ID), Amount: amount, Destination: destinationOf(row)})
		return transferOutcome(t, err)
	}
	rail, err := s.pixRail(row.Livemode)
	if err != nil {
		return PixTransfer{}
	}
	t, err := rail.Transfer(ctx, PixTransferRequest{ID: bankID(row.ID), Amount: amount, Key: row.PixKey, Description: row.Description})
	return transferOutcome(t, err)
}

// transferStatus asks a payout's bank how its transfer stands.
func (s *Service) transferStatus(ctx context.Context, row db.PaymentsPayout) (PixTransfer, error) {
	if row.Method == PayoutBankTransfer {
		rail, err := s.transferRail(row.Livemode)
		if err != nil {
			return PixTransfer{}, err
		}
		return rail.TransferStatus(ctx, bankID(row.ID))
	}
	rail, err := s.pixRail(row.Livemode)
	if err != nil {
		return PixTransfer{}, err
	}
	return rail.TransferStatus(ctx, bankID(row.ID))
}

func destinationOf(row db.PaymentsPayout) PayoutDestination {
	return PayoutDestination{
		Method: row.Method, PixKey: row.PixKey, ISPB: row.BankIspb, Branch: row.BankBranch, Account: row.BankAccount,
		HolderName: row.HolderName, HolderTaxID: row.HolderTaxID,
	}
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
// releases it, returned gives a paid amount back; anything else leaves the payout pending
// for the resolver.
func (s *Service) FinishPayout(ctx context.Context, tx pgx.Tx, owner Owner, payoutID id.ID, t PixTransfer) (Payout, error) {
	q := db.New(tx)
	row, err := q.LockPayoutByID(ctx, payoutID.String())
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (row.MerchantID != owner.Merchant.String() || row.Livemode != owner.Livemode)) {
		return Payout{}, fmt.Errorf("%w: %s", ErrNotFound, payoutID)
	}
	if err != nil {
		return Payout{}, err
	}
	now := s.cfg.Now().UTC()
	if t.Status == PixTransferReturned && (row.Status == "paid" || row.Status == "sending" || row.Status == "unknown") {
		if err := s.paidAndReturned(ctx, tx, owner, &row, t); err != nil {
			return Payout{}, err
		}
		row.UpdatedAt = ts(now)
		if err := savePayout(ctx, q, row); err != nil {
			return Payout{}, err
		}
		return payoutFromRow(row)
	}
	if row.Status != "sending" && row.Status != "unknown" {
		return payoutFromRow(row)
	}
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

// paidAndReturned applies a return; a payout Jupiter had not heard was made is paid first.
func (s *Service) paidAndReturned(ctx context.Context, tx pgx.Tx, owner Owner, row *db.PaymentsPayout, t PixTransfer) error {
	if row.Status != "paid" {
		if err := s.payoutPaid(ctx, tx, owner, row, t); err != nil {
			return err
		}
	}
	return s.payoutReturned(ctx, tx, owner, row, t)
}

// payoutReturned gives back a payout the receiving bank returned: what left the balance
// comes back to it, through Jupiter's account at the bank.
func (s *Service) payoutReturned(ctx context.Context, tx pgx.Tx, owner Owner, row *db.PaymentsPayout, t PixTransfer) error {
	amount, err := money.New(row.Amount, mustCurrency(row.Currency))
	if err != nil {
		return err
	}
	source, err := ledger.AccountPrefix.Parse(row.SourceAccount)
	if err != nil {
		return err
	}
	settlement, err := s.payoutSettlement(ctx, tx, row.Livemode, amount.Currency(), row.Method)
	if err != nil {
		return err
	}
	if _, err := s.cfg.Ledger.Post(ctx, tx, ledger.Posting{
		Description: "payout " + row.ID + " returned", Legs: []ledger.Leg{ledger.Debit(settlement, amount), ledger.Credit(source, amount)},
	}); err != nil {
		return fmt.Errorf("posting the returned payout: %w", err)
	}
	if row.RecipientID != "" {
		if s.cfg.Balances == nil {
			return fmt.Errorf("payments: payout %s is a recipient's, and recipients are not available", row.ID)
		}
		if err := s.cfg.Balances.PayoutReturned(ctx, tx, owner, row.RecipientID, row.ID, amount); err != nil {
			return err
		}
	}
	row.Status, row.FailureCode, row.FailureMessage = "returned", "transfer_returned", t.Reason
	row.ReturnedAt = ts(s.cfg.Now().UTC())
	return s.publish(ctx, tx, owner, events.TypePayoutReturned, row.ID)
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
		Before: ts(s.cfg.Now().UTC().Add(-s.cfg.ResolveAfter)), WatchSince: ts(s.cfg.Now().UTC().Add(-returnWindow)), MaxCount: resolveBatch,
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
	t, err := s.transferStatus(ctx, row)
	switch {
	case errors.Is(err, ErrPixNotFound) && row.Status != "paid":
		t = s.transfer(ctx, row)
	case row.Status == "paid" && (err != nil || t.Status != PixTransferReturned):
		// Paid, and not returned so far: look again later, within the window.
		return false, errors.Join(err, db.New(pool).TouchPayout(ctx, db.TouchPayoutParams{ID: row.ID, UpdatedAt: ts(s.cfg.Now().UTC())}))
	default:
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
	return t.Status == PixTransferPaid || t.Status == PixTransferFailed || t.Status == PixTransferReturned, err
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
	switch row.Status {
	case "paid", "failed", "returned", "held":
		status = PayoutStatus(row.Status)
	}
	out := Payout{
		ID: payoutID, Owner: Owner{Merchant: merchant, Livemode: row.Livemode}, Amount: amount, Destination: destinationOf(row),
		Description: row.Description, Status: status, FailureCode: row.FailureCode, FailureMessage: row.FailureMessage,
		EndToEndID: row.E2eID, RecipientName: row.RecipientName, ArrivedAt: row.ArrivedAt.Time, ReturnedAt: row.ReturnedAt.Time,
		CreatedAt: row.CreatedAt.Time, Recipient: row.RecipientID,
	}
	if row.ScheduledOn.Valid {
		out.ScheduledOn = row.ScheduledOn.Time.Format(time.DateOnly)
	}
	return out, nil
}

func savePayout(ctx context.Context, q *db.Queries, row db.PaymentsPayout) error {
	return q.SavePayout(ctx, db.SavePayoutParams{
		ID: row.ID, Status: row.Status, FailureCode: row.FailureCode, FailureMessage: row.FailureMessage, E2eID: row.E2eID,
		RecipientName: row.RecipientName, UnknownSince: row.UnknownSince, Resolutions: row.Resolutions,
		ArrivedAt: row.ArrivedAt, ReturnedAt: row.ReturnedAt, UpdatedAt: row.UpdatedAt,
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
