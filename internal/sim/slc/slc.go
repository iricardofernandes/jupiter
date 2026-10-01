// Package slc simulates centralized settlement (the SLC Núclea runs) for a sub-acquirer:
// each day Jupiter sends the grade of what its card receivables settle to whom, and at
// the settlement window the network's money is paid by it, to Jupiter's settlement
// account for what is Jupiter's to pass on and to other institutions directly; Jupiter
// also reports the receivables it anticipated, by the business day after. It stands for
// another company: Jupiter reaches it only over HTTP. The README says what is simulated
// and how faithfully; Núclea's layouts were not available to the research.
package slc

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/iricardofernandes/jupiter/pkg/bizday"
	"github.com/iricardofernandes/jupiter/pkg/slcapi"
)

var (
	ErrInvalid  = errors.New("slc: invalid request")
	ErrNotFound = errors.New("slc: not found")
	ErrConflict = errors.New("slc: conflict")
)

var brasilia = time.FixedZone("BRT", -3*60*60)

// Participant is a sub-acquirer, by its CNPJ and the ISPB of its settlement account.
type Participant struct {
	Token string
	TaxID string
	ISPB  string
}

type Config struct {
	Now          func() time.Time
	Logger       *slog.Logger
	Participants []Participant
	// Credit, if set, pays what a settled grade credited into the participant's settlement
	// account at its bank: a statement line there, referenced CreditReference.
	Credit func(p Participant, date string, amount int64)
}

// CreditReference is how a grade's credit reads on the participant's statement.
func CreditReference(date string) string { return "SLC/" + date }

type (
	Domicile = slcapi.Domicile
	Entry    = slcapi.Entry
	Grade    = slcapi.Grade
	Report   = slcapi.Report
	Late     = slcapi.Late
)

type participant struct {
	Participant
	grades  map[string]*Grade
	reports map[string]Report
	late    []Late
}

type Sim struct {
	cfg     Config
	byToken map[string]*participant
	mu      sync.Mutex
}

func New(cfg Config) *Sim {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	s := &Sim{cfg: cfg, byToken: map[string]*participant{}}
	for _, p := range cfg.Participants {
		s.byToken[p.Token] = &participant{Participant: p, grades: map[string]*Grade{}, reports: map[string]Report{}}
	}
	return s
}

func (s *Sim) today() time.Time {
	y, m, d := s.cfg.Now().In(brasilia).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

const maxEntries = 100_000

// Submit takes a day's grade: for today, a business day, its entries due on or before
// it. The same grade again changes nothing; another for the same day is refused.
func (s *Sim) Submit(token string, g Grade) (Grade, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.byToken[token]
	if p == nil {
		return Grade{}, fmt.Errorf("%w: unknown participant", ErrNotFound)
	}
	today := s.today()
	if g.Date != today.Format(time.DateOnly) || !bizday.IsBusinessDay(today) {
		return Grade{}, fmt.Errorf("%w: a grade is for today, a business day", ErrInvalid)
	}
	if err := validEntries(g.Entries, g.Date); err != nil {
		return Grade{}, err
	}
	if seen := p.grades[g.Date]; seen != nil {
		if !slices.Equal(seen.Entries, g.Entries) {
			return Grade{}, fmt.Errorf("%w: the grade of %s was another", ErrConflict, g.Date)
		}
		return *seen, nil
	}
	stored := &Grade{Date: g.Date, Status: slcapi.Accepted, Entries: slices.Clone(g.Entries)}
	for _, e := range g.Entries {
		stored.Total += e.Amount
	}
	p.grades[g.Date] = stored
	return *stored, nil
}

func validEntries(entries []Entry, date string) error {
	if len(entries) == 0 || len(entries) > maxEntries {
		return fmt.Errorf("%w: a grade has 1 to %d entries", ErrInvalid, maxEntries)
	}
	ids := map[string]bool{}
	for _, e := range entries {
		switch {
		case e.ID == "" || ids[e.ID]:
			return fmt.Errorf("%w: entry ids are given and distinct", ErrInvalid)
		case e.Amount <= 0 || e.Holder == "" || e.Beneficiary == "" || e.Domicile.ISPB == "":
			return fmt.Errorf("%w: entry %s needs a positive amount, a holder, a beneficiary and a domicile", ErrInvalid, e.ID)
		case e.SettlementDate > date:
			return fmt.Errorf("%w: entry %s is due on %s, after the grade's day", ErrInvalid, e.ID, e.SettlementDate)
		}
		ids[e.ID] = true
	}
	return nil
}

// Tick runs the settlement window: every accepted grade is paid, the participant's
// share to its settlement account, the rest to the other institutions.
func (s *Sim) Tick() {
	type credit struct {
		p      Participant
		date   string
		amount int64
	}
	var credits []credit
	defer func() {
		for _, c := range credits {
			s.cfg.Credit(c.p, c.date, c.amount)
		}
	}()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.byToken {
		for _, g := range p.grades {
			if g.Status != slcapi.Accepted {
				continue
			}
			for _, e := range g.Entries {
				if e.Domicile.ISPB == p.ISPB {
					g.Credited += e.Amount
				} else {
					g.PaidOther += e.Amount
				}
			}
			g.Status = slcapi.Settled
			if s.cfg.Credit != nil && g.Credited > 0 {
				credits = append(credits, credit{p: p.Participant, date: g.Date, amount: g.Credited})
			}
		}
	}
}

// GradeOf reads a day's grade.
func (s *Sim) GradeOf(token, date string) (Grade, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.byToken[token]
	if p == nil || p.grades[date] == nil {
		return Grade{}, fmt.Errorf("%w: no grade of %s", ErrNotFound, date)
	}
	return *p.grades[date], nil
}

// ReportAnticipations takes anticipation notices, due by the business day after each was
// made; one later is listed as late. The same notice again changes nothing.
func (s *Sim) ReportAnticipations(token string, reports []Report) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.byToken[token]
	if p == nil {
		return fmt.Errorf("%w: unknown participant", ErrNotFound)
	}
	now := s.cfg.Now()
	for _, r := range reports {
		day, err := time.Parse(time.DateOnly, r.AnticipatedOn)
		if err != nil || r.ID == "" || r.Amount <= 0 {
			return fmt.Errorf("%w: report %q needs an id, a positive amount and its day", ErrInvalid, r.ID)
		}
		if _, ok := p.reports[r.ID]; ok {
			continue
		}
		p.reports[r.ID] = r
		due := bizday.Add(day, 1)
		if now.In(brasilia).After(time.Date(due.Year(), due.Month(), due.Day(), 23, 59, 59, 0, brasilia)) {
			p.late = append(p.late, Late{Report: r.ID, Due: due.Format(time.DateOnly), At: now.In(brasilia).Format(time.RFC3339)})
		}
	}
	return nil
}

// Reports lists what a participant reported, and the deadlines it missed.
func (s *Sim) Reports(token string) ([]Report, []Late) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.byToken[token]
	if p == nil {
		return nil, nil
	}
	out := make([]Report, 0, len(p.reports))
	for _, r := range p.reports {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b Report) int {
		if a.ID < b.ID {
			return -1
		}
		return 1
	})
	return out, slices.Clone(p.late)
}
