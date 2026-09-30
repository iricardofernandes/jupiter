package brcode_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/iricardofernandes/jupiter/pkg/brcode"
)

// The BR Codes printed in the Manual de Padrões para Iniciação do Pix v2.10.0: static
// (§2.6.3), dynamic (§2.7.2) and the three composite codes with recurrence (§2.8.5).
var manual = map[string]struct {
	code string
	kind brcode.Kind
}{
	"static": {
		"00020126580014br.gov.bcb.pix0136123e4567-e12b-12d1-a456-4266554400005204000053039865802BR5913Fulano de Tal6008BRASILIA62070503***63041D3D",
		brcode.Static,
	},
	"dynamic": {
		"00020101021226700014br.gov.bcb.pix2548pix.example.com/8b3da2f39a4140d1a91abd93113bd4415204000053039865802BR5913Fulano de Tal6008BRASILIA62070503***630464E4",
		brcode.Dynamic,
	},
	"recurrence only": {
		"00020126180014br.gov.bcb.pix5204000053039865802BR5913Fulano de Tal6008BRASILIA62070503***80740014br.gov.bcb.pix2552pix.example.com/rec/2353c790eefb11eaadc10242ac1200026304F2DA",
		brcode.Composite,
	},
	"static with recurrence": {
		"00020126580014br.gov.bcb.pix0136123e4567-e12b-12d1-a456-4266554400005204000053039865406100.505802BR5913Fulano de Tal6008BRASILIA62070503***80740014br.gov.bcb.pix2552pix.example.com/rec/2353c790eefb11eaadc10242ac12000263042875",
		brcode.Composite,
	},
	"dynamic with recurrence": {
		"00020101021226700014br.gov.bcb.pix2548pix.example.com/8b3da2f39a4140d1a91abd93113bd4415204000053039865802BR5913Fulano de Tal6008BRASILIA62070503***80740014br.gov.bcb.pix2552pix.example.com/rec/2353c790eefb11eaadc10242ac1200026304FB42",
		brcode.Composite,
	},
}

func TestTheManualsExamplesParseAndRoundTrip(t *testing.T) {
	for name, example := range manual {
		t.Run(name, func(t *testing.T) {
			p, err := brcode.Parse(example.code)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if p.Kind() != example.kind {
				t.Errorf("kind %v, want %v", p.Kind(), example.kind)
			}
			again, err := p.Encode()
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if again != example.code {
				t.Fatalf("round trip:\n got %s\nwant %s", again, example.code)
			}
			fields, err := brcode.Decode(example.code)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if raw := brcode.EncodeFields(fields); raw != example.code {
				t.Fatalf("field round trip:\n got %s\nwant %s", raw, example.code)
			}
		})
	}
}

func TestWhatTheManualsExamplesSay(t *testing.T) {
	static, _ := brcode.Parse(manual["static"].code)
	if static.Key != "123e4567-e12b-12d1-a456-426655440000" || static.Amount != "" || static.TxID != brcode.NoTxID ||
		static.MerchantName != "Fulano de Tal" || static.MerchantCity != "BRASILIA" {
		t.Errorf("static: %+v", static)
	}
	dynamic, _ := brcode.Parse(manual["dynamic"].code)
	if dynamic.URL != "pix.example.com/8b3da2f39a4140d1a91abd93113bd441" || dynamic.PointOfInitiation != brcode.SingleUse {
		t.Errorf("dynamic: %+v", dynamic)
	}
	both, _ := brcode.Parse(manual["static with recurrence"].code)
	if both.Amount != "100.50" || both.RecurrenceURL != "pix.example.com/rec/2353c790eefb11eaadc10242ac120002" {
		t.Errorf("static with recurrence: %+v", both)
	}
}

func TestCRC(t *testing.T) {
	// The manual's footnote: polynomial 0x1021, initial value 0xFFFF, over everything
	// up to and including "6304".
	body := strings.TrimSuffix(manual["static"].code, "1D3D")
	if got := brcode.CRC16(body); got != "1D3D" {
		t.Fatalf("CRC16 = %s, want 1D3D", got)
	}
}

func TestBuildingCodes(t *testing.T) {
	static := brcode.Pix{
		Key: "jupiter@example.com", Amount: brcode.FormatAmount(12345), MerchantName: "Loja Exemplo",
		MerchantCity: "SAO PAULO", TxID: "pedido42",
	}
	code, err := static.Encode()
	if err != nil {
		t.Fatal(err)
	}
	back, err := brcode.Parse(code)
	if err != nil {
		t.Fatalf("%s: %v", code, err)
	}
	if back.Key != static.Key || back.Amount != "123.45" || back.TxID != "pedido42" || back.MCC != "0000" ||
		back.Currency != "986" || back.Country != "BR" || back.Kind() != brcode.Static {
		t.Fatalf("parsed back: %+v", back)
	}

	dynamic := brcode.Pix{
		PointOfInitiation: brcode.SingleUse, URL: "pix.example.com/qr/v2/cobv/2353c790eefb11eaadc10242ac120002",
		MerchantName: "Loja Exemplo", MerchantCity: "SAO PAULO",
	}
	code, err = dynamic.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if back, err := brcode.Parse(code); err != nil || back.URL != dynamic.URL || back.TxID != brcode.NoTxID {
		t.Fatalf("dynamic: %+v, %v", back, err)
	}
}

func TestInvalidCodes(t *testing.T) {
	valid := manual["static"].code
	for name, code := range map[string]string{
		"empty":             "",
		"bad checksum":      strings.TrimSuffix(valid, "1D3D") + "1D3E",
		"no checksum":       strings.TrimSuffix(valid, "63041D3D"),
		"truncated":         valid[:40],
		"not a number":      "00AB01" + valid[6:],
		"not first":         withCRC("0102120002015204000053039865802BR5913Fulano de Tal6008BRASILIA62070503***"),
		"no pix template":   withCRC("0002015204000053039865802BR5913Fulano de Tal6008BRASILIA62070503***"),
		"key and url":       withCRC("00020126620014br.gov.bcb.pix0103abc2533pix.example.com/qr/v2/abcdef123455204000053039865802BR5913Fulano de Tal6008BRASILIA62070503***"),
		"scheme in the url": withCRC("00020101021226550014br.gov.bcb.pix2533https://pix.example.com/qr/v2/abc5204000053039865802BR5913Fulano de Tal6008BRASILIA62070503***"),
		"wrong currency":    withCRC("00020126580014br.gov.bcb.pix0136123e4567-e12b-12d1-a456-4266554400005204000053038405802BR5913Fulano de Tal6008BRASILIA62070503***"),
		"bad txid":          withCRC("00020126580014br.gov.bcb.pix0136123e4567-e12b-12d1-a456-4266554400005204000053039865802BR5913Fulano de Tal6008BRASILIA62080504a-b!"),
		"bad amount":        withCRC("00020126580014br.gov.bcb.pix0136123e4567-e12b-12d1-a456-42665544000052040000530398654041,005802BR5913Fulano de Tal6008BRASILIA62070503***"),
	} {
		if _, err := brcode.Parse(code); !errors.Is(err, brcode.ErrInvalid) {
			t.Errorf("%s: Parse = %v, want ErrInvalid", name, err)
		}
	}
}

func TestEncodeRefusesWhatDoesNotFit(t *testing.T) {
	for name, p := range map[string]brcode.Pix{
		"no key or url": {MerchantName: "A", MerchantCity: "B"},
		"long name":     {Key: "k", MerchantName: strings.Repeat("a", 26), MerchantCity: "B"},
		"long city":     {Key: "k", MerchantName: "A", MerchantCity: strings.Repeat("b", 16)},
		"long url":      {URL: "pix.example.com/" + strings.Repeat("a", 62), MerchantName: "A", MerchantCity: "B"},
		"long txid":     {Key: "k", MerchantName: "A", MerchantCity: "B", TxID: strings.Repeat("a", 26)},
		"template full": {Key: strings.Repeat("k", 77), AdditionalInfo: "some more", MerchantName: "A", MerchantCity: "B"},
	} {
		if _, err := p.Encode(); !errors.Is(err, brcode.ErrInvalid) {
			t.Errorf("%s: Encode = %v, want ErrInvalid", name, err)
		}
	}
}

func TestFormatAmount(t *testing.T) {
	for minor, want := range map[int64]string{1: "0.01", 100: "1.00", 60000: "600.00", 12345678: "123456.78"} {
		if got := brcode.FormatAmount(minor); got != want {
			t.Errorf("FormatAmount(%d) = %s, want %s", minor, got, want)
		}
	}
}

func withCRC(body string) string {
	body += "6304"
	return body + brcode.CRC16(body)
}

func FuzzParse(f *testing.F) {
	for _, example := range manual {
		f.Add(example.code)
	}
	f.Fuzz(func(t *testing.T, code string) {
		p, err := brcode.Parse(code)
		if err != nil {
			return
		}
		// Whatever parses encodes, and parses to the same.
		again, err := p.Encode()
		if err != nil {
			t.Fatalf("%q parsed but does not encode: %v", code, err)
		}
		back, err := brcode.Parse(again)
		if err != nil || back != p {
			t.Fatalf("%q: re-encoded %q parses to %+v, %v", code, again, back, err)
		}
	})
}
