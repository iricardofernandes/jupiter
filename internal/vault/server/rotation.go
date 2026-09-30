package server

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/vault/server/db"
)

// Rewrap moves up to batch data keys from older key-encryption keys to the active one
// and returns how many it moved. The numbers are not re-encrypted: only their data keys
// are, which is what makes rotating a key-encryption key cheap. Rows are taken with SKIP
// LOCKED, so several vault instances can rewrap at once, and tokenization and
// detokenization carry on throughout, since each row names the key its data key is
// wrapped under.
func (s *Service) Rewrap(ctx context.Context, batch int32) (int, error) {
	active := s.cfg.KMS.ActiveKey()
	moved := 0
	err := postgres.InTx(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
		moved = 0
		q := db.New(tx)
		rows, err := q.CardsToRewrap(ctx, db.CardsToRewrapParams{ActiveKey: active, MaxCount: batch})
		if err != nil {
			return err
		}
		for _, row := range rows {
			dataKey, err := s.cfg.KMS.Unwrap(ctx, row.KeyID, row.WrappedKey, []byte(row.Token))
			if err != nil {
				return fmt.Errorf("unwrapping the data key of %s: %w", row.Token, err)
			}
			wrapped, err := s.cfg.KMS.Wrap(ctx, active, dataKey, []byte(row.Token))
			clear(dataKey)
			if err != nil {
				return fmt.Errorf("wrapping the data key of %s: %w", row.Token, err)
			}
			if err := q.RewrapCard(ctx, db.RewrapCardParams{WrappedKey: wrapped, KeyID: active, Token: row.Token}); err != nil {
				return err
			}
			moved++
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("rewrapping: %w", err)
	}
	return moved, nil
}

// RewrapAll rewraps until no data key is left under an older key.
func (s *Service) RewrapAll(ctx context.Context, batch int32) (int, error) {
	total := 0
	for {
		n, err := s.Rewrap(ctx, batch)
		total += n
		if err != nil || n == 0 {
			return total, err
		}
	}
}

// KeyUsage counts the cards whose data keys each key-encryption key wraps.
func (s *Service) KeyUsage(ctx context.Context) (map[string]int64, error) {
	rows, err := db.New(s.cfg.Pool).KeyUsage(ctx)
	if err != nil {
		return nil, err
	}
	usage := make(map[string]int64, len(rows))
	for _, r := range rows {
		usage[r.KeyID] = r.Cards
	}
	return usage, nil
}

// CheckRetirable fails while any data key is still wrapped under keyID, so a key is
// never removed from the KMS with cards it alone can open.
func (s *Service) CheckRetirable(ctx context.Context, keyID string) error {
	if keyID == s.cfg.KMS.ActiveKey() {
		return fmt.Errorf("%w: %s is the active key", ErrKeyInUse, keyID)
	}
	n, err := db.New(s.cfg.Pool).CountCardsUnderKey(ctx, keyID)
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("%w: %d cards under %s", ErrKeyInUse, n, keyID)
	}
	return nil
}
