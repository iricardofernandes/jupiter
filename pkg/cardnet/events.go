package cardnet

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// EventSignatureHeader carries the network's signature on the events it sends token
// requestors: t=unix,v1=hex HMAC-SHA256 of "t.body" under a secret they share.
const EventSignatureHeader = "Cardnet-Signature"

var ErrEventSignature = errors.New("cardnet: bad event signature")

// TokenEvent is what the network tells a token requestor about one of its tokens: the
// card behind it was replaced (token.updated) or it was suspended (token.suspended).
// OccurredAt orders events about the same token, which may arrive out of order.
type TokenEvent struct {
	Type       string    `json:"type"`
	Reference  string    `json:"token_reference"`
	Status     string    `json:"status"`
	Last4      string    `json:"last4"`
	ExpMonth   int       `json:"exp_month"`
	ExpYear    int       `json:"exp_year"`
	OccurredAt time.Time `json:"occurred_at"`
}

// Valid reports whether e is an event a token requestor can apply.
func (e TokenEvent) Valid() bool {
	switch {
	case e.Type != "token.updated" && e.Type != "token.suspended",
		e.Status != "active" && e.Status != "suspended",
		e.Reference == "" || len(e.Reference) > 64,
		len(e.Last4) != 4 || strings.Trim(e.Last4, "0123456789") != "",
		e.ExpMonth < 1 || e.ExpMonth > 12 || e.ExpYear < 2000 || e.ExpYear > 2099,
		e.OccurredAt.IsZero():
		return false
	}
	return true
}

func SignEvent(body []byte, secret string, now time.Time) string {
	t := strconv.FormatInt(now.Unix(), 10)
	return "t=" + t + ",v1=" + eventMAC(t, body, secret)
}

func VerifyEvent(body []byte, header, secret string, tolerance time.Duration, now time.Time) error {
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
	if err != nil || v1 == "" || secret == "" {
		return ErrEventSignature
	}
	if age := now.Sub(time.Unix(unix, 0)); age > tolerance || age < -tolerance {
		return ErrEventSignature
	}
	if !hmac.Equal([]byte(v1), []byte(eventMAC(t, body, secret))) {
		return ErrEventSignature
	}
	return nil
}

func eventMAC(t string, body []byte, secret string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(t + "."))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}
