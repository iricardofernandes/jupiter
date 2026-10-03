// Package webhook signs and verifies Jupiter webhook deliveries. Receivers import it to
// check the Jupiter-Signature header of each request.
//
// The header is "t=<unix seconds>,v1=<hex HMAC-SHA256 of "<t>.<body>">". During a secret
// rotation it carries one v1 signature per active secret.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

const (
	SignatureHeader  = "Jupiter-Signature"
	DefaultTolerance = 5 * time.Minute
)

var (
	ErrInvalidHeader             = errors.New("webhook: malformed signature header")
	ErrNoSignature               = errors.New("webhook: no v1 signature in header")
	ErrSignatureMismatch         = errors.New("webhook: no signature matches the payload")
	ErrTimestampOutsideTolerance = errors.New("webhook: timestamp outside the tolerance")
)

func Sign(payload []byte, at time.Time, secrets ...string) string {
	timestamp := strconv.FormatInt(at.Unix(), 10)
	var b strings.Builder
	b.WriteString("t=")
	b.WriteString(timestamp)
	for _, secret := range secrets {
		b.WriteString(",v1=")
		b.WriteString(hex.EncodeToString(signature(payload, timestamp, secret)))
	}
	return b.String()
}

// Verify checks that header carries a v1 signature of payload under secret, made within
// tolerance of now in either direction. A receiver should also discard an event whose
// id it has already processed, because delivery is at least once.
func Verify(payload []byte, header, secret string, tolerance time.Duration, now time.Time) error {
	if secret == "" {
		// A signature under no secret is one anybody can make.
		return ErrSignatureMismatch
	}
	timestamp, unix, signatures, err := parse(header)
	if err != nil {
		return err
	}
	if age := now.Sub(time.Unix(unix, 0)); age > tolerance || age < -tolerance {
		return ErrTimestampOutsideTolerance
	}
	expected := signature(payload, timestamp, secret)
	for _, s := range signatures {
		if hmac.Equal(s, expected) {
			return nil
		}
	}
	return ErrSignatureMismatch
}

func signature(payload []byte, timestamp, secret string) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(payload)
	return mac.Sum(nil)
}

func parse(header string) (timestamp string, unix int64, signatures [][]byte, err error) {
	for part := range strings.SplitSeq(header, ",") {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			return "", 0, nil, ErrInvalidHeader
		}
		switch key {
		case "t":
			timestamp = value
		case "v1":
			// A signature that is not hex can never match; skipping it lets a valid one
			// beside it through.
			if s, decodeErr := hex.DecodeString(value); decodeErr == nil {
				signatures = append(signatures, s)
			}
		}
	}
	unix, err = strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return "", 0, nil, ErrInvalidHeader
	}
	if len(signatures) == 0 {
		return "", 0, nil, ErrNoSignature
	}
	return timestamp, unix, signatures, nil
}
