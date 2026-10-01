// Package receivables keeps what card networks will pay each merchant and when: one
// receivable unit per merchant, arrangement and settlement date, constituted by the
// installments of every card payment captured, net of Jupiter's fee, and reduced by
// refunds. It registers the units with a receivables registry, as a sub-acquirer must
// (Res. BCB 264/2022), reconciles with it, and shows each merchant its agenda.
package receivables

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/merchant"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/receivables/migrations"
	"github.com/iricardofernandes/jupiter/pkg/registryapi"
)

var UnitPrefix = id.MustPrefix("ur")

var (
	ErrInvalid          = errors.New("receivables: invalid request")
	ErrNotFound         = errors.New("receivables: not found")
	ErrNoRegistry       = errors.New("receivables: no registry in this mode")
	ErrRegistryRefused  = errors.New("receivables: the registry refused")
	ErrRegistryNotFound = errors.New("receivables: the registry has no such unit")
)

// Registry is the receivables registry Jupiter registers its units with, as an
// accreditor.
type Registry interface {
	SetUnits(ctx context.Context, units []registryapi.Unit) ([]registryapi.UnitResult, error)
	// Units lists Jupiter's units at the registry: all of them, or those settled or not.
	Units(ctx context.Context, settled *bool, from, to string) ([]registryapi.Position, error)
	Instructions(ctx context.Context, holder, arrangement, settlementDate string) ([]registryapi.Payment, error)
	Settle(ctx context.Context, n registryapi.Settlement) ([]registryapi.Payment, error)
	SetOptIn(ctx context.Context, o registryapi.OptIn, on bool) error
	HoldersWithContracts(ctx context.Context) ([]string, error)
}

type Config struct {
	Pool      *pgxpool.Pool
	Merchants *merchant.Service
	// Registries serve each mode; units of a mode without one are kept, not registered.
	LiveRegistry Registry
	TestRegistry Registry
	// Domicile is where the units of every merchant settle: the merchants' payment
	// accounts at Jupiter, which pays them out.
	Domicile registryapi.Domicile
	Now      func() time.Time
	Logger   *slog.Logger
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
	return postgres.Migrate(ctx, pool, "receivables", migrations.FS)
}

func (s *Service) registry(livemode bool) (Registry, error) {
	r := s.cfg.TestRegistry
	if livemode {
		r = s.cfg.LiveRegistry
	}
	if r == nil {
		return nil, ErrNoRegistry
	}
	return r, nil
}

// brasilia is the time zone of settlement dates.
var brasilia = time.FixedZone("BRT", -3*60*60)

func dayOf(t time.Time) time.Time {
	y, m, d := t.In(brasilia).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// arrangements are the credit arrangements of the schemes Jupiter accepts.
var arrangements = map[string]string{
	"visa":       registryapi.ArrangementVisaCredit,
	"mastercard": registryapi.ArrangementMastercardCredit,
	"elo":        registryapi.ArrangementEloCredit,
	"amex":       registryapi.ArrangementAmexCredit,
	"hipercard":  registryapi.ArrangementHipercardCredit,
}
