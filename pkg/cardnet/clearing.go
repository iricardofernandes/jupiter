package cardnet

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// A clearing file lists, for one acquirer and one business day, every completion and
// refund the network accepted. It is fixed-width text, one record per line:
//
//	H YYYYMMDD acquirer(11)
//	D kind(1: C completion, R refund) RRN(12) network transaction id(15) amount(12)
//	  currency(3) installments(2) authorization code(6) merchant(15)
//	T records(6) completions total(15) refunds total(15)
type ClearingFile struct {
	BusinessDate time.Time
	AcquirerID   string
	Records      []ClearingRecord
}

type ClearingKind string

const (
	ClearingCompletion ClearingKind = "C"
	ClearingRefund     ClearingKind = "R"
)

type ClearingRecord struct {
	Kind ClearingKind
	// RRN is the retrieval reference number of the completion (0220) or refund (0200).
	RRN                  string
	NetworkTransactionID string
	Amount               int64
	Currency             string
	Installments         int
	AuthorizationCode    string
	MerchantID           string
}

const (
	dateLayout   = "20060102"
	headerLength = 1 + 8 + 11
	detailLength = 1 + 1 + 12 + 15 + 12 + 3 + 2 + 6 + 15
	footerLength = 1 + 6 + 15 + 15
)

var ErrClearingFile = errors.New("cardnet: malformed clearing file")

func (f ClearingFile) Encode() []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "H%s%s\n", f.BusinessDate.Format(dateLayout), PadAcquirer(f.AcquirerID))
	var completions, refunds int64
	for _, r := range f.Records {
		fmt.Fprintf(&b, "D%s%-12s%-15s%012d%3s%02d%-6s%-15s\n", r.Kind, r.RRN, r.NetworkTransactionID, r.Amount,
			r.Currency, r.Installments, r.AuthorizationCode, r.MerchantID)
		if r.Kind == ClearingRefund {
			refunds += r.Amount
		} else {
			completions += r.Amount
		}
	}
	fmt.Fprintf(&b, "T%06d%015d%015d\n", len(f.Records), completions, refunds)
	return b.Bytes()
}

// DecodeClearing reads a clearing file and checks its trailer against its records.
func DecodeClearing(data []byte) (ClearingFile, error) {
	var f ClearingFile
	var completions, refunds int64
	trailer := ""
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for line := 1; scanner.Scan(); line++ {
		text := scanner.Text()
		switch {
		case trailer != "":
			return ClearingFile{}, fmt.Errorf("%w: line %d follows the trailer", ErrClearingFile, line)
		case line == 1:
			if len(text) != headerLength || text[0] != 'H' {
				return ClearingFile{}, fmt.Errorf("%w: no header", ErrClearingFile)
			}
			date, err := time.Parse(dateLayout, text[1:9])
			if err != nil {
				return ClearingFile{}, fmt.Errorf("%w: header date: %w", ErrClearingFile, err)
			}
			f.BusinessDate, f.AcquirerID = date, text[9:]
		case strings.HasPrefix(text, "D"):
			r, err := decodeRecord(text)
			if err != nil {
				return ClearingFile{}, fmt.Errorf("%w: line %d: %w", ErrClearingFile, line, err)
			}
			if r.Kind == ClearingRefund {
				refunds += r.Amount
			} else {
				completions += r.Amount
			}
			f.Records = append(f.Records, r)
		case strings.HasPrefix(text, "T"):
			trailer = text
		default:
			return ClearingFile{}, fmt.Errorf("%w: line %d has no record type", ErrClearingFile, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return ClearingFile{}, err
	}
	want := fmt.Sprintf("T%06d%015d%015d", len(f.Records), completions, refunds)
	if len(trailer) != footerLength || trailer != want {
		return ClearingFile{}, fmt.Errorf("%w: the trailer %q does not match the records (%q)", ErrClearingFile, trailer, want)
	}
	return f, nil
}

func decodeRecord(text string) (ClearingRecord, error) {
	if len(text) != detailLength {
		return ClearingRecord{}, fmt.Errorf("a record has %d characters, want %d", len(text), detailLength)
	}
	r := ClearingRecord{
		Kind: ClearingKind(text[1:2]), RRN: strings.TrimSpace(text[2:14]), NetworkTransactionID: strings.TrimSpace(text[14:29]),
		Currency: text[41:44], AuthorizationCode: strings.TrimSpace(text[46:52]), MerchantID: strings.TrimSpace(text[52:67]),
	}
	if r.Kind != ClearingCompletion && r.Kind != ClearingRefund {
		return ClearingRecord{}, fmt.Errorf("unknown kind %q", r.Kind)
	}
	var err error
	if r.Amount, err = strconv.ParseInt(text[29:41], 10, 64); err != nil || r.Amount <= 0 {
		return ClearingRecord{}, fmt.Errorf("amount %q", text[29:41])
	}
	if r.Installments, err = strconv.Atoi(text[44:46]); err != nil {
		return ClearingRecord{}, fmt.Errorf("installments %q", text[44:46])
	}
	return r, nil
}
