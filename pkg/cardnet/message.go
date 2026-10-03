package cardnet

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/moov-io/iso8583"
)

// Message types. The network answers every request (xx00) and advice (xx20, xx21) with
// the matching response (xx10, xx30).
const (
	AuthorizationRequest   = "0100"
	AuthorizationResponse  = "0110"
	FinancialRequest       = "0200"
	FinancialResponse      = "0210"
	CompletionAdvice       = "0220"
	CompletionAdviceRepeat = "0221"
	CompletionResponse     = "0230"
	ReversalRequest        = "0400"
	ReversalResponse       = "0410"
	ReversalAdvice         = "0420"
	ReversalAdviceRepeat   = "0421"
	ReversalAdviceResponse = "0430"
	NetworkRequest         = "0800"
	NetworkResponse        = "0810"
)

// ResponseTo is the message type that answers mti.
func ResponseTo(mti string) string {
	switch mti {
	case CompletionAdviceRepeat:
		return CompletionResponse
	case ReversalAdviceRepeat:
		return ReversalAdviceResponse
	}
	if len(mti) != 4 {
		return ""
	}
	return mti[:2] + string(mti[2]+1) + "0"
}

// Processing codes (DE 3).
const (
	ProcessingPurchase = "000000"
	ProcessingRefund   = "200000"
)

// Point of service entry modes (DE 22).
const (
	EntryECommerce        = "810"
	EntryCredentialOnFile = "102"
)

// Network management codes (DE 70).
const (
	SignOn  = "001"
	SignOff = "002"
	Echo    = "301"
)

// Stored credential indicators (DE 48, tag SC).
const (
	StoredCredentialInitial  = "I"
	StoredCredentialCustomer = "C"
	StoredCredentialMerchant = "M"
)

// Response codes (DE 39).
const (
	Approved                 = "00"
	DoNotHonour              = "05"
	PartiallyApproved        = "10"
	InvalidTransaction       = "12"
	InvalidAmount            = "13"
	InvalidCardNumber        = "14"
	FormatError              = "30"
	InsufficientFunds        = "51"
	ExpiredCard              = "54"
	NotPermittedToCardholder = "57"
	IssuerUnavailable        = "91"
	SystemMalfunction        = "96"
)

// CurrencyBRL is the ISO 4217 numeric code of the real.
const CurrencyBRL = "986"

// Message holds the data elements the network uses. Unset elements are left out of the
// packed message. It prints with the card number and security code masked.
type Message struct {
	MTI                  string       `iso8583:"0"`
	PAN                  string       `iso8583:"2"`
	ProcessingCode       string       `iso8583:"3"`
	Amount               int64        `iso8583:"4"`
	TransmissionDateTime string       `iso8583:"7"`
	STAN                 string       `iso8583:"11"`
	LocalTime            string       `iso8583:"12"`
	LocalDate            string       `iso8583:"13"`
	Expiry               string       `iso8583:"14"`
	MCC                  string       `iso8583:"18"`
	EntryMode            string       `iso8583:"22"`
	AcquirerID           string       `iso8583:"32"`
	RRN                  string       `iso8583:"37"`
	AuthorizationCode    string       `iso8583:"38"`
	ResponseCode         string       `iso8583:"39"`
	TerminalID           string       `iso8583:"41"`
	MerchantID           string       `iso8583:"42"`
	Private              *PrivateData `iso8583:"48"`
	Currency             string       `iso8583:"49"`
	Installments         string       `iso8583:"67"`
	NetworkCode          string       `iso8583:"70"`
	OriginalData         string       `iso8583:"90"`
}

type PrivateData struct {
	AuthenticationValue  string `iso8583:"AV"`
	CVC                  string `iso8583:"CV"`
	DSTransID            string `iso8583:"DS"`
	ECI                  string `iso8583:"EC"`
	InstallmentFinancing string `iso8583:"IF"`
	NetworkTransactionID string `iso8583:"NT"`
	PartialApproval      string `iso8583:"PA"`
	StoredCredential     string `iso8583:"SC"`
	StandIn              string `iso8583:"SI"`
	TokenCryptogram      string `iso8583:"TC"`
}

func (p PrivateData) String() string {
	cvc := ""
	if p.CVC != "" {
		cvc = "***"
	}
	return fmt.Sprintf("AV=%s CV=%s DS=%s EC=%s IF=%s NT=%s PA=%s SC=%s SI=%s TC=%s", p.AuthenticationValue, cvc, p.DSTransID, p.ECI,
		p.InstallmentFinancing, p.NetworkTransactionID, p.PartialApproval, p.StoredCredential, p.StandIn, p.TokenCryptogram)
}

func (p PrivateData) GoString() string { return p.String() }

func (p PrivateData) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(p.String())) }

func (p PrivateData) LogValue() slog.Value { return slog.StringValue(p.String()) }

func (p PrivateData) MarshalJSON() ([]byte, error) {
	return nil, errors.New("cardnet: PrivateData is never marshaled: it carries the security code")
}

func (m Message) String() string {
	return fmt.Sprintf("%s stan=%s rrn=%s pan=%s amount=%d rc=%s", m.MTI, m.STAN, m.RRN, MaskPAN(m.PAN), m.Amount, m.ResponseCode)
}

func (m Message) GoString() string { return m.String() }

func (m Message) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(m.String())) }

func (m Message) LogValue() slog.Value { return slog.StringValue(m.String()) }

func (m Message) MarshalJSON() ([]byte, error) {
	return nil, errors.New("cardnet: Message is never marshaled: it carries the card number")
}

// MaskPAN keeps the last four digits.
func MaskPAN(pan string) string {
	if pan == "" {
		return ""
	}
	if len(pan) <= 4 {
		return "****"
	}
	return "****" + pan[len(pan)-4:]
}

// NetworkTransactionID is DE 48's NT, or "" without one.
func (m Message) NetworkTransactionID() string {
	if m.Private == nil {
		return ""
	}
	return m.Private.NetworkTransactionID
}

func Pack(m Message) (*iso8583.Message, error) {
	msg := iso8583.NewMessage(Spec)
	if err := msg.Marshal(&m); err != nil {
		return nil, fmt.Errorf("cardnet: building %s: %w", m.MTI, err)
	}
	return msg, nil
}

func Unpack(msg *iso8583.Message) (Message, error) {
	var m Message
	if err := msg.Unmarshal(&m); err != nil {
		return Message{}, fmt.Errorf("cardnet: reading a message: %w", err)
	}
	if m.Private != nil && *m.Private == (PrivateData{}) {
		m.Private = nil
	}
	return m, nil
}

// TransmissionTime formats DE 7, in UTC.
func TransmissionTime(t time.Time) string {
	return t.UTC().Format("0102150405")
}

// OriginalData is DE 90: the message a reversal or completion refers to.
type OriginalData struct {
	MTI                  string
	STAN                 string
	TransmissionDateTime string
	// AcquirerID is padded to eleven digits.
	AcquirerID string
}

var errOriginalData = errors.New("cardnet: DE 90 must be 42 digits")

// Original builds DE 90 for the message m, as sent.
func Original(m Message) OriginalData {
	return OriginalData{MTI: m.MTI, STAN: m.STAN, TransmissionDateTime: m.TransmissionDateTime, AcquirerID: PadAcquirer(m.AcquirerID)}
}

// PadAcquirer pads an acquiring institution code to the eleven digits DE 90 carries.
func PadAcquirer(id string) string {
	if len(id) >= 11 {
		return id
	}
	return strings.Repeat("0", 11-len(id)) + id
}

// Encode lays DE 90 out as ISO 8583:1987 does: the original message type (4), STAN (6),
// transmission date and time (10), acquiring institution (11) and forwarding
// institution (11, zeros here).
func (o OriginalData) Encode() string {
	return o.MTI + o.STAN + o.TransmissionDateTime + PadAcquirer(o.AcquirerID) + strings.Repeat("0", 11)
}

func ParseOriginalData(s string) (OriginalData, error) {
	if len(s) != 42 || strings.Trim(s, "0123456789") != "" {
		return OriginalData{}, errOriginalData
	}
	return OriginalData{MTI: s[:4], STAN: s[4:10], TransmissionDateTime: s[10:20], AcquirerID: s[20:31]}, nil
}
