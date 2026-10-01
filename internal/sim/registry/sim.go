// Package registry simulates a receivables registry (registradora) for card
// receivables: accreditors register their merchants' receivable units, financiers
// place contract effects on them, and the registry says whom each unit settles to, in
// the order the Convenção entre Entidades Registradoras sets. It stands for another
// company: nothing in Jupiter imports it, and Jupiter reaches it only over HTTP. The
// README says what is simulated and how faithfully.
package registry

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/iricardofernandes/jupiter/pkg/bizday"
	"github.com/iricardofernandes/jupiter/pkg/registryapi"
)

// maxLate bounds the list of missed deadlines the simulator keeps.
const maxLate = 10_000

// brasilia is the time zone of settlement dates and deadlines.
var brasilia = time.FixedZone("BRT", -3*60*60)

type Role string

const (
	Accreditor Role = "accreditor"
	Financier  Role = "financier"
)

// Participant is a registry user: an accreditor (credenciador or subcredenciador) or a
// financier, by its CNPJ, and the token it authenticates with.
type Participant struct {
	Token string
	TaxID string
	Role  Role
}

type Config struct {
	Now          func() time.Time
	Logger       *slog.Logger
	Participants []Participant
}

type Sim struct {
	cfg    Config
	byTok  map[string]Participant
	mu     sync.Mutex
	engine *engine
	optIns map[registryapi.OptIn]bool
	late   []late
}

type late struct {
	accreditor string
	registryapi.Lateness
}

func New(cfg Config) *Sim {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	s := &Sim{cfg: cfg, byTok: map[string]Participant{}, engine: newEngine(), optIns: map[registryapi.OptIn]bool{}}
	for _, p := range cfg.Participants {
		s.byTok[p.Token] = p
	}
	return s
}

// deadline is the end of the business day after day, in Brasília: when an update for a
// sale on day, or the notice of a settlement on day, is due (Res. BCB 264 art. 3º §1º;
// Convenção 5.3.6).
func deadline(day string) (time.Time, error) {
	d, err := time.ParseInLocation(time.DateOnly, day, brasilia)
	if err != nil {
		return time.Time{}, err
	}
	return bizday.Add(d, 1).AddDate(0, 0, 1), nil
}

func (s *Sim) checkLate(kind string, k UnitKey, day string) error {
	due, err := deadline(day)
	if err != nil {
		return fmt.Errorf("%w: date %q", ErrInvalid, day)
	}
	if now := s.cfg.Now(); now.After(due) && len(s.late) < maxLate {
		s.late = append(s.late, late{accreditor: k.Accreditor, Lateness: registryapi.Lateness{
			Kind: kind, Holder: k.Holder, Arrangement: k.Arrangement, SettlementDate: k.Date,
			Due: due.AddDate(0, 0, -1).Format(time.DateOnly), At: now.In(brasilia).Format(time.RFC3339),
		}})
	}
	return nil
}

// SetUnits registers or updates an accreditor's units, each on its own: one refused
// does not stop the others.
func (s *Sim) SetUnits(accreditor string, units []registryapi.Unit) []registryapi.UnitResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]registryapi.UnitResult, 0, len(units))
	for _, u := range units {
		k := UnitKey{Accreditor: accreditor, Holder: u.Holder, Arrangement: u.Arrangement, Date: u.SettlementDate}
		r := registryapi.UnitResult{Holder: u.Holder, Arrangement: u.Arrangement, SettlementDate: u.SettlementDate}
		err := validKey(k)
		if err == nil {
			_, err = deadline(u.ConstitutedOn)
		}
		if err == nil {
			err = s.engine.SetUnit(k, u.Value, u.Blocked, Domicile(u.Domicile))
		}
		if err == nil {
			err = s.checkLate("update", k, u.ConstitutedOn)
		}
		if err != nil {
			r.Error = err.Error()
		}
		out = append(out, r)
	}
	return out
}

func validKey(k UnitKey) error {
	if _, err := time.Parse(time.DateOnly, k.Date); err != nil || k.Holder == "" || k.Arrangement == "" {
		return fmt.Errorf("%w: a unit needs a holder, an arrangement and a settlement date", ErrInvalid)
	}
	return nil
}

// Settle records that an accreditor settled a unit, and how the registry splits it. The
// same notice again answers the same split.
func (s *Sim) Settle(accreditor string, n registryapi.Settlement) ([]registryapi.Payment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := UnitKey{Accreditor: accreditor, Holder: n.Holder, Arrangement: n.Arrangement, Date: n.SettlementDate}
	if u := s.engine.units[k]; u != nil && u.settled {
		var paid int64
		for _, p := range u.payments {
			if p.To != accreditor || p.Contract != "" {
				paid += p.Amount
			}
		}
		if paid != n.Amount {
			return nil, fmt.Errorf("%w: %s settled for %d, not %d", ErrSettled, k, paid, n.Amount)
		}
		return payments(u.payments), nil
	}
	if _, err := deadline(n.SettledOn); err != nil {
		return nil, fmt.Errorf("%w: settled_on %q", ErrInvalid, n.SettledOn)
	}
	out, err := s.engine.Settle(k, n.Amount)
	if err != nil {
		return nil, err
	}
	return payments(out), s.checkLate("settlement", k, n.SettledOn)
}

func payments(in []Payment) []registryapi.Payment {
	out := make([]registryapi.Payment, 0, len(in))
	for _, p := range in {
		out = append(out, registryapi.Payment{To: p.To, Contract: ownID(p.To, p.Contract), Amount: p.Amount, Domicile: registryapi.Domicile(p.Domicile)})
	}
	return out
}

// ownID is a contract's id as its financier named it.
func ownID(financier, id string) string { return strings.TrimPrefix(id, financier+":") }

// Instructions say how an accreditor's unit settles now.
func (s *Sim) Instructions(k UnitKey) ([]registryapi.Payment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out, err := s.engine.Instructions(k)
	return payments(out), err
}

// contractID keeps each financier's contract ids its own.
func contractID(financier, id string) string { return financier + ":" + id }

// Accept registers a financier's contract.
func (s *Sim) Accept(financier string, c registryapi.Contract) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.ID == "" {
		return fmt.Errorf("%w: a contract needs an id", ErrInvalid)
	}
	if old := s.engine.contracts[contractID(financier, c.ID)]; old != nil {
		// The same contract again, after an answer was lost, is accepted again.
		if old.Holder == c.Holder && string(old.Effect) == c.Effect && string(old.Rule) == c.Rule && old.Amount == c.Amount &&
			old.BasisPoints == c.BasisPoints && slices.Equal(old.Arrangements, c.Arrangements) &&
			slices.Equal(old.Accreditors, c.Accreditors) && old.From == c.From && old.To == c.To {
			return nil
		}
		return fmt.Errorf("%w: contract %s exists with other terms", ErrInvalid, c.ID)
	}
	return s.engine.Accept(Contract{
		ID: contractID(financier, c.ID), Beneficiary: financier, Holder: c.Holder, Effect: Effect(c.Effect), Rule: Rule(c.Rule), Amount: c.Amount,
		BasisPoints: c.BasisPoints, Arrangements: c.Arrangements, Accreditors: c.Accreditors, From: c.From, To: c.To,
		Domicile: Domicile(c.Domicile),
	})
}

// End ends a financier's contract.
func (s *Sim) End(financier, contract string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := contractID(financier, contract)
	if s.engine.contracts[id] == nil {
		return fmt.Errorf("%w: contract %s", ErrNotFound, contract)
	}
	return s.engine.End(id)
}

// SetOptIn records a holder's opt-in, or its revocation, as an accreditor of the holder's
// units passes it on.
func (s *Sim) SetOptIn(accreditor string, o registryapi.OptIn, on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	accredits := false
	for k := range s.engine.units {
		accredits = accredits || (k.Accreditor == accreditor && k.Holder == o.Holder)
	}
	if !accredits {
		return fmt.Errorf("%w: %s has no units of %s", ErrForbidden, accreditor, o.Holder)
	}
	if on {
		s.optIns[o] = true
	} else {
		delete(s.optIns, o)
	}
	return nil
}

func (s *Sim) OptedIn(o registryapi.OptIn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.optIns[o]
}

// Positions lists units: those of accreditor, or, for a financier, of a holder that
// opted in to show them. Holder, from and to narrow them when set.
func (s *Sim) Positions(p Participant, holder, from, to string, settled *bool) ([]registryapi.Position, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.Role == Financier && (holder == "" || !s.optIns[registryapi.OptIn{Holder: holder, Financier: p.TaxID}]) {
		return nil, fmt.Errorf("%w: %s has not authorized %s to see its agenda", ErrForbidden, holder, p.TaxID)
	}
	positions := s.engine.Positions(func(k UnitKey) bool {
		return (p.Role == Financier || k.Accreditor == p.TaxID) && (holder == "" || k.Holder == holder) &&
			(from == "" || k.Date >= from) && (to == "" || k.Date <= to)
	})
	out := make([]registryapi.Position, 0, len(positions))
	for _, pos := range positions {
		if settled != nil && pos.Settled != *settled {
			continue
		}
		out = append(out, wirePosition(pos))
	}
	return out, nil
}

func wirePosition(p Position) registryapi.Position {
	out := registryapi.Position{
		Accreditor: p.Key.Accreditor, Holder: p.Key.Holder, Arrangement: p.Key.Arrangement, SettlementDate: p.Key.Date,
		Value: p.Value, Blocked: p.Blocked, Free: p.Free, Settled: p.Settled, Payments: payments(p.Payments),
		Domicile: registryapi.Domicile(p.Domicile), Committed: []registryapi.Commitment{},
	}
	for _, c := range p.Committed {
		out.Committed = append(out.Committed, registryapi.Commitment{Contract: ownID(c.Beneficiary, c.Contract), Beneficiary: c.Beneficiary, Effect: string(c.Effect), Amount: c.Amount})
	}
	return out
}

// HoldersWithContracts are the holders of accreditor's units with something committed
// to a contract on a unit not yet settled.
func (s *Sim) HoldersWithContracts(accreditor string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	for k, u := range s.engine.units {
		if k.Accreditor != accreditor || u.settled {
			continue
		}
		for _, amount := range u.committed {
			seen[k.Holder] = seen[k.Holder] || amount > 0
		}
	}
	out := make([]string, 0, len(seen))
	for h, committed := range seen {
		if committed {
			out = append(out, h)
		}
	}
	slices.Sort(out)
	return out
}

// Late lists the deadlines accreditor missed.
func (s *Sim) Late(accreditor string) []registryapi.Lateness {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []registryapi.Lateness{}
	for _, l := range s.late {
		if l.accreditor == accreditor {
			out = append(out, l.Lateness)
		}
	}
	return out
}
