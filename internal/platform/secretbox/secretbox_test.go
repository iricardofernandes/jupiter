package secretbox_test

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
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

// A value names its key: after a new key is made the active one, what the old sealed
// still opens, and what is sealed now is under the new. A value sealed before keys had
// ids opens too.
func TestKeysRotate(t *testing.T) {
	k1 := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	k2 := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
	old, err := secretbox.FromBase64("a:" + k1)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := old.Seal([]byte("secret"), []byte("row 1"))
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := secretbox.FromBase64("b:" + k2 + ", a:" + k1)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := rotated.Open(sealed, []byte("row 1")); err != nil || string(got) != "secret" {
		t.Fatalf("a value sealed under the old key: %q, %v", got, err)
	}
	fresh, _ := rotated.Seal([]byte("secret"), []byte("row 1"))
	if _, err := old.Open(fresh, []byte("row 1")); !errors.Is(err, secretbox.ErrOpen) {
		t.Fatalf("the old key alone opened a value sealed under the new: %v", err)
	}
	if _, err := rotated.Open(fresh, []byte("row 2")); !errors.Is(err, secretbox.ErrOpen) {
		t.Fatal("a value opened for another row")
	}

	block, _ := aes.NewCipher(bytes.Repeat([]byte{1}, 32))
	aead, _ := cipher.NewGCM(block)
	nonce := bytes.Repeat([]byte{9}, aead.NonceSize())
	legacy := aead.Seal(nonce, nonce, []byte("before"), []byte("row 1"))
	if got, err := rotated.Open(legacy, []byte("row 1")); err != nil || string(got) != "before" {
		t.Fatalf("a value sealed before keys had ids: %q, %v", got, err)
	}
	for _, bad := range []string{"a:" + k1 + ",a:" + k2, ":" + k1, "a:short"} {
		if _, err := secretbox.FromBase64(bad); err == nil {
			t.Fatalf("%q was accepted", bad)
		}
	}
}
