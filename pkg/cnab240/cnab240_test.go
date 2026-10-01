package cnab240_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/iricardofernandes/jupiter/pkg/cnab240"
)

// record builds a 240-character record by FEBRABAN's positions, independently of the
// package: each value placed at its 1-based start.
func record(fields map[int]string) string {
	b := []byte(strings.Repeat(" ", cnab240.LineLength))
	for start, v := range fields {
		copy(b[start-1:], v)
	}
	return string(b)
}

// The examples are one of each record, laid out from the layout's field tables (v10.11).
var (
	fileHeader = record(map[int]string{
		1: "999", 4: "0000", 8: "0", 18: "2", 19: "11222333000181", 33: "CONVENIO-0001",
		53: "00001", 58: "0", 59: "000000123456", 71: "7", 72: " ", 73: "JUPITER PAGAMENTOS", 103: "BANCO SIMULADO",
		143: "1", 144: "01102026", 152: "093000", 158: "000001", 164: "103", 167: "01600",
	})
	batchHeader = record(map[int]string{
		1: "999", 4: "0001", 8: "1", 9: "R", 10: "01", 14: "060", 18: "2", 19: "011222333000181", 34: "CONVENIO-0001",
		54: "00001", 59: "0", 60: "000000123456", 72: "7", 74: "JUPITER PAGAMENTOS", 104: "PAGAVEL EM QUALQUER BANCO",
		184: "00000001", 192: "01102026", 200: "00000000",
	})
	segmentP = record(map[int]string{
		1: "999", 4: "0001", 8: "3", 9: "00001", 14: "P", 16: "01", 18: "00001", 23: "0", 24: "000000123456", 36: "7",
		38: "000000000013", 58: "1", 59: "1", 60: "2", 61: "2", 62: "P", 63: "PI-0001", 78: "15102026",
		86: "000000000060000", 101: "00000", 107: "02", 109: "N", 110: "01102026", 118: "3", 119: "00000000",
		127: "000000000000000", 142: "0", 143: "00000000", 151: "000000000000000", 166: "000000000000000",
		181: "000000000000000", 196: "pi_01m3v", 221: "3", 222: "00", 224: "1", 225: "030", 228: "09", 230: "0000000000",
	})
	segmentQ = record(map[int]string{
		1: "999", 4: "0001", 8: "3", 9: "00002", 14: "Q", 16: "01", 18: "1", 19: "000012345678909", 34: "MARIA DA SILVA",
		74: "RUA DAS FLORES 100", 114: "CENTRO", 129: "01001", 134: "000", 137: "SAO PAULO", 152: "SP", 154: "0",
		155: "000000000000000", 210: "000",
	})
	segmentY = record(map[int]string{
		1: "999", 4: "0001", 8: "3", 9: "00003", 14: "Y", 16: "61", 18: "03", 81: "0",
		82: "pix.bancosimulado.example/qr/v2/cobv/7f3a", 159: "PI01M3VBOLETO0001",
	})
	segmentT = record(map[int]string{
		1: "999", 4: "0001", 8: "3", 9: "00001", 14: "T", 16: "06", 18: "00001", 23: "0", 24: "000000123456", 36: "7",
		38: "000000000013", 58: "1", 59: "PI-0001", 74: "15102026", 82: "000000000060000", 97: "999", 100: "00001",
		106: "pi_01m3v", 131: "09", 133: "1", 134: "000012345678909", 149: "MARIA DA SILVA", 189: "0000000000",
		199: "000000000000000", 214: "61",
	})
	segmentU = record(map[int]string{
		1: "999", 4: "0001", 8: "3", 9: "00002", 14: "U", 16: "06", 18: "000000000000000", 33: "000000000000000",
		48: "000000000000000", 63: "000000000000000", 78: "000000000060000", 93: "000000000060000", 108: "000000000000000",
		123: "000000000000000", 138: "14102026", 146: "15102026", 158: "00000000", 166: "000000000000000", 211: "000",
	})
	batchTrailer = func(batch, records string) string {
		return record(map[int]string{
			1: "999", 4: batch, 8: "5", 18: records, 24: "000001", 30: "00000000000060000", 47: "000000",
			53: "00000000000000000", 70: "000000", 76: "00000000000000000", 93: "000000", 99: "00000000000000000",
		})
	}
	fileTrailer = func(records string) string {
		return record(map[int]string{1: "999", 4: "99999", 18: "000001", 24: records, 30: "000000"})
	}
)

func file(lines ...string) []byte { return []byte(strings.Join(lines, "\r\n") + "\r\n") }

func TestTheLayoutsRemittance(t *testing.T) {
	data := file(fileHeader, batchHeader, segmentP, segmentQ, segmentY, batchTrailer("0001", "000005"), fileTrailer("000007"))
	f, err := cnab240.ParseRemittance(data)
	if err != nil {
		t.Fatal(err)
	}
	h, p, q, y := f.Header, f.Batches[0].Titles[0].P, f.Batches[0].Titles[0].Q, f.Batches[0].Titles[0].Y03
	switch {
	case h.Bank != "999" || h.Doc != "11222333000181" || h.Account.Number != "000000123456" || h.Sequence != 1 ||
		!h.Generated.Equal(time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC)):
		t.Fatalf("file header: %+v", h)
	case p.Movement != cnab240.Entry || p.OurNumber != "000000000013" || p.Amount != 60000 || p.Distribution != "P" ||
		!p.Due.Equal(time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)) || p.WriteOffDays != 30 || p.Currency != 9:
		t.Fatalf("segment P: %+v", p)
	case q.Payer.Doc != "000012345678909" || q.Payer.Name != "MARIA DA SILVA" || q.State != "SP":
		t.Fatalf("segment Q: %+v", q)
	case y == nil || y.Movement != cnab240.PixQRCodeMaintenance || y.TxID != "PI01M3VBOLETO0001":
		t.Fatalf("segment Y-03: %+v", y)
	}
	again, err := f.Bytes()
	if err != nil || !bytes.Equal(again, data) {
		t.Fatalf("written back differently, %v:\n%s\n%s", err, again, data)
	}
}

func TestTheLayoutsReturn(t *testing.T) {
	ret := strings.Replace(fileHeader, "1"+"01102026", "2"+"01102026", 1)
	header := strings.Replace(batchHeader, "R01", "T01", 1)
	data := file(ret, header, segmentT, segmentU, batchTrailer("0001", "000004"), fileTrailer("000006"))
	f, err := cnab240.ParseReturn(data)
	if err != nil {
		t.Fatal(err)
	}
	title := f.Batches[0].Titles[0]
	if !title.T.Occurrence.Paid() || title.T.Reasons[0] != cnab240.ChannelPix || title.U.Paid != 60000 ||
		!title.U.CreditOn.Equal(time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("title: %+v", title)
	}
	again, err := f.Bytes()
	if err != nil || !bytes.Equal(again, data) {
		t.Fatalf("written back differently, %v", err)
	}
}

func TestBrokenFiles(t *testing.T) {
	good := []string{fileHeader, batchHeader, segmentP, segmentQ, batchTrailer("0001", "000004"), fileTrailer("000006")}
	for name, lines := range map[string][]string{
		"a short record":           {fileHeader[:239], batchHeader},
		"a Q without its P":        {fileHeader, batchHeader, segmentQ, batchTrailer("0001", "000003"), fileTrailer("000005")},
		"a wrong record count":     {fileHeader, batchHeader, segmentP, segmentQ, batchTrailer("0001", "000005"), fileTrailer("000006")},
		"a wrong file count":       {fileHeader, batchHeader, segmentP, segmentQ, batchTrailer("0001", "000004"), fileTrailer("000007")},
		"no trailer":               good[:5],
		"letters in a number":      {fileHeader, batchHeader, strings.Replace(segmentP, "000000000060000", "0000000000600X0", 1), segmentQ, good[4], good[5]},
		"a return as a remittance": {strings.Replace(fileHeader, "1"+"01102026", "2"+"01102026", 1), batchHeader, segmentP, segmentQ, good[4], good[5]},
	} {
		if _, err := cnab240.ParseRemittance(file(lines...)); !errors.Is(err, cnab240.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestWritingRefusesWhatDoesNotFit(t *testing.T) {
	f := cnab240.RemittanceFile{
		Header: cnab240.FileHeader{Bank: "999", DocType: cnab240.CNPJ, Doc: "11222333000181", CompanyName: "JUPITER"},
		Batches: []cnab240.RemittanceBatch{{Titles: []cnab240.RemittanceTitle{{
			P: cnab240.SegmentP{Movement: cnab240.Entry, OurNumber: "000000000013", Amount: 100, DocumentNumber: "A DOCUMENT NUMBER TOO LONG"},
		}}}},
	}
	if _, err := f.Bytes(); !errors.Is(err, cnab240.ErrInvalid) {
		t.Fatalf("a field too long: %v", err)
	}
	f.Batches[0].Titles[0].P.DocumentNumber = "ÇÃO"
	if _, err := f.Bytes(); !errors.Is(err, cnab240.ErrInvalid) {
		t.Fatalf("a field not ASCII: %v", err)
	}
}

func TestTheFebrabanProfile(t *testing.T) {
	p := cnab240.Febraban{BankCode: "999", BankName: "BANCO SIMULADO"}
	n, err := p.OurNumber(13)
	if err != nil || n != "000000000132" {
		t.Fatalf("nosso número %s, %v", n, err)
	}
	free, err := p.FreeField(cnab240.Account{Branch: "00001", Number: "000000123456"}, n)
	if err != nil || free != "0001000000000132001234561" {
		t.Fatalf("free field %s, %v", free, err)
	}
}

func FuzzParseRemittance(f *testing.F) {
	f.Add(file(fileHeader, batchHeader, segmentP, segmentQ, segmentY, batchTrailer("0001", "000005"), fileTrailer("000007")))
	f.Add([]byte(""))
	f.Fuzz(func(t *testing.T, data []byte) {
		parsed, err := cnab240.ParseRemittance(data)
		if err != nil {
			return
		}
		again, err := parsed.Bytes()
		if err != nil {
			t.Fatalf("a file read cannot be written: %v", err)
		}
		if _, err := cnab240.ParseRemittance(again); err != nil {
			t.Fatalf("a file written cannot be read: %v", err)
		}
	})
}

func FuzzParseReturn(f *testing.F) {
	ret := strings.Replace(fileHeader, "1"+"01102026", "2"+"01102026", 1)
	f.Add(file(ret, strings.Replace(batchHeader, "R01", "T01", 1), segmentT, segmentU, batchTrailer("0001", "000004"), fileTrailer("000006")))
	f.Fuzz(func(t *testing.T, data []byte) {
		parsed, err := cnab240.ParseReturn(data)
		if err != nil {
			return
		}
		again, err := parsed.Bytes()
		if err != nil {
			t.Fatalf("a file read cannot be written: %v", err)
		}
		if _, err := cnab240.ParseReturn(again); err != nil {
			t.Fatalf("a file written cannot be read: %v", err)
		}
	})
}
