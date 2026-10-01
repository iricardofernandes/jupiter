package receivables

import (
	"fmt"
	"math"
	"math/big"

	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/payments"
)

// Line types: a recipient's share, or the merchant's own (commission); what of Jupiter's
// fee a recipient paid; and the centavos rounding left, which one recipient takes.
const (
	LineRecipient  = "recipient"
	LineCommission = "commission"
	LineFee        = "fee"
	LineRemainder  = "remainder"
)

// share is what a capture gives one recipient.
type share struct {
	recipient  string
	commission bool
	liable     bool
	gross      int64 // its share, rounded down
	remainder  int64 // the centavos left over, for the remainder rule's recipient
	fee        int64 // its part of Jupiter's fee
}

func (s share) net() int64 { return s.gross + s.remainder - s.fee }

// divide splits a capture of amount, of which fee is Jupiter's, by rules: each recipient
// gets its weight's share of the amount, rounded down; the remainder rule's recipient
// gets the centavos left over; and the fee is shared by the rules that pay it, in
// proportion to what they got. No centavo is lost or made up, and no recipient nets less
// than nothing. own is the merchant's own recipient, whose share is its commission.
func divide(amount, fee money.Amount, rules []payments.SplitRule, own string) ([]share, error) {
	if len(rules) == 0 {
		return nil, fmt.Errorf("%w: no split rules", ErrInvalid)
	}
	total, err := totalWeight(rules)
	if err != nil {
		return nil, err
	}
	out := make([]share, len(rules))
	var given int64
	for i, r := range rules {
		// amount × weight / total, in big integers: the product overflows int64.
		p := new(big.Int).Mul(big.NewInt(amount.Minor()), big.NewInt(r.Weight()))
		out[i] = share{recipient: r.Recipient, commission: r.Recipient == own, liable: r.Liable, gross: p.Quo(p, big.NewInt(total)).Int64()}
		given += out[i].gross
	}
	payers := make([]int64, len(rules))
	everyone := make([]int64, len(rules))
	for i, r := range rules {
		if r.Remainder {
			out[i].remainder = amount.Minor() - given
		}
		everyone[i] = out[i].gross + out[i].remainder
		if r.ChargeFee {
			payers[i] = everyone[i]
		}
	}
	if fee.IsPositive() {
		if fee.Minor() > amount.Minor() {
			return nil, fmt.Errorf("%w: a fee of %d on a payment of %d", ErrInvalid, fee.Minor(), amount.Minor())
		}
		fees, err := feeShares(fee, payers, everyone)
		if err != nil {
			return nil, err
		}
		for i := range out {
			out[i].fee = fees[i]
		}
	}
	for _, s := range out {
		if s.net() < 0 {
			return nil, fmt.Errorf("%w: %s's share of %d does not cover its part of the fee, %d", ErrInvalid, s.recipient, s.gross+s.remainder, s.fee)
		}
	}
	return out, nil
}

func totalWeight(rules []payments.SplitRule) (int64, error) {
	var total int64
	for _, r := range rules {
		if r.Weight() <= 0 || r.Weight() > math.MaxInt64/int64(len(rules)) {
			return 0, fmt.Errorf("%w: a split rule's weight is %d", ErrInvalid, r.Weight())
		}
		total += r.Weight() // cannot overflow: each weight is under MaxInt64 / len(rules)
	}
	return total, nil
}

// feeShares divides the fee among its payers in proportion to their shares. Should their
// shares not cover it, as only a payment of a few centavos among many recipients can do,
// it is divided among every recipient instead: in proportion to shares that add up to the
// whole payment, no part is more than its share.
func feeShares(fee money.Amount, payers, everyone []int64) ([]int64, error) {
	for _, weights := range [][]int64{payers, everyone} {
		parts, err := fee.Allocate(weights...)
		if err != nil {
			continue // no payer has a share
		}
		out := make([]int64, len(parts))
		covered := true
		for i, p := range parts {
			out[i] = p.Minor()
			covered = covered && out[i] <= everyone[i]
		}
		if covered {
			return out, nil
		}
	}
	return nil, fmt.Errorf("%w: the shares cannot cover a fee of %d", ErrInvalid, fee.Minor())
}

// lines are a share's typed lines, those with an amount.
func (s share) lines() []splitLine {
	kind := LineRecipient
	if s.commission {
		kind = LineCommission
	}
	out := []splitLine{{kind: kind, amount: s.gross}}
	if s.fee > 0 {
		out = append(out, splitLine{kind: LineFee, amount: s.fee})
	}
	if s.remainder > 0 {
		out = append(out, splitLine{kind: LineRemainder, amount: s.remainder})
	}
	return out
}

type splitLine struct {
	kind   string
	amount int64
}
