package threeds_test

import (
	"errors"
	"testing"
	"time"

	"github.com/iricardofernandes/jupiter/pkg/threeds"
)

func TestAnEmptySecretVerifiesNothing(t *testing.T) {
	now := time.Now()
	body := []byte(`{"messageType":"RReq"}`)
	if err := threeds.Verify(body, threeds.Sign(body, "", now), "", time.Minute, now); !errors.Is(err, threeds.ErrSignature) {
		t.Fatalf("an empty secret verified: %v", err)
	}
}
