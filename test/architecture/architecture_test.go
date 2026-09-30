package architecture_test

import (
	"slices"
	"testing"

	"github.com/iricardofernandes/jupiter/test/architecture"
)

// TestRepository is the check that guards Jupiter itself.
func TestRepository(t *testing.T) {
	pkgs, err := architecture.Load(t.Context(), "../..")
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) == 0 {
		t.Fatal("no packages loaded; the check would pass vacuously")
	}
	for _, v := range architecture.Check(pkgs) {
		t.Error(v)
	}
}

// TestCatchesAForbiddenImport runs the same loader and rules against a fixture module
// that reaches past another module's public interface, once in plain code and once in a
// test file behind a build tag.
func TestCatchesAForbiddenImport(t *testing.T) {
	pkgs, err := architecture.Load(t.Context(), "testdata/violating")
	if err != nil {
		t.Fatal(err)
	}
	want := []architecture.Violation{
		{Rule: "module-boundary", From: "internal/payments", To: "internal/ledger/store"},
		// Behind the integration build tag: invisible to a plain `go list`.
		{Rule: "module-boundary", From: "internal/refunds", To: "internal/ledger/store"},
	}
	if got := architecture.Check(pkgs); !slices.Equal(got, want) {
		t.Fatalf("Check = %v, want %v", got, want)
	}
}

func TestRules(t *testing.T) {
	tests := []struct {
		from, to string
		rule     string // "" when the import is allowed
	}{
		// module-boundary
		{"internal/payments", "internal/ledger", ""},
		{"internal/payments", "internal/ledger/store", "module-boundary"},
		{"internal/ledger/store", "internal/ledger/store/queries", ""},
		{"internal/ledger", "internal/ledger/store", ""},
		{"cmd/api", "internal/ledger", ""},
		{"cmd/api", "internal/ledger/store", "module-boundary"},
		{"cmd/api", "internal/vault", ""},
		{"cmd/api", "internal/vault/server", "module-boundary"},
		{"internal/payments/intents", "internal/money", ""},
		{"internal/payments/intents", "internal/platform/postgres", ""},

		// shared-kernel
		{"internal/money", "internal/ledger", "shared-kernel"},
		{"internal/platform/service", "internal/vault", "shared-kernel"},
		{"internal/platform/service", "internal/id", ""},

		// vault-isolation
		{"internal/vault/server", "internal/ledger", "vault-isolation"},
		{"cmd/vault", "internal/vault/server", ""},
		{"internal/vault/server", "internal/platform/service", ""},
		{"internal/vault/server", "internal/money", ""},
		{"internal/vault/server", "pkg/cnab240", ""},
		{"internal/vault/server", "test/e2e", "vault-isolation"},
		{"cmd/vault", "internal/vault", ""},

		// simulator-isolation
		{"internal/sim/pix/spi", "internal/sim/pix/dict", ""},
		{"cmd/sim-pix", "internal/sim/pix", ""},
		{"cmd/sim-pix", "internal/sim/bank", "simulator-isolation"},
		{"internal/sim/pix", "internal/payments", "simulator-isolation"},
		{"internal/sim/pix", "internal/money", "simulator-isolation"},
		{"internal/sim/pix", "internal/platform/service", ""},
		{"internal/sim/pix", "pkg/brcode", ""},
		{"internal/sim/pix", "internal/sim", "simulator-isolation"},
		{"internal/payments", "internal/sim", "simulator-isolation"},
		{"internal/payments/pix", "internal/sim/pix", "simulator-isolation"},
		{"cmd/api", "internal/sim/pix", "simulator-isolation"},

		// pkg-independence
		{"pkg/cnab240", "internal/money", "pkg-independence"},
		{"pkg/cnab240", "pkg/cnab240/segments", ""},

		// test/ composes everything
		{"test/e2e", "internal/sim/pix", ""},
		{"test/e2e", "internal/ledger/store", ""},
	}
	for _, tt := range tests {
		got := architecture.Check([]architecture.Package{{Path: tt.from, Imports: []string{tt.to}}})
		switch {
		case tt.rule == "" && len(got) != 0:
			t.Errorf("%s → %s: unexpected %v", tt.from, tt.to, got)
		case tt.rule != "" && (len(got) != 1 || got[0].Rule != tt.rule):
			t.Errorf("%s → %s: got %v, want rule %s", tt.from, tt.to, got, tt.rule)
		}
	}
}
