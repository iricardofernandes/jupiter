// Package taxid checks Brazilian taxpayer numbers.
package taxid

// Valid reports whether s is a Brazilian taxpayer number with valid check digits: a
// CPF of 11 digits, or a CNPJ of 14 characters, whose first 12 may since 2026 be
// letters as well as digits. A character counts as its ASCII code less 48, which for a
// digit is its value.
func Valid(s string) bool {
	switch len(s) {
	case 11:
		return digitsOnly(s) && !repeated(s) && checkDigits(s, 11, []int{10, 9, 8, 7, 6, 5, 4, 3, 2}, []int{11, 10, 9, 8, 7, 6, 5, 4, 3, 2})
	case 14:
		for i, r := range s {
			if (r < '0' || r > '9') && (i >= 12 || r < 'A' || r > 'Z') {
				return false
			}
		}
		return !repeated(s) && checkDigits(s, 14, []int{5, 4, 3, 2, 9, 8, 7, 6, 5, 4, 3, 2}, []int{6, 5, 4, 3, 2, 9, 8, 7, 6, 5, 4, 3, 2})
	}
	return false
}

func checkDigits(s string, n int, first, second []int) bool {
	dv := func(weights []int) byte {
		sum := 0
		for i, w := range weights {
			sum += int(s[i]-'0') * w
		}
		if r := sum % 11; r >= 2 {
			return byte('0' + 11 - r)
		}
		return '0'
	}
	return s[n-2] == dv(first) && s[n-1] == dv(second)
}

func digitsOnly(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func repeated(s string) bool {
	for i := range len(s) {
		if s[i] != s[0] {
			return false
		}
	}
	return true
}
