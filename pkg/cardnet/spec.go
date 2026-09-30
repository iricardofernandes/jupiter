// Package cardnet is the wire protocol of the card network Jupiter's acquirer connector
// and the card network simulator speak: ISO 8583:1987 in ASCII, framed by a two-byte
// binary length, with the subset of data elements docs/cardnet/iso8583.md lists field by
// field. It plays the part of a network's published specification, which is why both
// sides share it; neither shares anything else.
package cardnet

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"github.com/moov-io/iso8583"
	"github.com/moov-io/iso8583/encoding"
	"github.com/moov-io/iso8583/field"
	"github.com/moov-io/iso8583/padding"
	"github.com/moov-io/iso8583/prefix"
	"github.com/moov-io/iso8583/sort"
)

func fixed(length int, description string) *field.String {
	return field.NewString(&field.Spec{
		Length: length, Description: description, Enc: encoding.ASCII, Pref: prefix.ASCII.Fixed,
	})
}

// numeric fields are digits of exact length: their leading zeros are part of the value.
func numeric(length int, description string) *field.String {
	return fixed(length, description)
}

func llvar(maxLength int, description string) *field.String {
	return field.NewString(&field.Spec{
		Length: maxLength, Description: description, Enc: encoding.ASCII, Pref: prefix.ASCII.LL,
	})
}

// Spec is the network's message specification.
var Spec = &iso8583.MessageSpec{
	Name: "Jupiter card network, ISO 8583:1987 ASCII",
	Fields: map[int]field.Field{
		0: fixed(4, "Message Type Indicator"),
		// A primary bitmap of 64 bits, followed by the secondary when bit 1 says so.
		1: field.NewBitmap(&field.Spec{
			Length: 8, Description: "Bitmap", Enc: encoding.BytesToASCIIHex, Pref: prefix.Hex.Fixed,
		}),
		2: llvar(19, "Primary Account Number"),
		3: numeric(6, "Processing Code"),
		4: field.NewNumeric(&field.Spec{
			Length: 12, Description: "Amount, Transaction", Enc: encoding.ASCII, Pref: prefix.ASCII.Fixed, Pad: padding.Left('0'),
		}),
		7:  numeric(10, "Transmission Date and Time (MMDDhhmmss, UTC)"),
		11: numeric(6, "Systems Trace Audit Number"),
		12: numeric(6, "Time, Local Transaction (hhmmss)"),
		13: numeric(4, "Date, Local Transaction (MMDD)"),
		14: numeric(4, "Date, Expiration (YYMM)"),
		18: numeric(4, "Merchant Type (MCC)"),
		22: numeric(3, "Point of Service Entry Mode"),
		32: llvar(11, "Acquiring Institution Identification Code"),
		37: fixed(12, "Retrieval Reference Number"),
		38: fixed(6, "Authorization Identification Response"),
		39: fixed(2, "Response Code"),
		41: fixed(8, "Card Acceptor Terminal Identification"),
		42: fixed(15, "Card Acceptor Identification Code"),
		48: field.NewComposite(&field.Spec{
			Length: 999, Description: "Additional Data, Private", Pref: prefix.ASCII.LLL,
			Tag: &field.TagSpec{Length: 2, Enc: encoding.ASCII, Sort: sort.Strings},
			Subfields: map[string]field.Field{
				TagAuthenticationValue:  llvar(40, "3-D Secure authentication value (CAVV, AAV)"),
				TagCVC:                  llvar(4, "Card verification code"),
				TagDSTransID:            llvar(36, "3-D Secure directory server transaction id"),
				TagECI:                  llvar(2, "Electronic commerce indicator"),
				TagInstallmentFinancing: llvar(1, "Installment financing: M merchant, I issuer"),
				TagNetworkTransactionID: llvar(15, "Network transaction identifier"),
				TagPartialApproval:      llvar(1, "Partial approval capable: 1"),
				TagStoredCredential:     llvar(1, "Stored credential: I initial, C customer-initiated, M merchant-initiated"),
				TagStandIn:              llvar(1, "Approved in stand-in by the network: 1"),
				TagTokenCryptogram:      llvar(40, "Network token cryptogram, for DE 2 holding a token"),
			},
		}),
		49: numeric(3, "Currency Code, Transaction (ISO 4217)"),
		67: numeric(2, "Extended Payment Code (installments)"),
		70: numeric(3, "Network Management Information Code"),
		90: numeric(42, "Original Data Elements"),
	},
}

// The tags of the TLV subfields of DE 48.
const (
	TagAuthenticationValue  = "AV"
	TagCVC                  = "CV"
	TagDSTransID            = "DS"
	TagECI                  = "EC"
	TagInstallmentFinancing = "IF"
	TagNetworkTransactionID = "NT"
	TagPartialApproval      = "PA"
	TagStoredCredential     = "SC"
	TagStandIn              = "SI"
	TagTokenCryptogram      = "TC"
)

// ReadLength and WriteLength frame each message with its length as two bytes, big-endian.
func ReadLength(r io.Reader) (int, error) {
	var header [2]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, err
	}
	return int(binary.BigEndian.Uint16(header[:])), nil
}

func WriteLength(w io.Writer, length int) (int, error) {
	if length > math.MaxUint16 {
		return 0, fmt.Errorf("cardnet: a message of %d bytes does not fit the two-byte header", length)
	}
	var header [2]byte
	binary.BigEndian.PutUint16(header[:], uint16(length)) //nolint:gosec // bounded above
	return w.Write(header[:])
}
