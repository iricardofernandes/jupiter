package webhook_test

import (
	"errors"
	"testing"
	"time"

	"github.com/iricardofernandes/jupiter/pkg/webhook"
)

// A signature under no secret is one anybody can make: it verifies nothing.
func TestAnEmptySecretVerifiesNothing(t *testing.T) {
	now := time.Now()
	body := []byte(`{"id":"evt_1"}`)
	if err := webhook.Verify(body, webhook.Sign(body, now, ""), "", webhook.DefaultTolerance, now); !errors.Is(err, webhook.ErrSignatureMismatch) {
		t.Fatalf("an empty secret verified: %v", err)
	}
}
