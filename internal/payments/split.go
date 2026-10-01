package payments

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/payments/db"
)

// SplitRule gives a recipient its share of a card payment: a fixed amount or a
// percentage, the same kind for every rule of a payment. Exactly one rule is liable for
// the payment's chargebacks, and exactly one takes the centavos rounding leaves over.
// ChargeFee rules share Jupiter's fee, in proportion to their shares.
type SplitRule struct {
	Recipient  string `json:"recipient"`
	Amount     int64  `json:"amount,omitempty"`
	Percentage string `json:"percentage,omitempty"`
	Liable     bool   `json:"liable"`
	Remainder  bool   `json:"remainder"`
	ChargeFee  bool   `json:"charge_fee"`
}

// Weight is the rule's share in the units it is given in: centavos, or hundredths of a
// percent.
func (r SplitRule) Weight() int64 {
	if r.Percentage == "" {
		return r.Amount
	}
	whole, frac, _ := strings.Cut(r.Percentage, ".")
	w, _ := strconv.ParseInt(whole+frac, 10, 64)
	return w
}

// Recipients checks the recipients a split names: each the merchant's, in its mode, and
// not rejected.
type Recipients interface {
	CheckSplit(ctx context.Context, tx pgx.Tx, owner Owner, recipientIDs []string) error
}

const (
	maxSplitRules = 20
	// minFeePayersPercent keeps the fee covered: the rules that pay it have at least this
	// share, well above any price in the table, so their shares always cover it.
	minFeePayersPercent = 10
)

var percentagePattern = regexp.MustCompile(`^\d{1,3}\.\d{2}$`)

// validateSplit checks a split's arithmetic: it gives out exactly the amount, or exactly
// 100%, with one liable rule and one remainder rule. An empty split is the merchant's
// whole.
func validateSplit(rules []SplitRule, amount money.Amount) error {
	if len(rules) == 0 {
		return nil
	}
	if len(rules) > maxSplitRules {
		return fmt.Errorf("%w: a split has at most %d rules", ErrInvalid, maxSplitRules)
	}
	seen := map[string]bool{}
	liable, remainder, fee := 0, 0, 0
	var total, feeWeight int64
	byPercentage := rules[0].Percentage != ""
	for _, r := range rules {
		switch {
		case r.Recipient == "" || seen[r.Recipient]:
			return fmt.Errorf("%w: each split rule names a recipient of its own", ErrInvalid)
		case byPercentage != (r.Percentage != ""):
			return fmt.Errorf("%w: a split's rules are all amounts or all percentages", ErrInvalid)
		case byPercentage && (!percentagePattern.MatchString(r.Percentage) || r.Amount != 0):
			return fmt.Errorf("%w: a percentage has two decimals, such as \"12.50\", and no amount", ErrInvalid)
		case r.Weight() <= 0:
			return fmt.Errorf("%w: each split rule gives its recipient more than nothing", ErrInvalid)
		case !byPercentage && r.Amount > amount.Minor():
			return fmt.Errorf("%w: a split rule's amount is more than the payment", ErrInvalid)
		}
		seen[r.Recipient] = true
		total += r.Weight()
		liable += b2i(r.Liable)
		remainder += b2i(r.Remainder)
		fee += b2i(r.ChargeFee)
		if r.ChargeFee {
			feeWeight += r.Weight()
		}
	}
	switch {
	case byPercentage && total != 100_00:
		return fmt.Errorf("%w: the split's percentages add up to %d.%02d%%, not 100%%", ErrInvalid, total/100, total%100)
	case !byPercentage && total != amount.Minor():
		return fmt.Errorf("%w: the split's amounts add up to %d, not the payment's %d", ErrInvalid, total, amount.Minor())
	case liable != 1:
		return fmt.Errorf("%w: exactly one split rule is liable for chargebacks", ErrInvalid)
	case remainder != 1:
		return fmt.Errorf("%w: exactly one split rule takes the remainder", ErrInvalid)
	case fee == 0 || feeWeight*100 < total*minFeePayersPercent:
		return fmt.Errorf("%w: the split rules that pay Jupiter's fee must have at least %d%% of the payment between them", ErrInvalid, minFeePayersPercent)
	}
	return nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// checkSplit checks a split, and its recipients when a directory is configured. A split
// is for card payments only.
func (s *Service) checkSplit(ctx context.Context, tx pgx.Tx, owner Owner, rules []SplitRule, amount money.Amount, paymentMethod string) error {
	if len(rules) == 0 {
		return nil
	}
	if isPix(paymentMethod) || paymentMethod == PaymentMethodBoleto {
		return fmt.Errorf("%w: only card payments can be split", ErrInvalid)
	}
	if err := validateSplit(rules, amount); err != nil {
		return err
	}
	if s.cfg.Recipients == nil {
		return fmt.Errorf("%w: splits are not available", ErrInvalid)
	}
	ids := make([]string, 0, len(rules))
	for _, r := range rules {
		ids = append(ids, r.Recipient)
	}
	return s.cfg.Recipients.CheckSplit(ctx, tx, owner, ids)
}

func splitColumn(rules []SplitRule) []byte {
	if len(rules) == 0 {
		return nil
	}
	raw, _ := json.Marshal(rules)
	return raw
}

func splitOf(row db.PaymentsIntent) ([]SplitRule, error) {
	if row.Split == nil {
		return nil, nil
	}
	var rules []SplitRule
	if err := json.Unmarshal(row.Split, &rules); err != nil {
		return nil, fmt.Errorf("payments: the split of %s: %w", row.ID, err)
	}
	return rules, nil
}

func mustAmount(row db.PaymentsIntent) money.Amount {
	a, err := money.New(row.Amount, mustCurrency(row.Currency))
	if err != nil {
		panic(err) // the row's currency was checked when it was written
	}
	return a
}

// recheckSplit checks the split an intent holds, against its amount and payment method
// as they are now.
func (s *Service) recheckSplit(ctx context.Context, tx pgx.Tx, owner Owner, row db.PaymentsIntent) error {
	rules, err := splitOf(row)
	if err != nil {
		return err
	}
	return s.checkSplit(ctx, tx, owner, rules, mustAmount(row), row.PaymentMethod)
}
