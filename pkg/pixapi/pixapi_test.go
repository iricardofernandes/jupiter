package pixapi_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/iricardofernandes/jupiter/pkg/pixapi"
)

func TestValor(t *testing.T) {
	for minor, s := range map[int64]string{0: "0.00", 1: "0.01", 60000: "600.00", 9999999999: "99999999.99"} {
		if got := pixapi.FormatValor(minor); got != s {
			t.Errorf("FormatValor(%d) = %s", minor, got)
		}
		if got, err := pixapi.ParseValor(s); err != nil || got != minor {
			t.Errorf("ParseValor(%s) = %d, %v", s, got, err)
		}
	}
	for _, bad := range []string{"", "1", "1.0", "1,00", "-1.00", "1.000", "12345678901.00", " 1.00"} {
		if _, err := pixapi.ParseValor(bad); !errors.Is(err, pixapi.ErrValor) {
			t.Errorf("ParseValor(%q) = %v", bad, err)
		}
	}
}

func TestIdentifiers(t *testing.T) {
	if !pixapi.ValidEndToEndID("E12345678202009091221abcdef12345") || pixapi.ValidEndToEndID("E1234567820200909122abcdef12345") {
		t.Error("endToEndId")
	}
	if !pixapi.ValidTxID("pa01m3sw7skseyk9y97z426m10e6") || pixapi.ValidTxID("short") || pixapi.ValidTxID("pa_01m3sw7skseyk9y97z426m10e6") {
		t.Error("txid")
	}
}

func TestNewReachesAnonymousFields(t *testing.T) {
	var cob pixapi.CobCompleta
	cal := pixapi.New(&cob.Calendario)
	cal.Expiracao = 3600
	pix := pixapi.Append(&cob.Pix)
	pix.EndToEndId, pix.Valor = "E12345678202009091221abcdef12345", "10.00"
	raw, err := json.Marshal(cob)
	if err != nil {
		t.Fatal(err)
	}
	var back struct {
		Calendario struct {
			Expiracao int `json:"expiracao"`
		} `json:"calendario"`
		Pix []any `json:"pix"`
	}
	if err := json.Unmarshal(raw, &back); err != nil || back.Calendario.Expiracao != 3600 || len(back.Pix) != 1 {
		t.Fatalf("%s", raw)
	}
}
