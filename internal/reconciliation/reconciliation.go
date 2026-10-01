// Package reconciliation proves that what Jupiter believes matches what every
// counterparty reports. Each counterparty's records come in streams: the card network's
// clearing files, the SLC's settlement grades, the bank's CNAB returns and statements,
// the Pix bank's SPI statement. Each stream is read from both sides: Jupiter's, from the
// records its ledger postings name, and the counterparty's. They are matched by the rules
// in match.go; what does not match is a break, kept in a queue and aged until it matches
// or someone resolves it. A movement that shows in a rail's record and in a statement is
// matched in both, which makes the three ways: the ledger, the rail and the bank. The
// algorithm is described in docs/reconciliation.md.
package reconciliation

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/reconciliation/migrations"
)

var BreakPrefix = id.MustPrefix("brk")

var (
	ErrInvalid  = errors.New("reconciliation: invalid request")
	ErrNotFound = errors.New("reconciliation: not found")
)

// OursFunc lists Jupiter's records of a stream dated since a day.
type OursFunc func(ctx context.Context, pool *pgxpool.Pool, livemode bool, since time.Time) ([]Record, error)

// TheirsFunc lists the counterparty's records of a stream for one day: its file, its
// grade or its statement of that day.
type TheirsFunc func(ctx context.Context, pool *pgxpool.Pool, livemode bool, day time.Time) ([]Record, error)

// Stream is one record of one counterparty, read from both sides. Jupiter's side may come
// from several places: a statement lists boletos, settlements and transfers alike.
type Stream struct {
	Counterparty string
	Name         string
	Ours         []OursFunc
	Theirs       TheirsFunc
}

// DivergenceFunc lists the divergences another reconciliation keeps open, which become
// breaks of their own: the registry's.
type DivergenceFunc func(ctx context.Context, pool *pgxpool.Pool, livemode bool) ([]Divergence, error)

// Divergence is one of them: what it is about, and what does not match.
type Divergence struct {
	Subject  string
	Detail   string
	FoundOn  time.Time
	Merchant string
}

// Mode is what is reconciled in one mode.
type Mode struct {
	Streams []Stream
	// Divergences, by the counterparty they are with.
	Divergences map[string]DivergenceFunc
}

type Config struct {
	Pool   *pgxpool.Pool
	Live   Mode
	Test   Mode
	Now    func() time.Time
	Logger *slog.Logger
}

type Service struct {
	cfg Config
}

func New(cfg Config) *Service {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Service{cfg: cfg}
}

func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return postgres.Migrate(ctx, pool, "reconciliation", migrations.FS)
}

func (s *Service) mode(livemode bool) Mode {
	if livemode {
		return s.cfg.Live
	}
	return s.cfg.Test
}

// brasilia is the time zone of business days.
var brasilia = time.FixedZone("BRT", -3*60*60)

// Day is the calendar day t falls on in Brasília, at midnight UTC: how days are kept here.
func Day(t time.Time) time.Time {
	y, m, d := t.In(brasilia).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// UTCDay is the calendar day t falls on in UTC, for counterparties that close days there.
func UTCDay(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func date(t time.Time) pgtype.Date {
	return pgtype.Date{Time: time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC), Valid: true}
}

func ts(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t.UTC(), Valid: !t.IsZero()}
}
