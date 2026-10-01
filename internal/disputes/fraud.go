package disputes

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/disputes/db"
	"github.com/iricardofernandes/jupiter/internal/disputes/rules"
	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/platform/page"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
)

// FraudReport is an issuer's report that a card payment was fraud (Visa's TC40,
// Mastercard's SAFE). It disputes nothing and moves no money, but counts against the
// merchant in the dispute ratio.
type FraudReport struct {
	ID            id.ID
	Owner         payments.Owner
	PaymentIntent string
	Network       string
	FraudType     string
	Amount        money.Amount
	ReportedAt    time.Time
}

// ReportedFraud is a fraud report as the network sends it.
type ReportedFraud struct {
	NetworkID            string
	NetworkTransactionID string
	FraudType            string
	ReportedAt           time.Time
}

var fraudTypes = map[string]bool{
	"card_never_received": true, "fraudulent_application": true, "counterfeit": true, "account_takeover": true,
	"card_not_present": true, "lost": true, "stolen": true, "other": true,
}

// ApplyFraudReport records a fraud report on a card payment, once.
func (s *Service) ApplyFraudReport(ctx context.Context, pool *pgxpool.Pool, livemode bool, r ReportedFraud) error {
	if r.NetworkID == "" || !fraudTypes[r.FraudType] {
		return fmt.Errorf("%w: a fraud report names itself and a known fraud type", ErrInvalid)
	}
	return postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		p, err := s.cfg.Payments.DisputedPaymentByNetworkID(ctx, tx, livemode, r.NetworkTransactionID)
		if err != nil {
			return err
		}
		if p.Method != payments.MethodCard {
			return fmt.Errorf("%w: %s is not a card payment", ErrInvalid, r.NetworkTransactionID)
		}
		_, err = s.insertFraudReport(ctx, tx, livemode, p, r)
		return err
	})
}

func (s *Service) insertFraudReport(ctx context.Context, tx pgx.Tx, livemode bool, p payments.DisputedPayment, r ReportedFraud) (string, error) {
	q := db.New(tx)
	reportID := FraudReportPrefix.New().String()
	reported := r.ReportedAt
	if reported.IsZero() {
		reported = s.cfg.Now()
	}
	inserted, err := q.InsertFraudReport(ctx, db.InsertFraudReportParams{
		ID: reportID, MerchantID: p.Owner.Merchant.String(), Livemode: livemode, PaymentIntent: p.Intent.String(), Network: p.Scheme,
		NetworkID: r.NetworkID, FraudType: r.FraudType, Amount: p.Captured.Minor(), Currency: p.Captured.Currency().Code(),
		ReportedAt: ts(reported), Now: ts(s.cfg.Now()),
	})
	if err != nil || inserted == 0 {
		return "", err // reported already
	}
	if s.cfg.Events != nil {
		if _, err := s.cfg.Events.Publish(ctx, tx, events.Owner{Merchant: p.Owner.Merchant, Livemode: livemode}, events.TypeFraudReportCreated,
			events.ObjectRef{ID: reportID, Type: "fraud_report"}); err != nil {
			return "", err
		}
	}
	return reportID, nil
}

// CreateTestFraudReport reports one of the merchant's test card payments as fraud, as an
// issuer would.
func (s *Service) CreateTestFraudReport(ctx context.Context, tx pgx.Tx, owner payments.Owner, intentID id.ID, fraudType string) (FraudReport, error) {
	if owner.Livemode {
		return FraudReport{}, fmt.Errorf("%w: test fraud reports are made in test mode", ErrInvalid)
	}
	if !fraudTypes[fraudType] {
		return FraudReport{}, fmt.Errorf("%w: unknown fraud type %q", ErrInvalid, fraudType)
	}
	p, err := s.cfg.Payments.DisputedPayment(ctx, tx, owner, intentID)
	if err != nil {
		return FraudReport{}, err
	}
	if p.Method != payments.MethodCard {
		return FraudReport{}, fmt.Errorf("%w: only a card payment is reported as fraud", ErrInvalid)
	}
	reportID, err := s.insertFraudReport(ctx, tx, false, p, ReportedFraud{NetworkID: "tc40_" + p.Attempt, FraudType: fraudType})
	if err != nil {
		return FraudReport{}, err
	}
	if reportID == "" {
		return FraudReport{}, fmt.Errorf("%w: the payment was reported already", ErrInvalidState)
	}
	row, err := db.New(tx).GetFraudReportByNetworkID(ctx, db.GetFraudReportByNetworkIDParams{Livemode: false, NetworkID: "tc40_" + p.Attempt})
	if err != nil {
		return FraudReport{}, err
	}
	return fraudReportOf(row, owner)
}

func fraudReportOf(row db.DisputesFraudReport, owner payments.Owner) (FraudReport, error) {
	reportID, err := FraudReportPrefix.Parse(row.ID)
	if err != nil {
		return FraudReport{}, err
	}
	currency, err := money.CurrencyByCode(row.Currency)
	if err != nil {
		return FraudReport{}, err
	}
	amount, err := money.New(row.Amount, currency)
	if err != nil {
		return FraudReport{}, err
	}
	return FraudReport{
		ID: reportID, Owner: owner, PaymentIntent: row.PaymentIntent, Network: row.Network, FraudType: row.FraudType,
		Amount: amount, ReportedAt: row.ReportedAt.Time,
	}, nil
}

// FraudReports lists the merchant's fraud reports, newest first.
func (s *Service) FraudReports(ctx context.Context, q db.DBTX, owner payments.Owner, r page.Request) ([]FraudReport, bool, error) {
	rows, err := db.New(q).ListFraudReports(ctx, db.ListFraudReportsParams{
		MerchantID: owner.Merchant.String(), Livemode: owner.Livemode,
		StartingAfter: r.StartingAfter, EndingBefore: r.EndingBefore, MaxCount: r.Fetch(),
	})
	if err != nil {
		return nil, false, err
	}
	out := make([]FraudReport, 0, len(rows))
	for _, row := range rows {
		f, err := fraudReportOf(row, owner)
		if err != nil {
			return nil, false, err
		}
		out = append(out, f)
	}
	items, more := page.Trim(out, r)
	return items, more, nil
}

// Monitoring is a merchant's dispute ratio for a month, in the style of Visa's VAMP: card
// disputes opened and fraud reports received over card payments, in basis points.
type Monitoring struct {
	Month        string
	Transactions int64
	Disputes     int64
	FraudReports int64
	RatioBps     int64
	Threshold    rules.Threshold
	Excessive    bool
}

// Monitor is the merchant's dispute ratio for the month month falls in (Brasília time).
func (s *Service) Monitor(ctx context.Context, q db.DBTX, owner payments.Owner, month time.Time) (Monitoring, error) {
	from, to := monthOf(month)
	counts, err := db.New(q).MonthCounts(ctx, db.MonthCountsParams{
		MerchantID: owner.Merchant.String(), Livemode: owner.Livemode, FromTime: pgtype.Timestamptz{Time: from, Valid: true}, ToTime: pgtype.Timestamptz{Time: to, Valid: true},
	})
	if err != nil {
		return Monitoring{}, err
	}
	transactions, err := s.cfg.Payments.CardPaymentsBetween(ctx, q, owner, from, to)
	if err != nil {
		return Monitoring{}, err
	}
	m := Monitoring{
		Month: from.In(brasilia).Format("2006-01"), Transactions: transactions, Disputes: counts.Disputes, FraudReports: counts.FraudReports,
		Threshold: rules.ThresholdFor(rules.DisputeRatio, from),
	}
	events := m.Disputes + m.FraudReports
	if transactions > 0 {
		m.RatioBps = events * 10_000 / transactions
	}
	m.Excessive = m.RatioBps >= m.Threshold.Bps && events >= m.Threshold.MinimumCount
	return m, nil
}

// Excessive lists the merchants whose dispute ratio for the month month falls in is at
// or above the threshold, for the worker to report.
func (s *Service) Excessive(ctx context.Context, pool *pgxpool.Pool, month time.Time) ([]Monitoring, []payments.Owner, error) {
	from, to := monthOf(month)
	active, err := db.New(pool).ActiveMerchants(ctx, db.ActiveMerchantsParams{FromTime: pgtype.Timestamptz{Time: from, Valid: true}, ToTime: pgtype.Timestamptz{Time: to, Valid: true}})
	if err != nil {
		return nil, nil, err
	}
	var out []Monitoring
	var owners []payments.Owner
	for _, a := range active {
		merchantID, err := id.Parse(a.MerchantID)
		if err != nil {
			return nil, nil, err
		}
		owner := payments.Owner{Merchant: merchantID, Livemode: a.Livemode}
		m, err := s.Monitor(ctx, pool, owner, month)
		if err != nil {
			return nil, nil, err
		}
		if m.Excessive {
			out, owners = append(out, m), append(owners, owner)
		}
	}
	return out, owners, nil
}

var brasilia = time.FixedZone("BRT", -3*60*60)

// monthOf is the calendar month, in Brasília, that t falls in.
func monthOf(t time.Time) (from, to time.Time) {
	y, m, _ := t.In(brasilia).Date()
	from = time.Date(y, m, 1, 0, 0, 0, 0, brasilia)
	return from.UTC(), from.AddDate(0, 1, 0).UTC()
}
