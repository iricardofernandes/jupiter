package webhook_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/iricardofernandes/jupiter/pkg/webhook"
)

var (
	payload = []byte(`{"id":"evt_1","type":"webhook_endpoint.created"}`)
	at      = time.Unix(1_790_000_000, 0)
)

func TestSignedPayloadVerifies(t *testing.T) {
	header := webhook.Sign(payload, at, "whsec_one")
	if !strings.HasPrefix(header, "t=1790000000,v1=") {
		t.Fatalf("header = %q", header)
	}
	if err := webhook.Verify(payload, header, "whsec_one", webhook.DefaultTolerance, at.Add(time.Minute)); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestSignatureMatchesTheDocumentedScheme(t *testing.T) {
	// HMAC-SHA256("whsec_test", "1790000000." + payload), computed with openssl dgst.
	const want = "t=1790000000,v1=cbb1b8bb1bc26bfe6098c5e3fc9b5df83195f426b35d1f5e66d3073b6bcf0b72"
	got := webhook.Sign(payload, at, "whsec_test")
	if got != want {
		t.Fatalf("Sign = %q\nwant  %q", got, want)
	}
}

// While a secret is being rotated, events carry a signature for each secret, and a
// receiver holding either one accepts them.
func TestEverySecretDuringRotationVerifies(t *testing.T) {
	header := webhook.Sign(payload, at, "whsec_old", "whsec_new")
	if strings.Count(header, "v1=") != 2 {
		t.Fatalf("header = %q, want two signatures", header)
	}
	for _, secret := range []string{"whsec_old", "whsec_new"} {
		if err := webhook.Verify(payload, header, secret, webhook.DefaultTolerance, at); err != nil {
			t.Errorf("Verify with %s: %v", secret, err)
		}
	}
}

func TestVerifyRejects(t *testing.T) {
	valid := webhook.Sign(payload, at, "whsec_one")
	tests := []struct {
		name    string
		payload []byte
		header  string
		secret  string
		now     time.Time
		want    error
	}{
		{"wrong secret", payload, valid, "whsec_two", at, webhook.ErrSignatureMismatch},
		{"tampered payload", []byte(`{"id":"evt_2"}`), valid, "whsec_one", at, webhook.ErrSignatureMismatch},
		{"replayed after the tolerance", payload, valid, "whsec_one", at.Add(5*time.Minute + time.Second), webhook.ErrTimestampOutsideTolerance},
		{"from too far in the future", payload, valid, "whsec_one", at.Add(-5*time.Minute - time.Second), webhook.ErrTimestampOutsideTolerance},
		{"no header", payload, "", "whsec_one", at, webhook.ErrInvalidHeader},
		{"no timestamp", payload, "v1=abc", "whsec_one", at, webhook.ErrInvalidHeader},
		{"no signature", payload, "t=1790000000", "whsec_one", at, webhook.ErrNoSignature},
		{"garbage timestamp", payload, "t=soon,v1=abc", "whsec_one", at, webhook.ErrInvalidHeader},
		{"unknown scheme only", payload, "t=1790000000,v0=abc", "whsec_one", at, webhook.ErrNoSignature},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := webhook.Verify(tt.payload, tt.header, tt.secret, webhook.DefaultTolerance, tt.now); !errors.Is(err, tt.want) {
				t.Fatalf("Verify = %v, want %v", err, tt.want)
			}
		})
	}
}

// A tampered timestamp breaks the signature even when it is inside the tolerance.
func TestTheTimestampIsSigned(t *testing.T) {
	header := webhook.Sign(payload, at, "whsec_one")
	moved := strings.Replace(header, "t=1790000000", "t=1790000010", 1)
	if err := webhook.Verify(payload, moved, "whsec_one", webhook.DefaultTolerance, at); !errors.Is(err, webhook.ErrSignatureMismatch) {
		t.Fatalf("Verify = %v, want ErrSignatureMismatch", err)
	}
}
