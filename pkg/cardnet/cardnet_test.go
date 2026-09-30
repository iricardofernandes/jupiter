package cardnet_test

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/moov-io/iso8583"
	"pgregory.net/rapid"

	"github.com/iricardofernandes/jupiter/pkg/cardnet"
)

func authorization() cardnet.Message {
	return cardnet.Message{
		MTI: cardnet.AuthorizationRequest, PAN: "4242424242424242", ProcessingCode: cardnet.ProcessingPurchase,
		Amount: 60000, TransmissionDateTime: "1001120000", STAN: "000123", LocalTime: "090000", LocalDate: "1001",
		Expiry: "3012", MCC: "5999", EntryMode: cardnet.EntryECommerce, AcquirerID: "10000000001",
		RRN: "627412000123", TerminalID: "JUPITER1", MerchantID: "M01JV4N8Q2X7ZK9", Currency: cardnet.CurrencyBRL,
		Installments: "06",
		Private:      &cardnet.PrivateData{CVC: "123", InstallmentFinancing: "M", StoredCredential: cardnet.StoredCredentialInitial},
	}
}

func roundTrip(t *testing.T, m cardnet.Message) (cardnet.Message, []byte) {
	t.Helper()
	msg, err := cardnet.Pack(m)
	if err != nil {
		t.Fatal(err)
	}
	packed, err := msg.Pack()
	if err != nil {
		t.Fatal(err)
	}
	back := iso8583.NewMessage(cardnet.Spec)
	if err := back.Unpack(packed); err != nil {
		t.Fatal(err)
	}
	got, err := cardnet.Unpack(back)
	if err != nil {
		t.Fatal(err)
	}
	return got, packed
}

// The wire format is fixed: a change to the spec that alters the bytes fails here.
func TestAnAuthorizationOnTheWire(t *testing.T) {
	m := authorization()
	got, packed := roundTrip(t, m)
	if fmt.Sprintf("%#v", *got.Private) != fmt.Sprintf("%#v", *m.Private) {
		t.Fatalf("DE 48 = %#v, want %#v", *got.Private, *m.Private)
	}
	got.Private, m.Private = nil, nil
	if got != m {
		t.Fatalf("round trip:\n got %#v\nwant %#v", got, m)
	}
	want := "0100" + "F23C440108C18000" + "2000000000000000" + // MTI, primary and secondary bitmaps
		"164242424242424242" + "000000" + "000000060000" + "1001120000" + "000123" + "090000" + "1001" +
		"3012" + "5999" + "810" + "1110000000001" + "627412000123" + "JUPITER1" + "M01JV4N8Q2X7ZK9" +
		"017" + "CV03123" + "IF01M" + "SC01I" + "986" + "06"
	if string(packed) != want {
		t.Fatalf("packed\n %s\nwant\n %s", packed, want)
	}
}

func TestAReversalCarriesTheOriginal(t *testing.T) {
	auth := authorization()
	original := cardnet.Original(auth)
	reversal := cardnet.Message{
		MTI: cardnet.ReversalAdvice, Amount: auth.Amount, TransmissionDateTime: "1001120030", STAN: "000124",
		AcquirerID: auth.AcquirerID, OriginalData: original.Encode(), Currency: cardnet.CurrencyBRL,
	}
	got, _ := roundTrip(t, reversal)
	parsed, err := cardnet.ParseOriginalData(got.OriginalData)
	if err != nil || parsed != original || parsed.STAN != "000123" || parsed.AcquirerID != "10000000001" {
		t.Fatalf("DE 90 = %+v, %v; want %+v", parsed, err, original)
	}
	for _, bad := range []string{"", "0100", strings.Repeat("x", 42)} {
		if _, err := cardnet.ParseOriginalData(bad); err == nil {
			t.Errorf("ParseOriginalData(%q) succeeded", bad)
		}
	}
}

func TestResponseTo(t *testing.T) {
	for mti, want := range map[string]string{
		"0100": "0110", "0200": "0210", "0220": "0230", "0221": "0230", "0400": "0410", "0420": "0430", "0421": "0430", "0800": "0810", "x": "",
	} {
		if got := cardnet.ResponseTo(mti); got != want {
			t.Errorf("ResponseTo(%s) = %s, want %s", mti, got, want)
		}
	}
}

func TestAMessageNeverPrintsTheCard(t *testing.T) {
	m := authorization()
	out := fmt.Sprintf("%v %+v %#v %s", m, m, m, m) + fmt.Sprint(&m)
	if strings.Contains(out, "4242424242424242") || strings.Contains(out, "CV03123") || !strings.Contains(out, "****4242") {
		t.Fatalf("printed %s", out)
	}
}

func TestFraming(t *testing.T) {
	var b bytes.Buffer
	if _, err := cardnet.WriteLength(&b, 300); err != nil || !bytes.Equal(b.Bytes(), []byte{1, 44}) {
		t.Fatalf("header = %v, %v", b.Bytes(), err)
	}
	if n, err := cardnet.ReadLength(&b); err != nil || n != 300 {
		t.Fatalf("ReadLength = %d, %v", n, err)
	}
	if _, err := cardnet.WriteLength(&b, 70000); err == nil {
		t.Fatal("a length over 65535 was framed")
	}
}

func TestClearingFile(t *testing.T) {
	f := cardnet.ClearingFile{
		BusinessDate: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), AcquirerID: "10000000001",
		Records: []cardnet.ClearingRecord{
			{
				Kind: cardnet.ClearingCompletion, RRN: "627412000124", NetworkTransactionID: "385729104857291", Amount: 60000,
				Currency: "986", Installments: 6, AuthorizationCode: "A1B2C3", MerchantID: "M01JV4N8Q2X7ZK9",
			},
			{
				Kind: cardnet.ClearingRefund, RRN: "627412000130", NetworkTransactionID: "385729104857291", Amount: 1000,
				Currency: "986", MerchantID: "M01JV4N8Q2X7ZK9",
			},
		},
	}
	encoded := f.Encode()
	got, err := cardnet.DecodeClearing(encoded)
	if err != nil || fmt.Sprint(got) != fmt.Sprint(f) {
		t.Fatalf("DecodeClearing = %+v, %v", got, err)
	}
	lines := strings.Split(strings.TrimSpace(string(encoded)), "\n")
	corrupt := map[string]string{
		"no trailer":      strings.Join(lines[:3], "\n"),
		"a changed total": strings.Replace(string(encoded), "000000000060000", "000000000060001", 1),
		"a dropped line":  lines[0] + "\n" + lines[2] + "\n" + lines[3],
		"no header":       strings.Join(lines[1:], "\n"),
		"a short record":  lines[0] + "\n" + lines[1][:40] + "\n" + lines[3],
	}
	for name, data := range corrupt {
		if _, err := cardnet.DecodeClearing([]byte(data)); !errors.Is(err, cardnet.ErrClearingFile) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestClearingRoundTripProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		f := cardnet.ClearingFile{BusinessDate: time.Date(2026, 10, rapid.IntRange(1, 31).Draw(t, "day"), 0, 0, 0, 0, time.UTC), AcquirerID: "00000000042"}
		for range rapid.IntRange(0, 20).Draw(t, "records") {
			f.Records = append(f.Records, cardnet.ClearingRecord{
				Kind:                 rapid.SampledFrom([]cardnet.ClearingKind{cardnet.ClearingCompletion, cardnet.ClearingRefund}).Draw(t, "kind"),
				RRN:                  rapid.StringMatching(`[0-9]{12}`).Draw(t, "rrn"),
				NetworkTransactionID: rapid.StringMatching(`[0-9]{15}`).Draw(t, "nti"),
				Amount:               rapid.Int64Range(1, 999_999_999_999).Draw(t, "amount"),
				Currency:             "986", Installments: rapid.IntRange(0, 12).Draw(t, "installments"),
				AuthorizationCode: rapid.StringMatching(`[A-Z0-9]{0,6}`).Draw(t, "auth"),
				MerchantID:        rapid.StringMatching(`[A-Z0-9]{1,15}`).Draw(t, "merchant"),
			})
		}
		got, err := cardnet.DecodeClearing(f.Encode())
		if err != nil || fmt.Sprint(got) != fmt.Sprint(f) {
			t.Fatalf("round trip: %+v, %v", got, err)
		}
	})
}
