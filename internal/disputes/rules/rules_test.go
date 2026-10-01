package rules

import (
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestDeadlines(t *testing.T) {
	now := at("2026-10-01T15:00:00Z")
	if d := For("mastercard", Represent, now); d.Length != 45 || d.Verified != "cited" {
		t.Fatalf("mastercard represent = %+v", d)
	}
	if d := For("elo", Represent, now); d.Network != "*" || d.Length != 30 {
		t.Fatalf("elo falls back to *: %+v", d)
	}
	if due := For("visa", Represent, now).Due(now); !due.Equal(at("2026-10-31T15:00:00Z")) {
		t.Fatalf("visa represent due %s", due)
	}
	if due := For(Pix, MEDNotification, now).Due(now); !due.Equal(at("2026-10-01T15:30:00Z")) {
		t.Fatalf("notification due %s", due)
	}
	if margin := For("visa", MerchantMargin, now); !margin.Before(at("2026-10-31T15:00:00Z")).Equal(at("2026-10-29T15:00:00Z")) {
		t.Fatalf("margin = %+v", margin)
	}
}

func TestEffectiveDates(t *testing.T) {
	if d := For(Pix, MEDContestation, at("2026-08-31T12:00:00Z")); d.Length != 30 {
		t.Fatalf("contestation before IN 766 = %+v", d)
	}
	if d := For(Pix, MEDContestation, at("2026-09-01T12:00:00Z")); d.Length != 80 {
		t.Fatalf("contestation after IN 766 = %+v", d)
	}
	if Has("visa", LiabilityCap, at("2026-05-10T12:00:00Z")) || !Has("visa", LiabilityCap, at("2026-05-11T00:00:00Z")) {
		t.Fatal("the liability cap applies from 11 May 2026")
	}
	if th := ThresholdFor(DisputeRatio, at("2026-10-01T00:00:00Z")); th.Bps != 150 || th.MinimumCount != 1500 {
		t.Fatalf("dispute ratio threshold = %+v", th)
	}
}

func TestMalformedTables(t *testing.T) {
	header := "network,stage,effective_from,length,unit,verified,source\n"
	for name, data := range map[string]string{
		"columns":  "network,days\n*,30\n",
		"unit":     header + "*,represent,2000-01-01,3,weeks,cited,x\n",
		"length":   header + "*,represent,2000-01-01,0,calendar,cited,x\n",
		"verified": header + "*,represent,2000-01-01,3,calendar,maybe,x\n",
	} {
		if _, err := parseDeadlines(data); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	if _, err := parseThresholds("program,effective_from,threshold_bps,minimum_count,verified,source\nx,2000-01-01,20000,1,cited,y\n"); err == nil {
		t.Error("a threshold above 100% parsed")
	}
}
