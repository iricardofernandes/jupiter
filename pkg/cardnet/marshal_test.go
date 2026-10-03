package cardnet_test

import (
	"encoding/json"
	"testing"

	"github.com/iricardofernandes/jupiter/pkg/cardnet"
)

// A message carries the card number and its private data the security code: neither
// reaches JSON, where a log or a stored payload would keep them.
func TestMessagesAreNeverMarshaled(t *testing.T) {
	m := cardnet.Message{MTI: "0100", PAN: "4242424242424242", Private: &cardnet.PrivateData{CVC: "737"}}
	for _, v := range []any{m, &m, *m.Private, map[string]any{"message": m}} {
		if raw, err := json.Marshal(v); err == nil {
			t.Fatalf("marshaled: %s", raw)
		}
	}
}
