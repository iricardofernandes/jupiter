package taxid_test

import (
	"testing"

	"github.com/iricardofernandes/jupiter/pkg/taxid"
)

func TestValid(t *testing.T) {
	for s, want := range map[string]bool{
		"12345678909":    true,  // CPF
		"12345678900":    false, // wrong check digits
		"11111111111":    false, // repeated digits pass the arithmetic but are not issued
		"11222333000181": true,  // CNPJ
		"11222333000180": false,
		"12ABC34501DE35": true, // alphanumeric CNPJ, the Receita Federal's example
		"12ABC34501DE36": false,
		"1234567890":     false,
		"1234567890a":    false,
		"":               false,
	} {
		if got := taxid.Valid(s); got != want {
			t.Errorf("Valid(%q) = %t, want %t", s, got, want)
		}
	}
}
