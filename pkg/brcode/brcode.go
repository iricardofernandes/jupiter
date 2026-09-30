// Package brcode encodes and parses Pix BR Codes: the EMV Merchant Presented Mode QR
// code payload the Banco Central's Manual de Padrões para Iniciação do Pix defines, also
// pasted as text ("Pix Copia e Cola"). A payload is a sequence of fields, each a two-digit
// ID, a two-digit length and a value, some of which are templates holding fields of
// their own, and ends with a CRC16 of everything before it.
package brcode

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

var ErrInvalid = errors.New("brcode: invalid")

// Field is one field of a payload. A template's value is its Fields, encoded.
type Field struct {
	ID     string
	Value  string
	Fields []Field
}

// Kind says how a payer finds what to pay.
type Kind string

const (
	// Static: the receiver's key, and the amount if the code carries one.
	Static Kind = "static"
	// Dynamic: a URL whose signed payload says what to pay.
	Dynamic Kind = "dynamic"
	// Composite: the parameters of a Pix Automático recurrence, alone or beside a static
	// or dynamic payment.
	Composite Kind = "composite"
)

// Point of initiation (ID 01).
const (
	Reusable  = "11"
	SingleUse = "12"
)

// NoTxID is the transaction id of a code that has none (ID 62-05).
const NoTxID = "***"

// GUI identifies the Pix arrangement in a merchant account template.
const GUI = "br.gov.bcb.pix"

// Pix is what a BR Code says, field by field. Empty strings are absent fields.
type Pix struct {
	PointOfInitiation string // 01
	Key               string // 26-01: the receiver's DICT key, in a static code
	AdditionalInfo    string // 26-02
	FSS               string // 26-03: the ISPB of a Pix Saque service provider
	URL               string // 26-25: the payload location of a dynamic code, without a scheme
	MCC               string // 52
	Currency          string // 53
	Amount            string // 54: in reais, with a dot and two decimals
	Country           string // 58
	MerchantName      string // 59
	MerchantCity      string // 60
	PostalCode        string // 61
	TxID              string // 62-05
	RecurrenceURL     string // 80-25: the recurrence parameters' location
}

func (p Pix) Kind() Kind {
	switch {
	case p.RecurrenceURL != "":
		return Composite
	case p.URL != "":
		return Dynamic
	default:
		return Static
	}
}

// Field IDs.
const (
	idFormat         = "00"
	idInitiation     = "01"
	idPixAccount     = "26"
	idMCC            = "52"
	idCurrency       = "53"
	idAmount         = "54"
	idCountry        = "58"
	idName           = "59"
	idCity           = "60"
	idPostalCode     = "61"
	idAdditionalData = "62"
	idRecurrence     = "80"
	idCRC            = "63"

	subGUI            = "00"
	subKey            = "01"
	subAdditionalInfo = "02"
	subFSS            = "03"
	subURL            = "25"
	subTxID           = "05"
)

// Limits, in characters.
const (
	maxValue          = 99
	maxKey            = 77
	maxAdditionalInfo = 72
	maxURL            = 77
	maxName           = 25
	maxCity           = 15
	maxPostalCode     = 8
	maxTxID           = 25
	maxAmount         = 13
)

var (
	txidPattern   = regexp.MustCompile(`^[A-Za-z0-9]{1,25}$`)
	amountPattern = regexp.MustCompile(`^\d{1,10}\.\d{2}$`)
	ispbPattern   = regexp.MustCompile(`^[0-9A-Z]{8}$`)
)

// Parse reads a BR Code and checks it is a well-formed Pix code. Fields of other payment
// arrangements are left out of what it returns.
func Parse(code string) (Pix, error) {
	fields, err := Decode(code)
	if err != nil {
		return Pix{}, err
	}
	var p Pix
	pixAccounts := 0
	for _, f := range fields {
		switch {
		case f.ID == idInitiation:
			p.PointOfInitiation = f.Value
		case isAccountTemplate(f.ID) && strings.EqualFold(sub(f, subGUI), GUI):
			pixAccounts++
			p.Key, p.AdditionalInfo, p.FSS, p.URL = sub(f, subKey), sub(f, subAdditionalInfo), sub(f, subFSS), sub(f, subURL)
		case f.ID == idMCC:
			p.MCC = f.Value
		case f.ID == idCurrency:
			p.Currency = f.Value
		case f.ID == idAmount:
			p.Amount = f.Value
		case f.ID == idCountry:
			p.Country = f.Value
		case f.ID == idName:
			p.MerchantName = f.Value
		case f.ID == idCity:
			p.MerchantCity = f.Value
		case f.ID == idPostalCode:
			p.PostalCode = f.Value
		case f.ID == idAdditionalData:
			p.TxID = sub(f, subTxID)
		case isUnreservedTemplate(f.ID) && strings.EqualFold(sub(f, subGUI), GUI):
			p.RecurrenceURL = sub(f, subURL)
		}
	}
	if pixAccounts != 1 {
		return Pix{}, fmt.Errorf("%w: %d Pix merchant account templates, want one", ErrInvalid, pixAccounts)
	}
	if err := p.validate(); err != nil {
		return Pix{}, err
	}
	return p, nil
}

func (p Pix) validate() error {
	var problems []string
	check := func(ok bool, format string, args ...any) {
		if !ok {
			problems = append(problems, fmt.Sprintf(format, args...))
		}
	}
	check(p.PointOfInitiation == "" || p.PointOfInitiation == Reusable || p.PointOfInitiation == SingleUse, "point of initiation %q", p.PointOfInitiation)
	check(p.Key != "" || p.URL != "" || p.RecurrenceURL != "", "neither a key nor a URL")
	check(p.Key == "" || p.URL == "", "both a key and a URL")
	check(runes(p.Key) <= maxKey, "a key longer than %d", maxKey)
	check(runes(p.AdditionalInfo) <= maxAdditionalInfo, "additional information longer than %d", maxAdditionalInfo)
	check(p.FSS == "" || ispbPattern.MatchString(p.FSS), "fss %q is not an ISPB", p.FSS)
	check(validURL(p.URL), "payload URL %q", p.URL)
	check(validURL(p.RecurrenceURL), "recurrence URL %q", p.RecurrenceURL)
	check(p.URL == "" || p.RecurrenceURL == "" || host(p.URL) == host(p.RecurrenceURL), "payment and recurrence URLs on different hosts")
	check(p.MCC != "" && len(p.MCC) == 4 && digits(p.MCC), "merchant category code %q", p.MCC)
	check(p.Currency == "986", "currency %q, want 986", p.Currency)
	check(p.Amount == "" || (len(p.Amount) <= maxAmount && amountPattern.MatchString(p.Amount)), "amount %q", p.Amount)
	check(p.Country == "BR", "country %q, want BR", p.Country)
	check(p.MerchantName != "" && runes(p.MerchantName) <= maxName, "merchant name of %d characters", runes(p.MerchantName))
	check(p.MerchantCity != "" && runes(p.MerchantCity) <= maxCity, "merchant city of %d characters", runes(p.MerchantCity))
	check(runes(p.PostalCode) <= maxPostalCode, "postal code %q", p.PostalCode)
	check(p.TxID == NoTxID || txidPattern.MatchString(p.TxID), "txid %q", p.TxID)
	if problems != nil {
		return fmt.Errorf("%w: %s", ErrInvalid, strings.Join(problems, "; "))
	}
	return nil
}

// Encode writes the code, fields in ascending order, and its CRC. The Pix defaults fill
// what is left empty: merchant category 0000, currency 986, country BR, and no txid.
func (p Pix) Encode() (string, error) {
	if p.MCC == "" {
		p.MCC = "0000"
	}
	if p.Currency == "" {
		p.Currency = "986"
	}
	if p.Country == "" {
		p.Country = "BR"
	}
	if p.TxID == "" {
		p.TxID = NoTxID
	}
	if err := p.validate(); err != nil {
		return "", err
	}
	fields := []Field{{ID: idFormat, Value: "01"}}
	fields = appendIf(fields, idInitiation, p.PointOfInitiation)
	account := []Field{{ID: subGUI, Value: GUI}}
	account = appendIf(account, subKey, p.Key)
	account = appendIf(account, subAdditionalInfo, p.AdditionalInfo)
	account = appendIf(account, subFSS, p.FSS)
	account = appendIf(account, subURL, p.URL)
	fields = append(fields, Field{ID: idPixAccount, Fields: account},
		Field{ID: idMCC, Value: p.MCC}, Field{ID: idCurrency, Value: p.Currency})
	fields = appendIf(fields, idAmount, p.Amount)
	fields = append(fields, Field{ID: idCountry, Value: p.Country}, Field{ID: idName, Value: p.MerchantName},
		Field{ID: idCity, Value: p.MerchantCity})
	fields = appendIf(fields, idPostalCode, p.PostalCode)
	fields = append(fields, Field{ID: idAdditionalData, Fields: []Field{{ID: subTxID, Value: p.TxID}}})
	if p.RecurrenceURL != "" {
		fields = append(fields, Field{ID: idRecurrence, Fields: []Field{{ID: subGUI, Value: GUI}, {ID: subURL, Value: p.RecurrenceURL}}})
	}
	for _, f := range fields {
		if runes(encodeValue(f)) > maxValue {
			return "", fmt.Errorf("%w: field %s is longer than %d characters", ErrInvalid, f.ID, maxValue)
		}
	}
	return EncodeFields(fields), nil
}

// FormatAmount writes an amount in centavos as field 54 carries it.
func FormatAmount(minor int64) string {
	return fmt.Sprintf("%d.%02d", minor/100, minor%100)
}

// Decode splits a payload into its fields, templates included, after checking its CRC.
// It keeps every field and their order, so EncodeFields gives back the same payload.
func Decode(code string) ([]Field, error) {
	const trailer = len(idCRC) + 2 + 4
	if !utf8.ValidString(code) || len(code) < trailer || code[len(code)-trailer:len(code)-4] != idCRC+"04" {
		return nil, fmt.Errorf("%w: no CRC at the end", ErrInvalid)
	}
	body, crc := code[:len(code)-4], code[len(code)-4:]
	if !strings.EqualFold(crc, CRC16(body)) {
		return nil, fmt.Errorf("%w: CRC %s does not match the payload", ErrInvalid, crc)
	}
	fields, err := decodeFields(body[:len(body)-trailer+4], true)
	if err != nil {
		return nil, err
	}
	if len(fields) == 0 || fields[0].ID != idFormat || fields[0].Value != "01" {
		return nil, fmt.Errorf("%w: it does not start with the payload format indicator 01", ErrInvalid)
	}
	return fields, nil
}

func decodeFields(s string, top bool) ([]Field, error) {
	var fields []Field
	rest := []rune(s)
	for len(rest) > 0 {
		if len(rest) < 4 || !digits(string(rest[:4])) {
			return nil, fmt.Errorf("%w: a field without an ID and a length", ErrInvalid)
		}
		id := string(rest[:2])
		n, _ := strconv.Atoi(string(rest[2:4]))
		if n == 0 || len(rest) < 4+n {
			return nil, fmt.Errorf("%w: field %s of length %d does not fit", ErrInvalid, id, n)
		}
		f := Field{ID: id, Value: string(rest[4 : 4+n])}
		if top && isTemplate(id) {
			sub, err := decodeFields(f.Value, false)
			if err != nil {
				return nil, fmt.Errorf("template %s: %w", id, err)
			}
			f.Value, f.Fields = "", sub
		}
		fields = append(fields, f)
		rest = rest[4+n:]
	}
	return fields, nil
}

// EncodeFields writes fields and appends the CRC.
func EncodeFields(fields []Field) string {
	var b strings.Builder
	for _, f := range fields {
		if f.ID == idCRC {
			continue
		}
		v := encodeValue(f)
		fmt.Fprintf(&b, "%s%02d%s", f.ID, runes(v), v)
	}
	b.WriteString(idCRC + "04")
	return b.String() + CRC16(b.String())
}

func encodeValue(f Field) string {
	if f.Fields == nil {
		return f.Value
	}
	var b strings.Builder
	for _, s := range f.Fields {
		fmt.Fprintf(&b, "%s%02d%s", s.ID, runes(s.Value), s.Value)
	}
	return b.String()
}

// CRC16 is the checksum the payload ends with: CRC-16/CCITT-FALSE, polynomial 0x1021,
// initial value 0xFFFF, over the payload's bytes up to and including "6304", as four
// upper-case hex digits.
func CRC16(s string) string {
	crc := uint16(0xFFFF)
	for i := range len(s) {
		crc ^= uint16(s[i]) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return fmt.Sprintf("%04X", crc)
}

func isAccountTemplate(id string) bool {
	n, _ := strconv.Atoi(id)
	return n >= 26 && n <= 51
}

func isUnreservedTemplate(id string) bool {
	n, _ := strconv.Atoi(id)
	return n >= 80 && n <= 99
}

// isTemplate: merchant account information (26-51), additional data (62), the language
// template (64) and unreserved templates (80-99) hold fields of their own.
func isTemplate(id string) bool {
	return isAccountTemplate(id) || id == idAdditionalData || id == "64" || isUnreservedTemplate(id)
}

func sub(f Field, id string) string {
	for _, s := range f.Fields {
		if s.ID == id {
			return s.Value
		}
	}
	return ""
}

func appendIf(fields []Field, id, value string) []Field {
	if value == "" {
		return fields
	}
	return append(fields, Field{ID: id, Value: value})
}

// validURL: a payload location is written without its scheme, and is always fetched
// over HTTPS.
func validURL(u string) bool {
	if u == "" {
		return true
	}
	return runes(u) <= maxURL && !strings.Contains(u, "://") && strings.Contains(u, "/") && host(u) != "" &&
		!strings.ContainsAny(u, " \t\r\n")
}

func host(u string) string {
	h, _, _ := strings.Cut(u, "/")
	return strings.ToLower(h)
}

func digits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func runes(s string) int { return utf8.RuneCountInString(s) }
