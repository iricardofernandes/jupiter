package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

const keySize = 32

var ErrOpen = errors.New("secretbox: cannot open sealed value")

// Box seals values with AES-256-GCM. The associated data passed to Seal must be passed
// to Open again, which ties a sealed value to the row it belongs to.
type Box struct {
	aead cipher.AEAD
}

func FromBase64(key string) (*Box, error) {
	raw, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		return nil, fmt.Errorf("secretbox: key is not base64: %w", err)
	}
	if len(raw) != keySize {
		return nil, fmt.Errorf("secretbox: key has %d bytes, want %d", len(raw), keySize)
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, fmt.Errorf("secretbox: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secretbox: %w", err)
	}
	return &Box{aead: aead}, nil
}

// Seal returns the nonce followed by the ciphertext.
func (b *Box) Seal(plaintext, associatedData []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize(), b.aead.NonceSize()+len(plaintext)+b.aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secretbox: nonce: %w", err)
	}
	return b.aead.Seal(nonce, nonce, plaintext, associatedData), nil
}

func (b *Box) Open(sealed, associatedData []byte) ([]byte, error) {
	if len(sealed) < b.aead.NonceSize() {
		return nil, ErrOpen
	}
	nonce, ciphertext := sealed[:b.aead.NonceSize()], sealed[b.aead.NonceSize():]
	plaintext, err := b.aead.Open(nil, nonce, ciphertext, associatedData)
	if err != nil {
		return nil, ErrOpen
	}
	return plaintext, nil
}
