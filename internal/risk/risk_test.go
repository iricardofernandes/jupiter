package risk

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/expr-lang/expr"
)

func TestTheRulesLanguage(t *testing.T) {
	for _, expression := range []string{
		`amount >= 50000 && brand == "elo"`,
		`bin in ["42424242", "55555555"] || !off_session`,
		`ip != '' && ip_cards_24h >= 5`,
		`brand not in ["visa"] and currency == "BRL"`,
		`bin startsWith "4242" || bin endsWith "42" || brand contains "master"`,
		`installments > 6 ? amount > 100000 : amount > 500000`,
		`merchant_decline_ratio_10m * 100 >= 20 && -amount < 0`,
	} {
		if _, err := Compile(expression); err != nil {
			t.Errorf("%s: %v", expression, err)
		}
	}
}

// A variable lets a short expression double a string at every step: 35 steps would
// build 8·2^35 bytes from an 8-digit BIN when a payment is decided.
func TestARuleCannotGrowWithoutBound(t *testing.T) {
	var b strings.Builder
	b.WriteString("let a0 = bin + bin; ")
	for i := 1; i < 35; i++ {
		fmt.Fprintf(&b, "let a%d = a%d + a%d; ", i, i-1, i-1)
	}
	b.WriteString(`a34 == "x"`)
	refused := map[string]string{
		"variables":        b.String(),
		"a sequence":       `amount > 1; amount > 2`,
		"a range":          `amount in 1..10`,
		"matches":          `bin matches "^(4|5)[0-9]{1000}$"`,
		"a function":       `len(bin) > 1`,
		"a builtin":        `all([1, 2], # > 0)`,
		"member access":    `bin[0] == "4"`,
		"a map":            `{"a": 1}.a == 1`,
		"a pointer":        `filter([1], # > 0)[0] == 1`,
		"too many nodes":   strings.Repeat("amount > 1 && ", 120) + "amount > 1",
		"a non-boolean":    `amount + 1`,
		"an unknown field": `cvc == "123"`,
	}
	for what, expression := range refused {
		if _, err := Compile(expression); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s was not refused: %v", what, err)
		}
	}
}

// The largest string a rule can build is bounded by its size: features and literals,
// concatenated at most once each.
func TestConcatenationIsBoundedByTheTree(t *testing.T) {
	expression := strings.Repeat("bin + ", 60) + `bin == ""`
	program, err := Compile(expression)
	if err != nil {
		t.Fatal(err)
	}
	out, err := expr.Run(program, Features{BIN: "42424242"})
	if err != nil || out != false {
		t.Fatalf("%v %v", out, err)
	}
}

func TestValidatingKeepsNothing(t *testing.T) {
	before := programCount()
	for i := range 50 {
		if err := Validate(fmt.Sprintf("amount > %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if programCount() != before {
		t.Fatal("validating rules kept their programs")
	}
}

func TestTheProgramCacheIsBounded(t *testing.T) {
	for i := range maxPrograms + 10 {
		if _, err := Compile(fmt.Sprintf("amount > %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if n := programCount(); n > maxPrograms {
		t.Fatalf("%d programs kept, the cap is %d", n, maxPrograms)
	}
	for _, r := range PlatformRules() {
		if _, err := Compile(r.Expression); err != nil {
			t.Fatal(err)
		}
	}
}

func programCount() int {
	programs.mu.Lock()
	defer programs.mu.Unlock()
	return len(programs.byExpression)
}
