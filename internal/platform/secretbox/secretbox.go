package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

const keySize = 32

var ErrOpen = errors.New("secretbox: cannot open sealed value")

// Box seals values with AES-256-GCM. The associated data passed to Seal must be passed
// to Open again, which ties a sealed value to the row it belongs to.
//
// A sealed value names the key it was sealed under, so keys can be rotated: a new key is
// added and made the active one, new values are sealed under it, and the old key stays
// until nothing sealed under it is left.
type Box struct {
	keys   map[string]cipher.AEAD
	active string
}

// format marks a sealed value that names its key: format, the key id's length, the id,
// the nonce, the ciphertext. Values sealed before keys had ids are a nonce and a
// ciphertext alone.
const format = 1

// FromBase64 reads one key in base64, whose id is then "1", or several as
// id:base64,id:base64, the first the active one.
func FromBase64(keys string) (*Box, error) {
	b := &Box{keys: map[string]cipher.AEAD{}}
	for _, field := range strings.Split(keys, ",") {
		id, encoded, named := strings.Cut(strings.TrimSpace(field), ":")
		if !named {
			id, encoded = "1", id
		}
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("secretbox: key is not base64: %w", err)
		}
		aead, err := newAEAD(raw)
		if err != nil {
			return nil, err
		}
		if _, twice := b.keys[id]; twice || id == "" || len(id) > 255 {
			return nil, fmt.Errorf("secretbox: key id %q is empty, too long or given twice", id)
		}
		b.keys[id] = aead
		if b.active == "" {
			b.active = id
		}
	}
	return b, nil
}

// New seals with a raw 32-byte key, whose id is "1". The Box keeps its own cipher state,
// so the caller may clear key afterwards.
func New(key []byte) (*Box, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	return &Box{keys: map[string]cipher.AEAD{"1": aead}, active: "1"}, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != keySize {
		return nil, fmt.Errorf("secretbox: key has %d bytes, want %d", len(key), keySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secretbox: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secretbox: %w", err)
	}
	return aead, nil
}

// Seal seals under the active key, and returns the key's id, the nonce and the
// ciphertext.
func (b *Box) Seal(plaintext, associatedData []byte) ([]byte, error) {
	aead := b.keys[b.active]
	header := append([]byte{format, byte(len(b.active))}, b.active...) //nolint:gosec // an id is at most 255 bytes
	out := make([]byte, len(header)+aead.NonceSize(), len(header)+aead.NonceSize()+len(plaintext)+aead.Overhead())
	copy(out, header)
	nonce := out[len(header):]
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secretbox: nonce: %w", err)
	}
	return aead.Seal(out, nonce, plaintext, associatedData), nil
}

// Open opens a value under the key it names; one sealed before keys had ids, under
// whichever key opens it.
func (b *Box) Open(sealed, associatedData []byte) ([]byte, error) {
	if len(sealed) > 2 && sealed[0] == format && len(sealed) > 2+int(sealed[1]) {
		if aead, ok := b.keys[string(sealed[2:2+int(sealed[1])])]; ok {
			if plaintext, err := open(aead, sealed[2+int(sealed[1]):], associatedData); err == nil {
				return plaintext, nil
			}
		}
	}
	for _, aead := range b.keys {
		if plaintext, err := open(aead, sealed, associatedData); err == nil {
			return plaintext, nil
		}
	}
	return nil, ErrOpen
}

func open(aead cipher.AEAD, sealed, associatedData []byte) ([]byte, error) {
	if len(sealed) < aead.NonceSize() {
		return nil, ErrOpen
	}
	return aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], associatedData)
}
