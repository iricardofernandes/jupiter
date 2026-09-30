package kms_test

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/iricardofernandes/jupiter/internal/vault/kms"
)

func key() []byte {
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	return k
}

func TestWrapAndUnwrap(t *testing.T) {
	ctx := t.Context()
	l, err := kms.NewLocal(map[string][]byte{"k1": key()}, "k1", key())
	if err != nil {
		t.Fatal(err)
	}
	dek := key()
	wrapped, err := l.Wrap(ctx, "k1", dek, []byte("tok_a"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := l.Unwrap(ctx, "k1", wrapped, []byte("tok_a")); err != nil || !bytes.Equal(got, dek) {
		t.Fatalf("Unwrap = %x, %v", got, err)
	}
	if _, err := l.Unwrap(ctx, "k1", wrapped, []byte("tok_b")); err == nil {
		t.Fatal("a data key unwrapped for another record")
	}
	if _, err := l.Unwrap(ctx, "k2", wrapped, []byte("tok_a")); !errors.Is(err, kms.ErrUnknownKey) {
		t.Fatalf("Unwrap under an unknown key = %v", err)
	}
}

func TestRotation(t *testing.T) {
	ctx := t.Context()
	l, err := kms.NewLocal(map[string][]byte{"k1": key()}, "k1", key())
	if err != nil {
		t.Fatal(err)
	}
	old, _ := l.Wrap(ctx, "k1", []byte("dek"), nil)
	if err := l.Add("k2", key()); err != nil {
		t.Fatal(err)
	}
	if err := l.Activate("k2"); err != nil || l.ActiveKey() != "k2" {
		t.Fatalf("Activate: %v, active %s", err, l.ActiveKey())
	}
	if _, err := l.Unwrap(ctx, "k1", old, nil); err != nil {
		t.Fatalf("the previous key stopped working when a new one was activated: %v", err)
	}
	if err := l.Remove("k2"); err == nil {
		t.Fatal("the active key was removed")
	}
	if err := l.Remove("k1"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Unwrap(ctx, "k1", old, nil); !errors.Is(err, kms.ErrUnknownKey) {
		t.Fatalf("a removed key still unwraps: %v", err)
	}
	if err := l.Activate("k1"); !errors.Is(err, kms.ErrUnknownKey) {
		t.Fatalf("Activate of a removed key = %v", err)
	}
}

func TestMACIsKeyed(t *testing.T) {
	ctx := t.Context()
	a, _ := kms.NewLocal(map[string][]byte{"k": key()}, "k", key())
	b, _ := kms.NewLocal(map[string][]byte{"k": key()}, "k", key())
	x1, _ := a.MAC(ctx, []byte("4242424242424242"))
	x2, _ := a.MAC(ctx, []byte("4242424242424242"))
	y, _ := b.MAC(ctx, []byte("4242424242424242"))
	if !bytes.Equal(x1, x2) || bytes.Equal(x1, y) {
		t.Fatal("MAC is not deterministic under one key, or not different under another")
	}
}

func TestConfigurationErrors(t *testing.T) {
	if _, err := kms.NewLocal(map[string][]byte{"k": key()}, "k", []byte("short")); err == nil {
		t.Error("a short fingerprint key was accepted")
	}
	if _, err := kms.NewLocal(map[string][]byte{"k": key()}, "other", key()); err == nil {
		t.Error("an active key outside the keyring was accepted")
	}
	if _, err := kms.NewLocal(map[string][]byte{"k": []byte("short")}, "k", key()); err == nil {
		t.Error("a short key was accepted")
	}
	if _, err := kms.NewLocal(map[string][]byte{"a:b": key()}, "a:b", key()); err == nil {
		t.Error("a key id with a colon was accepted")
	}
}

func TestParseKeyring(t *testing.T) {
	k1, k2 := key(), key()
	spec := "2026-09:" + base64.StdEncoding.EncodeToString(k1) + ", 2026-10:" + base64.StdEncoding.EncodeToString(k2)
	keys, err := kms.ParseKeyring(spec)
	if err != nil || !bytes.Equal(keys["2026-09"], k1) || !bytes.Equal(keys["2026-10"], k2) {
		t.Fatalf("ParseKeyring = %v, %v", keys, err)
	}
	for _, bad := range []string{"", "nokey", ":abc", "k:not base64!", "k:" + base64.StdEncoding.EncodeToString(k1) + ",k:" + base64.StdEncoding.EncodeToString(k2)} {
		if _, err := kms.ParseKeyring(bad); err == nil {
			t.Errorf("ParseKeyring(%q) succeeded", bad)
		}
	}
}
