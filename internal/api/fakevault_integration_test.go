//go:build integration

package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"sync/atomic"
	"uuid"

	"github.com/iricardofernandes/jupiter/internal/vault"
)

// fakeVault stands in for the vault in the API's tests; test/pci and the golden path
// run against the real one. It keeps the vault's contract: idempotent tokenization by
// request key, tokens made from web pages that wait to be claimed, and security codes
// given out once.
type fakeVault struct {
	mu        sync.Mutex
	cards     map[string]fakeCard
	byRequest map[string]string
	down      atomic.Bool
}

type fakeCard struct {
	vault.Card
	data vault.CardData
}

func newFakeVault() *fakeVault {
	return &fakeVault{cards: map[string]fakeCard{}, byRequest: map[string]string{}}
}

// badNumber is the one number the fake refuses, standing for every invalid card.
const badNumber = "4242424242424241"

func (f *fakeVault) Tokenize(_ context.Context, r vault.TokenizeRequest) (vault.Card, error) {
	r.Card.Number = strings.NewReplacer(" ", "", "-", "").Replace(r.Card.Number)
	if f.down.Load() {
		return vault.Card{}, vault.ErrUnavailable
	}
	if r.Card.Number == badNumber {
		return vault.Card{}, &vault.CardError{Code: "incorrect_number", Param: "number"}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if token, ok := f.byRequest[r.Owner+"|"+r.RequestKey]; ok {
		if f.cards[token].data.Number != r.Card.Number {
			return vault.Card{}, vault.ErrConflict
		}
		return f.cards[token].Card, nil
	}
	c := f.store(r.Card, r.Owner, "")
	f.byRequest[r.Owner+"|"+r.RequestKey] = c.Token
	return c, nil
}

// public is what the vault's tokenization route does for a web page.
func (f *fakeVault) public(publishableKey string, data vault.CardData) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.store(data, "", publishableKey).Token
}

func (f *fakeVault) store(data vault.CardData, owner, publishableKey string) vault.Card {
	sum := sha256.Sum256([]byte(data.Number))
	c := vault.Card{
		Token: vault.TokenPrefix.FromUUID(uuid.NewV4()).String(), Owner: owner, PublishableKey: publishableKey,
		Brand: "visa", BIN: data.Number[:8], Last4: data.Number[len(data.Number)-4:],
		ExpMonth: data.ExpMonth, ExpYear: data.ExpYear, Fingerprint: hex.EncodeToString(sum[:16]),
	}
	f.cards[c.Token] = fakeCard{Card: c, data: data}
	return c
}

func (f *fakeVault) Card(_ context.Context, token string) (vault.Card, error) {
	if f.down.Load() {
		return vault.Card{}, vault.ErrUnavailable
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.cards[token]
	if !ok {
		return vault.Card{}, vault.ErrNotFound
	}
	return c.Card, nil
}

func (f *fakeVault) Claim(_ context.Context, token, owner string) (vault.Card, error) {
	if f.down.Load() {
		return vault.Card{}, vault.ErrUnavailable
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.cards[token]
	switch {
	case !ok || (c.Owner != "" && c.Owner != owner):
		return vault.Card{}, vault.ErrNotFound
	case c.Owner == "":
		c.Owner, c.PublishableKey = owner, ""
		f.cards[token] = c
	}
	return c.Card, nil
}

func (f *fakeVault) Detokenize(_ context.Context, token, owner string) (vault.CardData, error) {
	if f.down.Load() {
		return vault.CardData{}, vault.ErrUnavailable
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.cards[token]
	if !ok || c.Owner == "" || c.Owner != owner {
		return vault.CardData{}, vault.ErrNotFound
	}
	data := c.data
	c.data.CVC = ""
	f.cards[token] = c
	return data, nil
}
