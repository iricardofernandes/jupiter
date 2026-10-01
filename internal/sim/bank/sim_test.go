package bank_test

import (
	"testing"
	"time"

	"github.com/iricardofernandes/jupiter/internal/sim/bank"
	"github.com/iricardofernandes/jupiter/pkg/cnab240"
)

const token = "jupiter bank token"

var account = cnab240.Account{Branch: "00001", BranchDV: "0", Number: "000000123456", NumberDV: "7"}

func newSim(now *time.Time) *bank.Sim {
	return bank.New(bank.Config{Now: func() time.Time { return *now }, Host: "bank.example", Clients: []bank.Client{{
		Token: token, TaxID: "11222333000181", Name: "Jupiter Pagamentos", Agreement: "CONVENIO-0001", Account: account,
	}}})
}

func remittance(t *testing.T, seq int64, titles ...cnab240.RemittanceTitle) []byte {
	t.Helper()
	data, err := cnab240.RemittanceFile{
		Header:  cnab240.FileHeader{Bank: bank.Code, DocType: cnab240.CNPJ, Doc: "11222333000181", Account: account, CompanyName: "JUPITER", Sequence: seq},
		Batches: []cnab240.RemittanceBatch{{Header: cnab240.BatchHeader{DocType: cnab240.CNPJ, Doc: "11222333000181", Account: account}, Titles: titles}},
	}.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func title(ourNumber string, amount int64, due time.Time, hybrid bool) cnab240.RemittanceTitle {
	t := cnab240.RemittanceTitle{
		P: cnab240.SegmentP{Movement: cnab240.Entry, Account: account, OurNumber: ourNumber, Portfolio: 1, Due: due, Amount: amount, WriteOffDays: 5, Currency: 9},
		Q: cnab240.SegmentQ{Movement: cnab240.Entry, Payer: cnab240.Party{DocType: cnab240.CPF, Doc: "12345678909", Name: "MARIA"}},
	}
	if hybrid {
		t.P.Distribution = "P"
	}
	return t
}

func lastReturn(t *testing.T, s *bank.Sim) cnab240.ReturnFile {
	t.Helper()
	files, _ := s.Returns(token, 0)
	data, err := s.ReturnFile(token, files[len(files)-1])
	if err != nil {
		t.Fatal(err)
	}
	f, err := cnab240.ParseReturn(data)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestABoletosLife(t *testing.T) {
	now := time.Date(2026, 10, 1, 15, 0, 0, 0, time.UTC)
	s := newSim(&now)
	due := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	data := remittance(t, 1, title("000000000132", 60000, due, true), title("000000000130", 100, due, false), title("000000000140", 100, due, false))
	if n, err := s.Remit(token, data); err != nil || n != 3 {
		t.Fatalf("remitting: %d, %v", n, err)
	}
	if n, err := s.Remit(token, data); err != nil || n != 0 {
		t.Fatalf("the same remittance again: %d, %v", n, err)
	}
	s.Tick()
	got := lastReturn(t, s).Batches[0].Titles
	if got[0].T.Occurrence != cnab240.EntryConfirmed || got[0].T.Reasons[0] != cnab240.RegisteredWithPix || got[0].Y03 == nil ||
		got[1].T.Occurrence != cnab240.EntryRejected || got[1].T.Reasons[0] != cnab240.InvalidOurNumber {
		t.Fatalf("the return: %+v", got)
	}
	boletos := s.Boletos(token)
	if len(boletos) != 2 {
		t.Fatalf("registered %+v", boletos)
	}
	for _, b := range boletos {
		var err error
		if b.PixCode != "" {
			_, err = s.PayPix(b.PixCode)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	s.Tick()
	paid := lastReturn(t, s).Batches[0].Titles[0]
	if !paid.T.Occurrence.Paid() || paid.T.Reasons[0] != cnab240.ChannelPix || paid.U.Paid != 60000 || paid.U.CreditOn.Format(time.DateOnly) != "2026-10-02" {
		t.Fatalf("the payment: %+v", paid)
	}
	now = due.AddDate(0, 0, 6).Add(15 * time.Hour)
	s.Tick()
	if off := lastReturn(t, s).Batches[0].Titles[0]; off.T.Occurrence != cnab240.WrittenOff || off.T.Reasons[0] != cnab240.WrittenOffByBank {
		t.Fatalf("past its term: %+v", off)
	}
	if entries, _ := s.Statement(token, "2026-10-02"); len(entries) != 1 || entries[0].Amount != 60000 {
		t.Fatalf("the statement: %+v", entries)
	}
}

func TestTransfers(t *testing.T) {
	now := time.Date(2026, 10, 1, 15, 0, 0, 0, time.UTC)
	s := newSim(&now)
	req := bank.TransferRequest{ID: "po_1", Amount: 5000, ISPB: "30000002", Branch: "0001", Account: "888888", TaxID: "12345678909", Name: "MARIA"}
	if tr, err := s.Transfer(token, req); err != nil || tr.Status != bank.TransferProcessing {
		t.Fatalf("a transfer: %+v, %v", tr, err)
	}
	s.Tick()
	if tr, _ := s.TransferStatusOf(token, "po_1"); tr.Status != bank.TransferCompleted {
		t.Fatalf("after a tick: %+v", tr)
	}
	s.Tick()
	if tr, _ := s.TransferStatusOf(token, "po_1"); tr.Status != bank.TransferReturned || tr.Reason != "AC04" {
		t.Fatalf("returned: %+v", tr)
	}
	req.ID, req.Account = "po_2", "999999"
	if tr, _ := s.Transfer(token, req); tr.Status != bank.TransferFailed {
		t.Fatalf("to an account that does not exist: %+v", tr)
	}
	req.Amount = 1
	if _, err := s.Transfer(token, req); err == nil {
		t.Fatal("the same id with another amount")
	}
}
