package loadgen_test

import (
	mrand "math/rand/v2"
	"testing"

	"github.com/iricardofernandes/jupiter/pkg/loadgen"
)

func TestRandomCardNumbersPassTheirCheckDigit(t *testing.T) {
	rng := mrand.New(mrand.NewPCG(1, 2)) //nolint:gosec // reproducible test data
	for range 1000 {
		n := loadgen.RandomCardNumber(rng, "4")
		if len(n) != 16 || n[0] != '4' || !luhn(n) {
			t.Fatalf("%s", n)
		}
	}
}

func luhn(n string) bool {
	sum := 0
	for i := range len(n) {
		d := int(n[len(n)-1-i] - '0')
		if i%2 == 1 {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return sum%10 == 0
}
