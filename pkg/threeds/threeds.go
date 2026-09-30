// Package threeds holds the EMV 3-D Secure 2.2 messages Jupiter's 3DS server and the
// directory server and access control server simulator exchange, as JSON, and how the
// simulated directory server signs the results it sends back. It plays the part of the
// EMVCo specification, which both sides implement; docs/threeds.md says which fields are
// taken from public descriptions of it and which are this project's.
package threeds

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const Version = "2.2.0"

// Message types.
const (
	TypeAReq  = "AReq"
	TypeARes  = "ARes"
	TypeCReq  = "CReq"
	TypeCRes  = "CRes"
	TypeRReq  = "RReq"
	TypeRRes  = "RRes"
	TypeError = "Erro"
)

// Transaction statuses (transStatus).
const (
	StatusAuthenticated    = "Y"
	StatusNotAuthenticated = "N"
	StatusChallenge        = "C"
	StatusAttempted        = "A"
	StatusUnavailable      = "U"
	StatusRejected         = "R"
)

// AReq is the authentication request the 3DS server sends the directory server.
type AReq struct {
	MessageType                       string `json:"messageType"`
	MessageVersion                    string `json:"messageVersion"`
	MessageCategory                   string `json:"messageCategory"`
	DeviceChannel                     string `json:"deviceChannel"`
	ThreeDSServerTransID              string `json:"threeDSServerTransID"`
	ThreeDSServerURL                  string `json:"threeDSServerURL"`
	ThreeDSRequestorID                string `json:"threeDSRequestorID"`
	ThreeDSRequestorName              string `json:"threeDSRequestorName"`
	ThreeDSRequestorAuthenticationInd string `json:"threeDSRequestorAuthenticationInd"`
	AcquirerBIN                       string `json:"acquirerBIN"`
	AcquirerMerchantID                string `json:"acquirerMerchantID"`
	MerchantName                      string `json:"merchantName"`
	MCC                               string `json:"mcc"`
	MerchantCountryCode               string `json:"merchantCountryCode"`
	AcctNumber                        string `json:"acctNumber"`
	CardExpiryDate                    string `json:"cardExpiryDate"`
	PurchaseAmount                    string `json:"purchaseAmount"`
	PurchaseCurrency                  string `json:"purchaseCurrency"`
	PurchaseExponent                  string `json:"purchaseExponent"`
	PurchaseDate                      string `json:"purchaseDate"`
	TransType                         string `json:"transType"`
	NotificationURL                   string `json:"notificationURL"`
	BrowserIP                         string `json:"browserIP,omitempty"`
}

// String masks the card number: an AReq carries it in the clear.
func (a AReq) String() string {
	return fmt.Sprintf("AReq %s card ****%s amount %s", a.ThreeDSServerTransID, last4(a.AcctNumber), a.PurchaseAmount)
}

func (a AReq) GoString() string { return a.String() }

func (a AReq) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(a.String())) }

func last4(pan string) string {
	if len(pan) < 4 {
		return ""
	}
	return pan[len(pan)-4:]
}

// ARes is the directory server's answer, from the access control server.
type ARes struct {
	MessageType          string `json:"messageType"`
	MessageVersion       string `json:"messageVersion"`
	ThreeDSServerTransID string `json:"threeDSServerTransID"`
	DSTransID            string `json:"dsTransID"`
	ACSTransID           string `json:"acsTransID"`
	TransStatus          string `json:"transStatus"`
	TransStatusReason    string `json:"transStatusReason,omitempty"`
	ECI                  string `json:"eci,omitempty"`
	AuthenticationValue  string `json:"authenticationValue,omitempty"`
	ACSChallengeMandated string `json:"acsChallengeMandated,omitempty"`
	ACSURL               string `json:"acsURL,omitempty"`
}

// CReq starts a challenge; the cardholder's browser posts it to the ACS, base64url
// encoded, as the creq form field.
type CReq struct {
	MessageType          string `json:"messageType"`
	MessageVersion       string `json:"messageVersion"`
	ThreeDSServerTransID string `json:"threeDSServerTransID"`
	ACSTransID           string `json:"acsTransID"`
	ChallengeWindowSize  string `json:"challengeWindowSize"`
}

// CRes ends a challenge; the ACS has the browser post it to the notification URL.
type CRes struct {
	MessageType            string `json:"messageType"`
	MessageVersion         string `json:"messageVersion"`
	ThreeDSServerTransID   string `json:"threeDSServerTransID"`
	ACSTransID             string `json:"acsTransID"`
	TransStatus            string `json:"transStatus"`
	ChallengeCompletionInd string `json:"challengeCompletionInd"`
}

// RReq carries a challenge's result from the ACS, through the directory server, to
// the 3DS server.
type RReq struct {
	MessageType          string `json:"messageType"`
	MessageVersion       string `json:"messageVersion"`
	ThreeDSServerTransID string `json:"threeDSServerTransID"`
	DSTransID            string `json:"dsTransID"`
	ACSTransID           string `json:"acsTransID"`
	TransStatus          string `json:"transStatus"`
	ECI                  string `json:"eci,omitempty"`
	AuthenticationValue  string `json:"authenticationValue,omitempty"`
	InteractionCounter   string `json:"interactionCounter"`
}

type RRes struct {
	MessageType          string `json:"messageType"`
	MessageVersion       string `json:"messageVersion"`
	ThreeDSServerTransID string `json:"threeDSServerTransID"`
	DSTransID            string `json:"dsTransID"`
	ACSTransID           string `json:"acsTransID"`
	ResultsStatus        string `json:"resultsStatus"`
}

// Error is sent instead of a response when a message cannot be processed.
type Error struct {
	MessageType      string `json:"messageType"`
	MessageVersion   string `json:"messageVersion"`
	ErrorCode        string `json:"errorCode"`
	ErrorComponent   string `json:"errorComponent"`
	ErrorDescription string `json:"errorDescription"`
}

// EncodeForm encodes a CReq or CRes for a browser form: base64url JSON, no padding.
func EncodeForm(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func DecodeForm(s string, v any) error {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil {
		return fmt.Errorf("threeds: form value is not base64url: %w", err)
	}
	return json.Unmarshal(raw, v)
}

// SignatureHeader carries the directory server's signature on a results request: the
// time and an HMAC-SHA256, as t=unix,v1=hex. Real directory servers authenticate with
// mutual TLS; this signature stands in for it here.
const SignatureHeader = "Threeds-Signature"

var ErrSignature = errors.New("threeds: bad signature")

func Sign(body []byte, secret string, now time.Time) string {
	t := strconv.FormatInt(now.Unix(), 10)
	return "t=" + t + ",v1=" + mac(t, body, secret)
}

// Verify accepts a signature made with secret no more than tolerance ago.
func Verify(body []byte, header, secret string, tolerance time.Duration, now time.Time) error {
	var t, v1 string
	for part := range strings.SplitSeq(header, ",") {
		k, v, _ := strings.Cut(part, "=")
		switch k {
		case "t":
			t = v
		case "v1":
			v1 = v
		}
	}
	unix, err := strconv.ParseInt(t, 10, 64)
	if err != nil || v1 == "" {
		return ErrSignature
	}
	if age := now.Sub(time.Unix(unix, 0)); age > tolerance || age < -tolerance {
		return fmt.Errorf("%w: too old", ErrSignature)
	}
	if !hmac.Equal([]byte(v1), []byte(mac(t, body, secret))) {
		return ErrSignature
	}
	return nil
}

func mac(t string, body []byte, secret string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(t + "."))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// AuthenticationValue is the value the simulated issuers' ACS computes for an
// authentication, and their authorization systems check: base64 of 20 bytes of
// HMAC-SHA256 over the card, the amount and the directory server's transaction. Real
// CAVV and AAV algorithms are the schemes' own.
func AuthenticationValue(key []byte, pan string, amount int64, dsTransID string) string {
	m := hmac.New(sha256.New, key)
	fmt.Fprintf(m, "%s|%d|%s", pan, amount, dsTransID)
	return base64.StdEncoding.EncodeToString(m.Sum(nil)[:20])
}
