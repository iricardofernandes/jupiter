package payments

import "testing"

func TestTheDailyPayoutLimitFromTheEnvironment(t *testing.T) {
	for value, want := range map[string]int64{"": defaultDailyPayoutLimit, "0": 0, "150000": 150000} {
		got, err := DailyPayoutLimit(func(string) string { return value })
		if err != nil || got != want {
			t.Errorf("%q: %d, %v", value, got, err)
		}
	}
	for _, value := range []string{"-1", "R$ 10", "1e6"} {
		if _, err := DailyPayoutLimit(func(string) string { return value }); err == nil {
			t.Errorf("%q was accepted", value)
		}
	}
}
