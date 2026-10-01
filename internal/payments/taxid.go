package payments

import "github.com/iricardofernandes/jupiter/pkg/taxid"

// ValidTaxID reports whether s is a CPF or CNPJ with valid check digits.
func ValidTaxID(s string) bool { return taxid.Valid(s) }
