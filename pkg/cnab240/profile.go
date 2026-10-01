package cnab240

import (
	"fmt"
	"strconv"
)

// BankProfile is what one bank does its own way within the FEBRABAN layout: its code
// and name, how it numbers titles (nosso número), and the boleto's free field.
type BankProfile interface {
	Code() string
	Name() string
	// OurNumber formats the company's title sequence as the bank's nosso número, check
	// digit included.
	OurNumber(sequence int64) (string, error)
	// FreeField is the boleto barcode's 25-digit free field for a title.
	FreeField(account Account, ourNumber string) (string, error)
}

// Febraban is a profile of the layout as published, for a bank with no variations of its
// own: an 11-digit nosso número with a mod 11 check digit, and a free field of the
// branch (4), the nosso número (11 and its digit) and the account (8), and a 1 for
// collection with registration.
type Febraban struct {
	BankCode string
	BankName string
}

func (f Febraban) Code() string { return f.BankCode }
func (f Febraban) Name() string { return f.BankName }

const maxOurNumber = 99_999_999_999

func (f Febraban) OurNumber(sequence int64) (string, error) {
	if sequence <= 0 || sequence > maxOurNumber {
		return "", fmt.Errorf("%w: nosso número %d", ErrInvalid, sequence)
	}
	s := fmt.Sprintf("%011d", sequence)
	return s + strconv.Itoa(mod11(s)), nil
}

func (f Febraban) FreeField(account Account, ourNumber string) (string, error) {
	branch, err := strconv.ParseInt(account.Branch, 10, 64)
	if err != nil || branch > 9999 {
		return "", fmt.Errorf("%w: branch %q", ErrInvalid, account.Branch)
	}
	number, err := strconv.ParseInt(account.Number, 10, 64)
	if err != nil || number > 99_999_999 {
		return "", fmt.Errorf("%w: account %q", ErrInvalid, account.Number)
	}
	if len(ourNumber) != 12 {
		return "", fmt.Errorf("%w: nosso número %q", ErrInvalid, ourNumber)
	}
	free := fmt.Sprintf("%04d%s%08d1", branch, ourNumber, number)
	if len(free) != 25 {
		return "", fmt.Errorf("%w: free field %q", ErrInvalid, free)
	}
	return free, nil
}

// mod11 is a check digit with weights 2 to 9 from the right; 0 for a remainder of 0 or 1.
func mod11(s string) int {
	sum, weight := 0, 2
	for i := len(s) - 1; i >= 0; i-- {
		sum += int(s[i]-'0') * weight
		if weight++; weight > 9 {
			weight = 2
		}
	}
	if r := sum % 11; r > 1 {
		return 11 - r
	}
	return 0
}
