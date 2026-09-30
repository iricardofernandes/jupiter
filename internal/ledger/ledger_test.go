package ledger

import (
	"errors"
	"slices"
	"testing"

	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/money"
)

func brl(t *testing.T, minor int64) money.Amount {
	t.Helper()
	a, err := money.New(minor, money.BRL)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func account(t *testing.T, raw string, normal Normal) Account {
	t.Helper()
	var u [16]byte
	copy(u[:], raw)
	return Account{
		ID:          AccountPrefix.FromUUID(u),
		AccountSpec: AccountSpec{Book: ClientFunds, Code: "test", Currency: money.BRL, Normal: normal},
	}
}

func TestSortForLockingOrdersByAccountThenLayerThenLoosening(t *testing.T) {
	a := account(t, "a", CreditNormal)
	b := account(t, "b", DebitNormal)
	entries := []draftEntry{
		{account: b, layer: LayerPosted, amount: -5},
		{account: a, layer: LayerPosted, amount: 7},  // lowers a credit-normal account
		{account: a, layer: LayerPosted, amount: -3}, // raises it
		{account: a, layer: LayerPending, amount: -9},
		{account: b, layer: LayerPosted, amount: 4},
	}
	sortForLocking(entries)

	type key struct {
		account id.ID
		layer   Layer
		amount  int64
	}
	got := make([]key, len(entries))
	for i, e := range entries {
		got[i] = key{e.account.ID, e.layer, e.amount}
	}
	want := []key{
		{a.ID, LayerPending, -9},
		{a.ID, LayerPosted, -3},
		{a.ID, LayerPosted, 7},
		{b.ID, LayerPosted, 4},
		{b.ID, LayerPosted, -5},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("order = %v\nwant    %v", got, want)
	}
}

func TestCheckBalancedPerBookCurrencyAndLayer(t *testing.T) {
	a, b := account(t, "a", DebitNormal), account(t, "b", CreditNormal)
	own := account(t, "c", CreditNormal)
	own.Book = OwnFunds

	tests := []struct {
		name    string
		entries []draftEntry
		want    error
	}{
		{"balanced", []draftEntry{{a, LayerPosted, 5}, {b, LayerPosted, -5}}, nil},
		{"off by one", []draftEntry{{a, LayerPosted, 5}, {b, LayerPosted, -4}}, ErrUnbalanced},
		{"balanced only across layers", []draftEntry{{a, LayerPosted, 5}, {b, LayerPending, -5}}, ErrUnbalanced},
		{"balanced only across books", []draftEntry{{a, LayerPosted, 5}, {own, LayerPosted, -5}}, ErrUnbalanced},
		{"overflowing sum", []draftEntry{{a, LayerPosted, 1<<63 - 1}, {a, LayerPosted, 1}}, ErrInvalid},
	}
	for _, tt := range tests {
		if err := checkBalanced(tt.entries); !errors.Is(err, tt.want) && (tt.want != nil || err != nil) {
			t.Errorf("%s: checkBalanced = %v, want %v", tt.name, err, tt.want)
		}
	}
}

func TestAccountSpecValidation(t *testing.T) {
	valid := AccountSpec{Book: ClientFunds, Code: "merchant_balance", Currency: money.BRL, Normal: CreditNormal, NonNegative: true}
	if err := valid.validate(); err != nil {
		t.Fatalf("valid spec: %v", err)
	}
	invalid := map[string]func(*AccountSpec){
		"unknown book":             func(s *AccountSpec) { s.Book = "house" },
		"unknown normal":           func(s *AccountSpec) { s.Normal = "sideways" },
		"code with capitals":       func(s *AccountSpec) { s.Code = "Merchant" },
		"no currency":              func(s *AccountSpec) { s.Currency = money.Currency{} },
		"batched and non-negative": func(s *AccountSpec) { s.Batched = true },
	}
	for name, mutate := range invalid {
		spec := valid
		mutate(&spec)
		if err := spec.validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: validate = %v, want ErrInvalid", name, err)
		}
	}
}

func TestBalanceArithmetic(t *testing.T) {
	credit := Balance{
		Normal:         CreditNormal,
		PostedDebits:   brl(t, 300),
		PostedCredits:  brl(t, 1000),
		PendingDebits:  brl(t, 200),
		PendingCredits: brl(t, 50),
	}
	if posted, err := credit.Posted(); err != nil || posted.Minor() != 700 {
		t.Errorf("credit Posted = %v, %v; want 700", posted, err)
	}
	// Pending credits do not count for a credit-normal account; pending debits count
	// against it.
	if available, err := credit.Available(); err != nil || available.Minor() != 500 {
		t.Errorf("credit Available = %v, %v; want 500", available, err)
	}

	debit := credit
	debit.Normal = DebitNormal
	if posted, err := debit.Posted(); err != nil || posted.Minor() != -700 {
		t.Errorf("debit Posted = %v, %v; want -700", posted, err)
	}
	if available, err := debit.Available(); err != nil || available.Minor() != -750 {
		t.Errorf("debit Available = %v, %v; want -750", available, err)
	}
}

func TestLegRequiresASide(t *testing.T) {
	a := account(t, "a", DebitNormal)
	if _, err := legEntry(Leg{Account: a.ID, Amount: brl(t, 1)}, a); !errors.Is(err, ErrInvalid) {
		t.Fatalf("leg without side: %v, want ErrInvalid", err)
	}
}
