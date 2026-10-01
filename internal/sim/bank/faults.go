package bank

import (
	"fmt"
	"time"

	"github.com/iricardofernandes/jupiter/pkg/bizday"
	"github.com/iricardofernandes/jupiter/pkg/cnab240"
)

// Event is a record the bank is about to write, for a test to disturb: a statement line
// ("statement") or a return record ("return"), named by its reference (a boleto's nosso
// número, a transfer's id, a settlement's reference).
type Event struct {
	Kind      string
	Reference string
	// Paid is set on a return record that reports a payment.
	Paid bool
}

// Fault is what happens to one record: lost, written twice, or late (a statement line on
// the next business day, a return record two files later).
type Fault struct {
	Drop      bool
	Duplicate bool
	Delay     bool
}

// returnDelay is how many closes a delayed return record waits: past the day its
// payment is credited.
const returnDelay = 2

type heldReturn struct {
	title  cnab240.ReturnTitle
	closes int
}

func (s *Sim) fault(e Event) Fault {
	if s.cfg.Faults == nil {
		return Fault{}
	}
	return s.cfg.Faults(e)
}

// book writes a statement line, as a fault says.
func (s *Sim) book(c *client, e Entry) {
	f := s.fault(Event{Kind: "statement", Reference: e.Reference})
	if f.Drop {
		return
	}
	if f.Delay {
		day, _ := time.Parse(time.DateOnly, e.Date)
		e.Date = bizday.Next(day.AddDate(0, 0, 1)).Format(time.DateOnly)
	}
	for range 1 + btoi(f.Duplicate) {
		c.entries++
		e.ID = fmt.Sprintf("L%08d", c.entries)
		c.statement = append(c.statement, e)
	}
}

// queue puts a return record in the next return file, as a fault says.
func (s *Sim) queue(c *client, t cnab240.ReturnTitle) {
	f := s.fault(Event{Kind: "return", Reference: t.T.OurNumber, Paid: t.T.Occurrence.Paid() && t.U.Paid > 0})
	switch {
	case f.Drop:
	case f.Delay:
		c.held = append(c.held, heldReturn{title: t, closes: returnDelay})
	default:
		for range 1 + btoi(f.Duplicate) {
			c.pending = append(c.pending, t)
		}
	}
}

// release moves the delayed return records whose time has come into the next file.
func (c *client) release() {
	var still []heldReturn
	for _, h := range c.held {
		if h.closes--; h.closes > 0 {
			still = append(still, h)
			continue
		}
		c.pending = append(c.pending, h.title)
	}
	c.held = still
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Credit is a payment into a client's account from another institution, such as the
// SLC's settlement: a statement line on day.
func (s *Sim) Credit(taxID, day string, amount int64, reference, description string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := time.Parse(time.DateOnly, day); err != nil || amount <= 0 || reference == "" {
		return fmt.Errorf("%w: a credit needs a day, a positive amount and a reference", ErrInvalid)
	}
	for _, c := range s.byToken {
		if c.TaxID == taxID {
			s.book(c, Entry{Date: day, Kind: "credit", Amount: amount, Reference: reference, Description: description})
			return nil
		}
	}
	return fmt.Errorf("%w: no account of %s", ErrNotFound, taxID)
}
