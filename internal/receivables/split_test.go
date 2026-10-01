package receivables

import (
	"fmt"
	"math/big"
	"testing"

	"pgregory.net/rapid"

	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/payments"
)

func genRules() *rapid.Generator[[]payments.SplitRule] {
	return rapid.Custom(func(t *rapid.T) []payments.SplitRule {
		n := rapid.IntRange(1, 8).Draw(t, "rules")
		rules := make([]payments.SplitRule, n)
		byPercentage := rapid.Bool().Draw(t, "percentages")
		for i := range rules {
			rules[i].Recipient = fmt.Sprintf("rp_%d", i)
			if byPercentage {
				bps := rapid.Int64Range(1, 10_000).Draw(t, "bps")
				rules[i].Percentage = fmt.Sprintf("%d.%02d", bps/100, bps%100)
			} else {
				rules[i].Amount = rapid.Int64Range(1, 1_000_000_000).Draw(t, "amount")
			}
			rules[i].ChargeFee = rapid.Bool().Draw(t, "fee")
		}
		rules[rapid.IntRange(0, n-1).Draw(t, "liable")].Liable = true
		rules[rapid.IntRange(0, n-1).Draw(t, "remainder")].Remainder = true
		rules[rapid.IntRange(0, n-1).Draw(t, "a payer")].ChargeFee = true
		return rules
	})
}

// For any split and any capture: the lines add up to the payment, the fee lines to the
// fee, every recipient gets its share to within a centavo before the remainder, and no
// one nets less than nothing.
func TestPropertySplitLinesSumToThePayment(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		rules := genRules().Draw(t, "split")
		// No price is above 5%.
		amount, _ := money.New(rapid.Int64Range(1, 10_000_000_000).Draw(t, "captured"), money.BRL)
		rate := rapid.Int64Range(0, 50).Draw(t, "fee per mille")
		fee, _ := money.New((amount.Minor()*rate+500)/1000, money.BRL)
		shares, err := divide(amount, fee, rules, "rp_0")
		if err != nil {
			t.Fatal(err)
		}
		var total, fees, nets, weights int64
		for _, r := range rules {
			weights += r.Weight()
		}
		for i, s := range shares {
			for _, l := range s.lines() {
				if l.amount < 0 {
					t.Fatalf("a line of %d", l.amount)
				}
				if l.kind == LineFee {
					fees += l.amount
				} else {
					total += l.amount
				}
			}
			if s.net() < 0 {
				t.Fatalf("%s nets %d", s.recipient, s.net())
			}
			nets += s.net()
			exact := new(big.Rat).SetFrac(new(big.Int).Mul(big.NewInt(amount.Minor()), big.NewInt(rules[i].Weight())), big.NewInt(weights))
			if diff := new(big.Rat).Sub(exact, new(big.Rat).SetInt64(s.gross)); diff.Sign() < 0 || diff.Cmp(big.NewRat(1, 1)) >= 0 {
				t.Fatalf("%s got %d of an exact %s", s.recipient, s.gross, exact.FloatString(4))
			}

			if s.remainder > 0 && !rules[i].Remainder {
				t.Fatalf("%s took a remainder it does not take", s.recipient)
			}
		}
		if total != amount.Minor() || fees != fee.Minor() || nets != amount.Minor()-fee.Minor() {
			t.Fatalf("lines %d of %d, fees %d of %d, nets %d", total, amount.Minor(), fees, fee.Minor(), nets)
		}
	})
}

func TestSplitExample(t *testing.T) {
	amount, _ := money.New(60000, money.BRL)
	fee, _ := money.New(2094, money.BRL)
	shares, err := divide(amount, fee, []payments.SplitRule{
		{Recipient: "rp_seller", Percentage: "90.00", Liable: true},
		{Recipient: "rp_market", Percentage: "10.00", Remainder: true, ChargeFee: true},
	}, "rp_market")
	if err != nil {
		t.Fatal(err)
	}
	if shares[0].net() != 54000 || shares[1].net() != 6000-2094 || !shares[1].commission || shares[0].lines()[0].kind != LineRecipient {
		t.Fatalf("shares: %+v", shares)
	}
}

// A fee its payers' shares cannot cover is shared by everyone, in proportion.
func TestAFeeThePayersCannotCover(t *testing.T) {
	amount, _ := money.New(300, money.BRL)
	fee, _ := money.New(200, money.BRL)
	shares, err := divide(amount, fee, []payments.SplitRule{
		{Recipient: "rp_a", Amount: 100, Liable: true, Remainder: true, ChargeFee: true}, {Recipient: "rp_b", Amount: 200},
	}, "rp_a")
	if err != nil || shares[0].fee != 67 || shares[1].fee != 133 {
		t.Fatalf("shares %+v, %v", shares, err)
	}
	tooBig, _ := money.New(301, money.BRL)
	if _, err := divide(amount, tooBig, []payments.SplitRule{{Recipient: "rp_a", Amount: 1, Liable: true, Remainder: true, ChargeFee: true}}, "rp_a"); err == nil {
		t.Fatal("a fee above the payment")
	}
}
