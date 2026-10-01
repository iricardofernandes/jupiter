package payments

import (
	"testing"

	"github.com/iricardofernandes/jupiter/internal/money"
)

func TestValidateSplit(t *testing.T) {
	amount, _ := money.New(10000, money.BRL)
	ok := []SplitRule{
		{Recipient: "rp_a", Percentage: "90.00", Liable: true, ChargeFee: true},
		{Recipient: "rp_b", Percentage: "10.00", Remainder: true},
	}
	if err := validateSplit(ok, amount); err != nil {
		t.Fatal(err)
	}
	if err := validateSplit([]SplitRule{{Recipient: "rp_a", Amount: 7000, Liable: true, Remainder: true, ChargeFee: true}, {Recipient: "rp_b", Amount: 3000}}, amount); err != nil {
		t.Fatal(err)
	}
	for name, rules := range map[string][]SplitRule{
		"over 100%":        {{Recipient: "rp_a", Percentage: "90.00", Liable: true, Remainder: true, ChargeFee: true}, {Recipient: "rp_b", Percentage: "10.01"}},
		"amounts exceed":   {{Recipient: "rp_a", Amount: 9000, Liable: true, Remainder: true, ChargeFee: true}, {Recipient: "rp_b", Amount: 1001}},
		"mixed":            {{Recipient: "rp_a", Amount: 9000, Liable: true, Remainder: true, ChargeFee: true}, {Recipient: "rp_b", Percentage: "10.00"}},
		"two liable":       {{Recipient: "rp_a", Percentage: "90.00", Liable: true, Remainder: true, ChargeFee: true}, {Recipient: "rp_b", Percentage: "10.00", Liable: true}},
		"no remainder":     {{Recipient: "rp_a", Percentage: "90.00", Liable: true, ChargeFee: true}, {Recipient: "rp_b", Percentage: "10.00"}},
		"no fee payer":     {{Recipient: "rp_a", Percentage: "90.00", Liable: true, Remainder: true}, {Recipient: "rp_b", Percentage: "10.00"}},
		"fee payers small": {{Recipient: "rp_a", Percentage: "95.00", Liable: true, Remainder: true}, {Recipient: "rp_b", Percentage: "5.00", ChargeFee: true}},
		"same recipient":   {{Recipient: "rp_a", Percentage: "90.00", Liable: true, Remainder: true, ChargeFee: true}, {Recipient: "rp_a", Percentage: "10.00"}},
		"zero share":       {{Recipient: "rp_a", Percentage: "100.00", Liable: true, Remainder: true, ChargeFee: true}, {Recipient: "rp_b", Percentage: "0.00"}},
		"bad percentage":   {{Recipient: "rp_a", Percentage: "100", Liable: true, Remainder: true, ChargeFee: true}},
	} {
		if err := validateSplit(rules, amount); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestASplitCannotOverflow(t *testing.T) {
	amount, _ := money.New(10000, money.BRL)
	third := int64(9223372036854775807 / 3)
	if err := validateSplit([]SplitRule{
		{Recipient: "rp_a", Amount: third, Liable: true, Remainder: true, ChargeFee: true},
		{Recipient: "rp_b", Amount: third},
		{Recipient: "rp_c", Amount: third + 10000 - 3*third},
	}, amount); err == nil {
		t.Fatal("accepted")
	}
}
