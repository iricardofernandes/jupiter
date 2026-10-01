package cnab240

import (
	"time"
)

// Account is a bank account as CNAB 240 lays it out: branch and account, each with its
// check digit, and a check digit for both.
type Account struct {
	Branch          string // 5
	BranchDV        string // 1
	Number          string // 12
	NumberDV        string // 1
	BranchAccountDV string // 1
}

func (l *line) account(start int, a Account) {
	l.code(start, start+4, a.Branch)
	l.text(start+5, start+5, a.BranchDV)
	l.code(start+6, start+17, a.Number)
	l.text(start+18, start+18, a.NumberDV)
	l.text(start+19, start+19, a.BranchAccountDV)
}

func (r *reader) account(start int) Account {
	return Account{
		Branch: r.code(start, start+4), BranchDV: r.text(start+5, start+5), Number: r.code(start+6, start+17),
		NumberDV: r.text(start+18, start+18), BranchAccountDV: r.text(start+19, start+19),
	}
}

// FileHeader is record type 0.
type FileHeader struct {
	Bank        string
	DocType     int
	Doc         string // 14
	Agreement   string // convênio, 20
	Account     Account
	CompanyName string // 30
	BankName    string // 30
	Code        int    // 1 remittance, 2 return
	Generated   time.Time
	Sequence    int64 // NSA
	Density     int64
	BankUse     string // 20
	CompanyUse  string // 20
}

// FileLayout is the version of the file layout this package writes and reads.
const FileLayout = "103"

func (h FileHeader) encode() (string, error) {
	l := newLine()
	l.code(1, 3, h.Bank)
	l.code(4, 7, "0")
	l.text(8, 8, "0")
	l.num(18, 18, int64(h.DocType))
	l.code(19, 32, h.Doc)
	l.text(33, 52, h.Agreement)
	l.account(53, h.Account)
	l.text(73, 102, h.CompanyName)
	l.text(103, 132, h.BankName)
	l.num(143, 143, int64(h.Code))
	l.date(144, 151, h.Generated)
	l.text(152, 157, h.Generated.Format("150405"))
	l.num(158, 163, h.Sequence)
	l.text(164, 166, FileLayout)
	l.num(167, 171, h.Density)
	l.text(172, 191, h.BankUse)
	l.text(192, 211, h.CompanyUse)
	return l.String(), l.err
}

func decodeFileHeader(s string) (FileHeader, error) {
	r := &reader{s: s}
	r.literal(4, 8, "00000")
	r.blank(9, 17)
	r.blank(133, 142)
	r.literal(164, 166, FileLayout)
	r.blank(212, 240)
	h := FileHeader{
		Bank: r.code(1, 3), DocType: int(r.num(18, 18)), Doc: r.code(19, 32), Agreement: r.text(33, 52), Account: r.account(53),
		CompanyName: r.text(73, 102), BankName: r.text(103, 132), Code: int(r.num(143, 143)), Sequence: r.num(158, 163),
		Density: r.num(167, 171), BankUse: r.text(172, 191), CompanyUse: r.text(192, 211),
	}
	h.Generated = r.date(144, 151)
	if !h.Generated.IsZero() {
		if t, err := time.Parse("150405", r.raw(152, 157)); err == nil {
			h.Generated = h.Generated.Add(time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute + time.Duration(t.Second())*time.Second)
		} else {
			r.fail(152, 157, "%q is not a time", r.raw(152, 157))
		}
	}
	return h, r.err
}

// BatchHeader is record type 1 of a collection batch (cobrança, layout 060).
type BatchHeader struct {
	Bank        string
	Batch       int64
	Operation   byte // R remittance, T return
	DocType     int
	Doc         string // 15
	Agreement   string // 20
	Account     Account
	CompanyName string // 30
	Message1    string // 40
	Message2    string // 40
	Number      int64  // remittance or return number
	Recorded    time.Time
	Credited    time.Time
}

// BatchLayout is the version of the collection batch layout.
const BatchLayout = "060"

func (h BatchHeader) encode() (string, error) {
	l := newLine()
	l.code(1, 3, h.Bank)
	l.num(4, 7, h.Batch)
	l.text(8, 8, "1")
	l.text(9, 9, string(h.Operation))
	l.text(10, 11, "01")
	l.text(14, 16, BatchLayout)
	l.num(18, 18, int64(h.DocType))
	l.code(19, 33, h.Doc)
	l.text(34, 53, h.Agreement)
	l.account(54, h.Account)
	l.text(74, 103, h.CompanyName)
	l.text(104, 143, h.Message1)
	l.text(144, 183, h.Message2)
	l.num(184, 191, h.Number)
	l.date(192, 199, h.Recorded)
	l.date(200, 207, h.Credited)
	return l.String(), l.err
}

func decodeBatchHeader(s string) (BatchHeader, error) {
	r := &reader{s: s}
	r.literal(8, 8, "1")
	r.literal(10, 11, "01")
	r.blank(12, 13)
	r.literal(14, 16, BatchLayout)
	r.blank(17, 17)
	r.blank(208, 240)
	h := BatchHeader{
		Bank: r.code(1, 3), Batch: r.num(4, 7), Operation: r.raw(9, 9)[0], DocType: int(r.num(18, 18)), Doc: r.code(19, 33),
		Agreement: r.text(34, 53), Account: r.account(54), CompanyName: r.text(74, 103), Message1: r.text(104, 143),
		Message2: r.text(144, 183), Number: r.num(184, 191), Recorded: r.date(192, 199), Credited: r.date(200, 207),
	}
	if h.Operation != Remittance && h.Operation != Return {
		r.fail(9, 9, "operation %q", h.Operation)
	}
	return h, r.err
}

// detail writes the start every detail record shares: bank, batch, record type 3, the
// record's sequence in the batch, its segment and the movement or occurrence.
func detail(bank string, batch, seq int64, segment byte, code int) *line {
	l := newLine()
	l.code(1, 3, bank)
	l.num(4, 7, batch)
	l.text(8, 8, "3")
	l.num(9, 13, seq)
	l.text(14, 14, string(segment))
	l.num(16, 17, int64(code))
	return l
}

func readDetail(s string, segment byte) *reader {
	r := &reader{s: s}
	r.literal(8, 8, "3")
	r.literal(14, 14, string(segment))
	r.blank(15, 15)
	return r
}

// SegmentP is a title's main record in a remittance: what is charged, when, and how.
type SegmentP struct {
	Movement       Movement
	Account        Account
	OurNumber      string // nosso número, 20
	Portfolio      int64  // carteira
	Registration   int64  // forma de cadastramento
	DocumentType   string // 1
	IssuedBy       int64  // emissão do boleto: 1 bank, 2 company
	Distribution   string // distribuição: 1 bank, 2 company, P or Q with a Pix QR code
	DocumentNumber string // 15
	Due            time.Time
	Amount         int64
	CollectingAt   string // agência cobradora, 5
	CollectingDV   string
	Kind           int64  // espécie
	Accept         string // A or N
	Issued         time.Time
	InterestCode   int64
	InterestFrom   time.Time
	Interest       int64 // per day, or rate
	DiscountCode   int64
	DiscountUntil  time.Time
	Discount       int64
	IOF            int64
	Rebate         int64
	CompanyUse     string // seu número, 25
	ProtestCode    int64
	ProtestDays    int64
	WriteOffCode   int64
	WriteOffDays   int64
	Currency       int64 // 09 for the real
	Contract       int64
	Partial        string // 1
}

func (p SegmentP) encode(bank string, batch, seq int64) (string, error) {
	l := detail(bank, batch, seq, 'P', int(p.Movement))
	l.account(18, p.Account)
	l.text(38, 57, p.OurNumber)
	l.num(58, 58, p.Portfolio)
	l.num(59, 59, p.Registration)
	l.text(60, 60, p.DocumentType)
	l.num(61, 61, p.IssuedBy)
	l.text(62, 62, p.Distribution)
	l.text(63, 77, p.DocumentNumber)
	l.date(78, 85, p.Due)
	l.num(86, 100, p.Amount)
	l.code(101, 105, p.CollectingAt)
	l.text(106, 106, p.CollectingDV)
	l.num(107, 108, p.Kind)
	l.text(109, 109, p.Accept)
	l.date(110, 117, p.Issued)
	l.num(118, 118, p.InterestCode)
	l.date(119, 126, p.InterestFrom)
	l.num(127, 141, p.Interest)
	l.num(142, 142, p.DiscountCode)
	l.date(143, 150, p.DiscountUntil)
	l.num(151, 165, p.Discount)
	l.num(166, 180, p.IOF)
	l.num(181, 195, p.Rebate)
	l.text(196, 220, p.CompanyUse)
	l.num(221, 221, p.ProtestCode)
	l.num(222, 223, p.ProtestDays)
	l.num(224, 224, p.WriteOffCode)
	l.num(225, 227, p.WriteOffDays)
	l.num(228, 229, p.Currency)
	l.num(230, 239, p.Contract)
	l.text(240, 240, p.Partial)
	return l.String(), l.err
}

func decodeSegmentP(s string) (SegmentP, error) {
	r := readDetail(s, 'P')
	p := SegmentP{
		Movement: Movement(r.num(16, 17)), Account: r.account(18), OurNumber: r.text(38, 57), Portfolio: r.num(58, 58),
		Registration: r.num(59, 59), DocumentType: r.text(60, 60), IssuedBy: r.num(61, 61), Distribution: r.text(62, 62),
		DocumentNumber: r.text(63, 77), Due: r.date(78, 85), Amount: r.num(86, 100), CollectingAt: r.code(101, 105),
		CollectingDV: r.text(106, 106), Kind: r.num(107, 108), Accept: r.text(109, 109), Issued: r.date(110, 117),
		InterestCode: r.num(118, 118), InterestFrom: r.date(119, 126), Interest: r.num(127, 141), DiscountCode: r.num(142, 142),
		DiscountUntil: r.date(143, 150), Discount: r.num(151, 165), IOF: r.num(166, 180), Rebate: r.num(181, 195),
		CompanyUse: r.text(196, 220), ProtestCode: r.num(221, 221), ProtestDays: r.num(222, 223), WriteOffCode: r.num(224, 224),
		WriteOffDays: r.num(225, 227), Currency: r.num(228, 229), Contract: r.num(230, 239), Partial: r.text(240, 240),
	}
	return p, r.err
}

// Party is a payer or a guarantor (sacador/avalista).
type Party struct {
	DocType int
	Doc     string // 15
	Name    string // 40
}

// SegmentQ is a title's payer.
type SegmentQ struct {
	Movement               Movement
	Payer                  Party
	Address                string // 40
	District               string // 15
	ZIP                    string // 5
	ZIPSuffix              string // 3
	City                   string // 15
	State                  string // 2
	Guarantor              Party
	CorrespondentBank      string // 3
	CorrespondentOurNumber string // 20
}

func (q SegmentQ) encode(bank string, batch, seq int64) (string, error) {
	l := detail(bank, batch, seq, 'Q', int(q.Movement))
	l.num(18, 18, int64(q.Payer.DocType))
	l.code(19, 33, q.Payer.Doc)
	l.text(34, 73, q.Payer.Name)
	l.text(74, 113, q.Address)
	l.text(114, 128, q.District)
	l.code(129, 133, q.ZIP)
	l.code(134, 136, q.ZIPSuffix)
	l.text(137, 151, q.City)
	l.text(152, 153, q.State)
	l.num(154, 154, int64(q.Guarantor.DocType))
	l.code(155, 169, q.Guarantor.Doc)
	l.text(170, 209, q.Guarantor.Name)
	l.code(210, 212, q.CorrespondentBank)
	l.text(213, 232, q.CorrespondentOurNumber)
	return l.String(), l.err
}

func decodeSegmentQ(s string) (SegmentQ, error) {
	r := readDetail(s, 'Q')
	r.blank(233, 240)
	q := SegmentQ{
		Movement: Movement(r.num(16, 17)), Payer: Party{DocType: int(r.num(18, 18)), Doc: r.code(19, 33), Name: r.text(34, 73)},
		Address: r.text(74, 113), District: r.text(114, 128), ZIP: r.code(129, 133), ZIPSuffix: r.code(134, 136),
		City: r.text(137, 151), State: r.text(152, 153),
		Guarantor:         Party{DocType: int(r.num(154, 154)), Doc: r.code(155, 169), Name: r.text(170, 209)},
		CorrespondentBank: r.code(210, 212), CorrespondentOurNumber: r.text(213, 232),
	}
	return q, r.err
}

// SegmentY03 carries a hybrid boleto's Pix: the key or the location URL of its QR code,
// and its txid. Positions 20 to 80, which the research did not read, are left blank.
type SegmentY03 struct {
	Movement Movement
	KeyType  int64  // the kind of Pix key; 0 for a location URL
	Key      string // the key, or the URL of the dynamic QR code's location, 77
	TxID     string // 35
}

func (y SegmentY03) encode(bank string, batch, seq int64) (string, error) {
	l := detail(bank, batch, seq, 'Y', int(y.Movement))
	l.text(18, 19, "03")
	l.num(81, 81, y.KeyType)
	l.text(82, 158, y.Key)
	l.text(159, 193, y.TxID)
	return l.String(), l.err
}

func decodeSegmentY03(s string) (SegmentY03, error) {
	r := readDetail(s, 'Y')
	r.literal(18, 19, "03")
	r.blank(20, 80)
	r.blank(194, 240)
	y := SegmentY03{Movement: Movement(r.num(16, 17)), KeyType: r.num(81, 81), Key: r.text(82, 158), TxID: r.text(159, 193)}
	return y, r.err
}

// SegmentT is a title's main record in a return: what happened to it.
type SegmentT struct {
	Occurrence       Occurrence
	Account          Account
	OurNumber        string
	Portfolio        int64
	DocumentNumber   string
	Due              time.Time
	Amount           int64
	CollectingBank   string // 3
	CollectingBranch string // 5
	CollectingDV     string
	CompanyUse       string // 25
	Currency         int64
	Payer            Party
	Contract         int64
	Charges          int64 // tarifa/custas
	Reasons          [5]Reason
}

func (t SegmentT) encode(bank string, batch, seq int64) (string, error) {
	l := detail(bank, batch, seq, 'T', int(t.Occurrence))
	l.account(18, t.Account)
	l.text(38, 57, t.OurNumber)
	l.num(58, 58, t.Portfolio)
	l.text(59, 73, t.DocumentNumber)
	l.date(74, 81, t.Due)
	l.num(82, 96, t.Amount)
	l.code(97, 99, t.CollectingBank)
	l.code(100, 104, t.CollectingBranch)
	l.text(105, 105, t.CollectingDV)
	l.text(106, 130, t.CompanyUse)
	l.num(131, 132, t.Currency)
	l.num(133, 133, int64(t.Payer.DocType))
	l.code(134, 148, t.Payer.Doc)
	l.text(149, 188, t.Payer.Name)
	l.num(189, 198, t.Contract)
	l.num(199, 213, t.Charges)
	for i, reason := range t.Reasons {
		l.text(214+2*i, 215+2*i, string(reason))
	}
	return l.String(), l.err
}

func decodeSegmentT(s string) (SegmentT, error) {
	r := readDetail(s, 'T')
	r.blank(224, 240)
	t := SegmentT{
		Occurrence: Occurrence(r.num(16, 17)), Account: r.account(18), OurNumber: r.text(38, 57), Portfolio: r.num(58, 58),
		DocumentNumber: r.text(59, 73), Due: r.date(74, 81), Amount: r.num(82, 96), CollectingBank: r.code(97, 99),
		CollectingBranch: r.code(100, 104), CollectingDV: r.text(105, 105), CompanyUse: r.text(106, 130), Currency: r.num(131, 132),
		Payer:    Party{DocType: int(r.num(133, 133)), Doc: r.code(134, 148), Name: r.text(149, 188)},
		Contract: r.num(189, 198), Charges: r.num(199, 213),
	}
	for i := range t.Reasons {
		t.Reasons[i] = Reason(r.text(214+2*i, 215+2*i))
	}
	return t, r.err
}

// SegmentU is a title's amounts and dates in a return.
type SegmentU struct {
	Occurrence             Occurrence
	Charges                int64 // juros, multa, encargos
	Discount               int64
	Rebate                 int64
	IOF                    int64
	Paid                   int64 // valor pago
	Credited               int64 // valor líquido creditado
	OtherExpenses          int64
	OtherCredits           int64
	OccurredOn             time.Time
	CreditOn               time.Time
	PayerOccurrence        string // 4
	PayerOccurrenceOn      time.Time
	PayerOccurrenceAmount  int64
	Complement             string // 30
	CorrespondentBank      string // 3
	CorrespondentOurNumber string // 20
}

func (u SegmentU) encode(bank string, batch, seq int64) (string, error) {
	l := detail(bank, batch, seq, 'U', int(u.Occurrence))
	l.num(18, 32, u.Charges)
	l.num(33, 47, u.Discount)
	l.num(48, 62, u.Rebate)
	l.num(63, 77, u.IOF)
	l.num(78, 92, u.Paid)
	l.num(93, 107, u.Credited)
	l.num(108, 122, u.OtherExpenses)
	l.num(123, 137, u.OtherCredits)
	l.date(138, 145, u.OccurredOn)
	l.date(146, 153, u.CreditOn)
	l.text(154, 157, u.PayerOccurrence)
	l.date(158, 165, u.PayerOccurrenceOn)
	l.num(166, 180, u.PayerOccurrenceAmount)
	l.text(181, 210, u.Complement)
	l.code(211, 213, u.CorrespondentBank)
	l.text(214, 233, u.CorrespondentOurNumber)
	return l.String(), l.err
}

func decodeSegmentU(s string) (SegmentU, error) {
	r := readDetail(s, 'U')
	r.blank(234, 240)
	u := SegmentU{
		Occurrence: Occurrence(r.num(16, 17)), Charges: r.num(18, 32), Discount: r.num(33, 47), Rebate: r.num(48, 62),
		IOF: r.num(63, 77), Paid: r.num(78, 92), Credited: r.num(93, 107), OtherExpenses: r.num(108, 122),
		OtherCredits: r.num(123, 137), OccurredOn: r.date(138, 145), CreditOn: r.date(146, 153), PayerOccurrence: r.text(154, 157),
		PayerOccurrenceOn: r.date(158, 165), PayerOccurrenceAmount: r.num(166, 180), Complement: r.text(181, 210),
		CorrespondentBank: r.code(211, 213), CorrespondentOurNumber: r.text(214, 233),
	}
	return u, r.err
}

// BatchTrailer is record type 5 of a collection batch: its record count and, by
// portfolio, how many titles and for how much.
type BatchTrailer struct {
	Records                      int64
	SimpleCount, SimpleTotal     int64
	LinkedCount, LinkedTotal     int64
	SecuredCount, SecuredTotal   int64
	DiscountCount, DiscountTotal int64
	Notice                       string // nº do aviso de lançamento, 8
}

func (t BatchTrailer) encode(bank string, batch int64) (string, error) {
	l := newLine()
	l.code(1, 3, bank)
	l.num(4, 7, batch)
	l.text(8, 8, "5")
	l.num(18, 23, t.Records)
	l.num(24, 29, t.SimpleCount)
	l.num(30, 46, t.SimpleTotal)
	l.num(47, 52, t.LinkedCount)
	l.num(53, 69, t.LinkedTotal)
	l.num(70, 75, t.SecuredCount)
	l.num(76, 92, t.SecuredTotal)
	l.num(93, 98, t.DiscountCount)
	l.num(99, 115, t.DiscountTotal)
	l.text(116, 123, t.Notice)
	return l.String(), l.err
}

func decodeBatchTrailer(s string) (BatchTrailer, error) {
	r := &reader{s: s}
	r.literal(8, 8, "5")
	r.blank(9, 17)
	r.blank(124, 240)
	t := BatchTrailer{
		Records: r.num(18, 23), SimpleCount: r.num(24, 29), SimpleTotal: r.num(30, 46), LinkedCount: r.num(47, 52),
		LinkedTotal: r.num(53, 69), SecuredCount: r.num(70, 75), SecuredTotal: r.num(76, 92), DiscountCount: r.num(93, 98),
		DiscountTotal: r.num(99, 115), Notice: r.text(116, 123),
	}
	return t, r.err
}

// FileTrailer is record type 9: how many batches and records the file has.
type FileTrailer struct {
	Batches  int64
	Records  int64
	Accounts int64
}

func (t FileTrailer) encode(bank string) (string, error) {
	l := newLine()
	l.code(1, 3, bank)
	l.text(4, 8, "99999")
	l.num(18, 23, t.Batches)
	l.num(24, 29, t.Records)
	l.num(30, 35, t.Accounts)
	return l.String(), l.err
}

func decodeFileTrailer(s string) (FileTrailer, error) {
	r := &reader{s: s}
	r.literal(4, 8, "99999")
	r.blank(9, 17)
	r.blank(36, 240)
	t := FileTrailer{Batches: r.num(18, 23), Records: r.num(24, 29), Accounts: r.num(30, 35)}
	return t, r.err
}
