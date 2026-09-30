// Package kms is the port to the key management service that holds the vault's
// key-encryption keys, and a local implementation of it. A cloud KMS or an HSM
// implements the same interface: the keys never leave it, and the vault asks it to wrap
// and unwrap each record's data key.
package kms

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/iricardofernandes/jupiter/internal/platform/secretbox"
)

var ErrUnknownKey = errors.New("kms: unknown key")

type KMS interface {
	// ActiveKey names the key new data keys are wrapped under.
	ActiveKey() string
	// Wrap encrypts a data key under keyID. The same context must be passed to Unwrap,
	// which binds a wrapped key to the record it belongs to.
	Wrap(ctx context.Context, keyID string, dataKey, context []byte) ([]byte, error)
	Unwrap(ctx context.Context, keyID string, wrapped, context []byte) ([]byte, error)
	// MAC is HMAC-SHA256 under a key kept for fingerprints. It is never rotated:
	// rotating it would stop the same card matching across tokens.
	MAC(ctx context.Context, data []byte) ([]byte, error)
}

// Local keeps its keys in process memory, from configuration. Keys can be added and
// activated while it runs, which is how a rotation starts without a restart.
type Local struct {
	mu     sync.RWMutex
	keys   map[string]*secretbox.Box
	active string
	mac    []byte
}

const keySize = 32

func NewLocal(keys map[string][]byte, active string, macKey []byte) (*Local, error) {
	if len(macKey) != keySize {
		return nil, fmt.Errorf("kms: the fingerprint key has %d bytes, want %d", len(macKey), keySize)
	}
	l := &Local{keys: map[string]*secretbox.Box{}, mac: append([]byte(nil), macKey...)}
	for id, key := range keys {
		if err := l.Add(id, key); err != nil {
			return nil, err
		}
	}
	if err := l.Activate(active); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *Local) Add(id string, key []byte) error {
	if id == "" || strings.ContainsAny(id, ":,") {
		return fmt.Errorf("kms: key id %q must be non-empty and hold no ':' or ','", id)
	}
	box, err := secretbox.New(key)
	if err != nil {
		return fmt.Errorf("kms: key %s: %w", id, err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.keys[id] = box
	return nil
}

func (l *Local) Activate(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.keys[id]; !ok {
		return fmt.Errorf("%w: %q", ErrUnknownKey, id)
	}
	l.active = id
	return nil
}

// Remove retires a key. Data keys still wrapped under it can no longer be unwrapped, so
// the vault checks that none remain first.
func (l *Local) Remove(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if id == l.active {
		return fmt.Errorf("kms: %s is the active key", id)
	}
	delete(l.keys, id)
	return nil
}

func (l *Local) ActiveKey() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.active
}

func (l *Local) Wrap(_ context.Context, keyID string, dataKey, context []byte) ([]byte, error) {
	box, err := l.key(keyID)
	if err != nil {
		return nil, err
	}
	return box.Seal(dataKey, associated(keyID, context))
}

func (l *Local) Unwrap(_ context.Context, keyID string, wrapped, context []byte) ([]byte, error) {
	box, err := l.key(keyID)
	if err != nil {
		return nil, err
	}
	return box.Open(wrapped, associated(keyID, context))
}

func (l *Local) MAC(_ context.Context, data []byte) ([]byte, error) {
	m := hmac.New(sha256.New, l.mac)
	m.Write(data)
	return m.Sum(nil), nil
}

func (l *Local) key(id string) (*secretbox.Box, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	box, ok := l.keys[id]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKey, id)
	}
	return box, nil
}

func associated(keyID string, context []byte) []byte {
	return append([]byte(keyID+"\x00"), context...)
}

// ParseKeyring reads keys written as "id:base64,id:base64".
func ParseKeyring(spec string) (map[string][]byte, error) {
	keys := map[string][]byte{}
	for entry := range strings.SplitSeq(spec, ",") {
		id, encoded, ok := strings.Cut(strings.TrimSpace(entry), ":")
		if !ok || id == "" {
			return nil, fmt.Errorf("kms: keyring entry %q is not id:base64", entry)
		}
		key, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("kms: key %s is not base64: %w", id, err)
		}
		if _, dup := keys[id]; dup {
			return nil, fmt.Errorf("kms: key %s appears twice", id)
		}
		keys[id] = key
	}
	return keys, nil
}
