// Package cnab240 writes and reads FEBRABAN CNAB 240 collection files (cobrança): the
// remittances a company sends its bank to register boletos and instruct on them, and the
// returns the bank sends back on what happened to each.
//
// A file is a header (record 0), batches (a header 1, detail records 3 by segment, a
// trailer 5) and a trailer (9), every record 240 characters. A remittance's titles are
// segments P and Q, and Y-03 for a hybrid boleto's Pix; a return's are T and U. Movements
// (C004), occurrences (C044) and their reasons (C047) are typed values. What varies by
// bank, the nosso número, the agreement and the boleto's free field, a BankProfile
// supplies.
//
// The layout is FEBRABAN's version 10.11 (file layout 103, batch layout 060), from the
// field tables the research extracted (docs/research, notes 03 §6).
package cnab240

import (
	"errors"
	"fmt"
	"strings"
)

var ErrInvalid = errors.New("cnab240: invalid")

// RemittanceTitle is one title in a remittance.
type RemittanceTitle struct {
	P   SegmentP
	Q   SegmentQ
	Y03 *SegmentY03
}

// ReturnTitle is one title in a return. Y03 carries, for a hybrid boleto the bank
// registered, the location of its Pix QR code.
type ReturnTitle struct {
	T   SegmentT
	U   SegmentU
	Y03 *SegmentY03
}

// RemittanceBatch is a collection batch of a remittance.
type RemittanceBatch struct {
	Header BatchHeader
	Titles []RemittanceTitle
	Notice string
}

// ReturnBatch is a collection batch of a return.
type ReturnBatch struct {
	Header BatchHeader
	Titles []ReturnTitle
	Notice string
}

// RemittanceFile is a remittance.
type RemittanceFile struct {
	Header  FileHeader
	Batches []RemittanceBatch
}

// ReturnFile is a return.
type ReturnFile struct {
	Header  FileHeader
	Batches []ReturnBatch
}

// maxRecords bounds what a file may hold: the trailers count records in six digits.
const maxRecords = 999_999

type writer struct {
	bank    string
	lines   []string
	records int64
	err     error
}

func (w *writer) add(s string, err error) {
	if w.err == nil && err != nil {
		w.err = fmt.Errorf("record %d: %w", len(w.lines)+1, err)
	}
	w.lines = append(w.lines, s)
	w.records++
}

func (w *writer) bytes() ([]byte, error) {
	if w.err != nil {
		return nil, w.err
	}
	if w.records > maxRecords {
		return nil, fmt.Errorf("%w: %d records, more than %d", ErrInvalid, w.records, maxRecords)
	}
	return []byte(strings.Join(w.lines, "\r\n") + "\r\n"), nil
}

// trailerFor counts a batch's records, its header and trailer included, and totals its
// titles by portfolio.
func trailerFor(records int64, notice string, titles []struct {
	portfolio int64
	amount    int64
},
) BatchTrailer {
	t := BatchTrailer{Records: records, Notice: notice}
	for _, title := range titles {
		switch title.portfolio {
		case 2:
			t.LinkedCount, t.LinkedTotal = t.LinkedCount+1, t.LinkedTotal+title.amount
		case 3:
			t.SecuredCount, t.SecuredTotal = t.SecuredCount+1, t.SecuredTotal+title.amount
		case 4:
			t.DiscountCount, t.DiscountTotal = t.DiscountCount+1, t.DiscountTotal+title.amount
		default:
			t.SimpleCount, t.SimpleTotal = t.SimpleCount+1, t.SimpleTotal+title.amount
		}
	}
	return t
}

type titleTotal = struct {
	portfolio int64
	amount    int64
}

// Bytes writes a remittance, its batch numbers, sequences and trailers computed.
func (f RemittanceFile) Bytes() ([]byte, error) {
	bank := f.Header.Bank
	w := &writer{bank: bank}
	h := f.Header
	h.Code = CodeRemittance
	w.add(h.encode())
	for b, batch := range f.Batches {
		n := int64(b + 1)
		bh := batch.Header
		bh.Bank, bh.Batch, bh.Operation = bank, n, Remittance
		w.add(bh.encode())
		var seq int64
		var totals []titleTotal
		for _, t := range batch.Titles {
			seq++
			w.add(t.P.encode(bank, n, seq))
			seq++
			w.add(t.Q.encode(bank, n, seq))
			if t.Y03 != nil {
				seq++
				w.add(t.Y03.encode(bank, n, seq))
			}
			totals = append(totals, titleTotal{t.P.Portfolio, t.P.Amount})
		}
		w.add(trailerFor(seq+2, batch.Notice, totals).encode(bank, n))
	}
	w.add(FileTrailer{Batches: int64(len(f.Batches)), Records: w.records + 1}.encode(bank))
	return w.bytes()
}

// Bytes writes a return, its batch numbers, sequences and trailers computed.
func (f ReturnFile) Bytes() ([]byte, error) {
	bank := f.Header.Bank
	w := &writer{bank: bank}
	h := f.Header
	h.Code = CodeReturn
	w.add(h.encode())
	for b, batch := range f.Batches {
		n := int64(b + 1)
		bh := batch.Header
		bh.Bank, bh.Batch, bh.Operation = bank, n, Return
		w.add(bh.encode())
		var seq int64
		var totals []titleTotal
		for _, t := range batch.Titles {
			seq++
			w.add(t.T.encode(bank, n, seq))
			seq++
			w.add(t.U.encode(bank, n, seq))
			if t.Y03 != nil {
				seq++
				w.add(t.Y03.encode(bank, n, seq))
			}
			totals = append(totals, titleTotal{t.T.Portfolio, t.T.Amount})
		}
		w.add(trailerFor(seq+2, batch.Notice, totals).encode(bank, n))
	}
	w.add(FileTrailer{Batches: int64(len(f.Batches)), Records: w.records + 1}.encode(bank))
	return w.bytes()
}

// records splits a file into its records: 240 characters each, separated by CRLF or LF.
func records(data []byte) ([]string, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return nil, fmt.Errorf("%w: an empty file", ErrInvalid)
	}
	lines := strings.Split(text, "\n")
	if len(lines) > maxRecords {
		return nil, fmt.Errorf("%w: more than %d records", ErrInvalid, maxRecords)
	}
	for i, l := range lines {
		if len(l) != LineLength {
			return nil, fmt.Errorf("%w: record %d is %d characters, not %d", ErrInvalid, i+1, len(l), LineLength)
		}
	}
	return lines, nil
}

// cursor walks a file's records, checking the structure they share.
type cursor struct {
	lines []string
	i     int
	bank  string
}

func (c *cursor) peek() (string, byte) {
	if c.i >= len(c.lines) {
		return "", 0
	}
	return c.lines[c.i], c.lines[c.i][7]
}

func (c *cursor) next(kind byte) (string, error) {
	l, k := c.peek()
	if k != kind {
		return "", fmt.Errorf("%w: record %d is of type %q, want %q", ErrInvalid, c.i+1, k, kind)
	}
	if l[:3] != c.bank {
		return "", fmt.Errorf("%w: record %d is of bank %s, the file of %s", ErrInvalid, c.i+1, l[:3], c.bank)
	}
	c.i++
	return l, nil
}

func (c *cursor) wrap(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("record %d: %w", c.i, err)
}

// open reads the file header and checks the trailer agrees with the file.
func open(data []byte, code int) (*cursor, FileHeader, error) {
	lines, err := records(data)
	if err != nil {
		return nil, FileHeader{}, err
	}
	c := &cursor{lines: lines, bank: lines[0][:3]}
	l, err := c.next('0')
	if err != nil {
		return nil, FileHeader{}, err
	}
	h, err := decodeFileHeader(l)
	if err != nil {
		return nil, FileHeader{}, c.wrap(err)
	}
	if h.Code != code {
		return nil, FileHeader{}, fmt.Errorf("%w: file code %d, want %d", ErrInvalid, h.Code, code)
	}
	return c, h, nil
}

func (c *cursor) close(batches int) error {
	l, err := c.next('9')
	if err != nil {
		return err
	}
	t, err := decodeFileTrailer(l)
	if err != nil {
		return c.wrap(err)
	}
	if c.i != len(c.lines) {
		return fmt.Errorf("%w: records after the file trailer", ErrInvalid)
	}
	if t.Batches != int64(batches) || t.Records != int64(len(c.lines)) {
		return fmt.Errorf("%w: the trailer counts %d batches and %d records; the file has %d and %d", ErrInvalid, t.Batches, t.Records, batches, len(c.lines))
	}
	return nil
}

// batch reads a batch header, then each detail record with read, then the trailer, and
// checks the batch numbers, sequences and record count.
func (c *cursor) batch(n int64, operation byte, read func(l string, seq int64) error) (BatchHeader, string, error) {
	l, err := c.next('1')
	if err != nil {
		return BatchHeader{}, "", err
	}
	h, err := decodeBatchHeader(l)
	if err != nil {
		return BatchHeader{}, "", c.wrap(err)
	}
	if h.Batch != n || h.Operation != operation {
		return BatchHeader{}, "", fmt.Errorf("%w: batch %d of operation %c, want %d of %c", ErrInvalid, h.Batch, h.Operation, n, operation)
	}
	var seq int64
	for {
		l, kind := c.peek()
		if kind != '3' {
			break
		}
		c.i++
		seq++
		if l[:3] != c.bank || l[3:7] != fmt.Sprintf("%04d", n) || l[8:13] != fmt.Sprintf("%05d", seq) {
			return BatchHeader{}, "", fmt.Errorf("%w: record %d is not record %d of batch %d", ErrInvalid, c.i, seq, n)
		}
		if err := read(l, seq); err != nil {
			return BatchHeader{}, "", c.wrap(err)
		}
	}
	l, err = c.next('5')
	if err != nil {
		return BatchHeader{}, "", err
	}
	t, err := decodeBatchTrailer(l)
	if err != nil {
		return BatchHeader{}, "", c.wrap(err)
	}
	if l[3:7] != fmt.Sprintf("%04d", n) || t.Records != seq+2 {
		return BatchHeader{}, "", fmt.Errorf("%w: batch %d's trailer counts %d records; it has %d", ErrInvalid, n, t.Records, seq+2)
	}
	return h, t.Notice, nil
}

// ParseRemittance reads a remittance.
func ParseRemittance(data []byte) (RemittanceFile, error) {
	c, h, err := open(data, CodeRemittance)
	if err != nil {
		return RemittanceFile{}, err
	}
	f := RemittanceFile{Header: h}
	for n := int64(1); ; n++ {
		if _, kind := c.peek(); kind != '1' {
			break
		}
		var titles []RemittanceTitle
		needQ := false
		bh, notice, err := c.batch(n, Remittance, func(l string, _ int64) error {
			switch {
			case l[13] == 'P' && !needQ:
				p, err := decodeSegmentP(l)
				titles = append(titles, RemittanceTitle{P: p})
				needQ = true
				return err
			case l[13] == 'Q' && needQ:
				q, err := decodeSegmentQ(l)
				titles[len(titles)-1].Q = q
				needQ = false
				return err
			case l[13] == 'Y' && !needQ && len(titles) > 0 && titles[len(titles)-1].Y03 == nil:
				y, err := decodeSegmentY03(l)
				titles[len(titles)-1].Y03 = &y
				return err
			}
			return fmt.Errorf("%w: segment %q out of place in a remittance", ErrInvalid, l[13])
		})
		if err == nil && needQ {
			err = fmt.Errorf("%w: a segment P without its Q", ErrInvalid)
		}
		if err != nil {
			return RemittanceFile{}, err
		}
		f.Batches = append(f.Batches, RemittanceBatch{Header: bh, Titles: titles, Notice: notice})
	}
	return f, c.close(len(f.Batches))
}

// ParseReturn reads a return.
func ParseReturn(data []byte) (ReturnFile, error) {
	c, h, err := open(data, CodeReturn)
	if err != nil {
		return ReturnFile{}, err
	}
	f := ReturnFile{Header: h}
	for n := int64(1); ; n++ {
		if _, kind := c.peek(); kind != '1' {
			break
		}
		var titles []ReturnTitle
		pending := false
		bh, notice, err := c.batch(n, Return, func(l string, _ int64) error {
			switch {
			case l[13] == 'T' && !pending:
				t, err := decodeSegmentT(l)
				titles = append(titles, ReturnTitle{T: t})
				pending = true
				return err
			case l[13] == 'U' && pending:
				u, err := decodeSegmentU(l)
				titles[len(titles)-1].U = u
				pending = false
				return err
			case l[13] == 'Y' && !pending && len(titles) > 0 && titles[len(titles)-1].Y03 == nil:
				y, err := decodeSegmentY03(l)
				titles[len(titles)-1].Y03 = &y
				return err
			}
			return fmt.Errorf("%w: segment %q out of place in a return", ErrInvalid, l[13])
		})
		if err == nil && pending {
			err = fmt.Errorf("%w: a segment T without its U", ErrInvalid)
		}
		if err != nil {
			return ReturnFile{}, err
		}
		f.Batches = append(f.Batches, ReturnBatch{Header: bh, Titles: titles, Notice: notice})
	}
	return f, c.close(len(f.Batches))
}
