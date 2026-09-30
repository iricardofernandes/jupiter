package secretbox_test

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/iricardofernandes/jupiter/internal/platform/secretbox"
)

func newBox(t *testing.T) *secretbox.Box {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	box, err := secretbox.FromBase64(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	return box
}

func TestSealAndOpen(t *testing.T) {
	box := newBox(t)
	sealed, err := box.Seal([]byte("whsec_abc"), []byte("we_1"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("whsec_abc")) {
		t.Fatal("sealed value contains the plaintext")
	}
	opened, err := box.Open(sealed, []byte("we_1"))
	if err != nil || string(opened) != "whsec_abc" {
		t.Fatalf("Open = %q, %v", opened, err)
	}
}

func TestSealingTwiceGivesDifferentCiphertexts(t *testing.T) {
	box := newBox(t)
	a, _ := box.Seal([]byte("same"), nil)
	b, _ := box.Seal([]byte("same"), nil)
	if bytes.Equal(a, b) {
		t.Fatal("two seals of the same plaintext are equal: nonces repeat")
	}
}

func TestOpenRejects(t *testing.T) {
	box := newBox(t)
	other := newBox(t)
	sealed, err := box.Seal([]byte("whsec_abc"), []byte("we_1"))
	if err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Clone(sealed)
	tampered[len(tampered)-1] ^= 1

	tests := map[string]func() ([]byte, error){
		"another row's associated data": func() ([]byte, error) { return box.Open(sealed, []byte("we_2")) },
		"another key":                   func() ([]byte, error) { return other.Open(sealed, []byte("we_1")) },
		"a flipped bit":                 func() ([]byte, error) { return box.Open(tampered, []byte("we_1")) },
		"a truncated value":             func() ([]byte, error) { return box.Open(sealed[:5], []byte("we_1")) },
	}
	for name, open := range tests {
		if _, err := open(); !errors.Is(err, secretbox.ErrOpen) {
			t.Errorf("%s: error = %v, want ErrOpen", name, err)
		}
	}
}

func TestFromBase64RejectsWeakKeys(t *testing.T) {
	for _, key := range []string{"", "not base64!", base64.StdEncoding.EncodeToString(make([]byte, 16))} {
		if _, err := secretbox.FromBase64(key); err == nil {
			t.Errorf("FromBase64(%q) accepted a key that is not 32 bytes", key)
		}
	}
}
