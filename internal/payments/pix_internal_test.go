package payments

import "testing"

func TestPixBounds(t *testing.T) {
	for name, c := range map[string]struct {
		opts        *PixOptions
		least, most int64
	}{
		"immediate":  {nil, 10000, 10000},
		"no options": {&PixOptions{ExpiresAfterSeconds: 600}, 10000, 10000},
		// Up to 2% fine and 3% a month for 31 days (3.10), a centavo either side.
		"due": {&PixOptions{Due: &PixDue{DaysAfter: 30, FinePercent: 200, InterestMonthlyPercent: 300, DiscountAmount: 500}}, 9499, 10511},
	} {
		if least, most := pixBounds(10000, c.opts); least != c.least || most != c.most {
			t.Errorf("%s: %d to %d, want %d to %d", name, least, most, c.least, c.most)
		}
	}
}
