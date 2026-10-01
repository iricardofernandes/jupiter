package rules_test

import (
	"testing"
	"time"

	"github.com/iricardofernandes/jupiter/internal/payments/rules"
)

func day(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestAuthorizationValidity(t *testing.T) {
	cnp := func(scheme string, initiator rules.Initiator, kind rules.Kind) rules.Authorization {
		return rules.Authorization{Scheme: scheme, Presence: rules.CardNotPresent, Initiator: initiator, Kind: kind}
	}
	tests := []struct {
		name     string
		a        rules.Authorization
		at       string
		days     int
		verified bool
	}{
		{"visa customer-initiated", cnp("visa", rules.Customer, rules.Final), "2026-10-01", 10, true},
		{"visa merchant-initiated", cnp("visa", rules.Merchant, rules.Final), "2026-10-01", 5, true},
		{"visa estimated", cnp("visa", rules.Customer, rules.Estimated), "2026-10-01", 30, true},
		{"visa card present", rules.Authorization{Scheme: "visa", Presence: rules.CardPresent, Initiator: rules.Customer, Kind: rules.Final}, "2026-10-01", 5, true},
		// The rule in effect when the payment was authorized, not today's.
		{"visa before the 2024 framework", cnp("visa", rules.Customer, rules.Final), "2024-04-12", 7, false},
		{"visa on the day it changed", cnp("visa", rules.Customer, rules.Final), "2024-04-13", 10, true},
		{"mastercard final", cnp("mastercard", rules.Merchant, rules.Final), "2026-10-01", 7, false},
		{"mastercard pre-authorization", cnp("mastercard", rules.Customer, rules.Estimated), "2026-10-01", 30, false},
		{"elo, which has no rule of its own", cnp("elo", rules.Customer, rules.Final), "2026-10-01", 7, false},
	}
	for _, tt := range tests {
		v := rules.AuthorizationValidity(tt.a, day(tt.at))
		if v.Duration != time.Duration(tt.days)*24*time.Hour || v.Verified != tt.verified || v.Source == "" {
			t.Errorf("%s: %v (verified %t, %q), want %d days (verified %t)", tt.name, v.Duration, v.Verified, v.Source, tt.days, tt.verified)
		}
	}
}

func TestCardFee(t *testing.T) {
	at, _ := time.Parse(time.DateOnly, "2026-10-01")
	for _, c := range []struct {
		financedBy   string
		installments int
		want         string
	}{
		{"", 1, "0.0299"}, {"merchant", 6, "0.0349"}, {"merchant", 7, "0.0399"}, {"issuer", 12, "0.0299"},
	} {
		f, ok := rules.CardFee("visa", c.financedBy, c.installments, at)
		if !ok || f.Rate.String() != c.want {
			t.Errorf("%s %d: %v %t", c.financedBy, c.installments, f.Rate, ok)
		}
	}
	if _, ok := rules.CardFee("visa", "merchant", 13, at); ok {
		t.Error("13 installments has a price")
	}
}
