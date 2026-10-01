package bank

import (
	"fmt"
	"time"
)

// TransferStatus is where a transfer out of the account stands.
type TransferStatus string

const (
	TransferProcessing TransferStatus = "processing"
	TransferCompleted  TransferStatus = "completed"
	TransferFailed     TransferStatus = "failed"
	// TransferReturned is a completed transfer the receiving bank sent back.
	TransferReturned TransferStatus = "returned"
)

// TransferRequest is a transfer to another bank's account.
type TransferRequest struct {
	ID      string `json:"id"`
	Amount  int64  `json:"amount"`
	ISPB    string `json:"ispb"`
	Branch  string `json:"branch"`
	Account string `json:"account"`
	TaxID   string `json:"tax_id"`
	Name    string `json:"name"`
}

// Transfer is a transfer as the bank has it.
type Transfer struct {
	TransferRequest
	Status      TransferStatus `json:"status"`
	Reason      string         `json:"reason,omitempty"`
	CompletedAt *time.Time     `json:"completed_at,omitempty"`
	ReturnedAt  *time.Time     `json:"returned_at,omitempty"`
}

type transfer struct {
	Transfer
	returnOnTick bool
}

// Test accounts with a fate of their own: a transfer to account 999999 is refused, one to
// 888888 is made and then returned by the receiving bank.
const (
	accountRefused  = "999999"
	accountReturned = "888888"
)

// Transfer takes a transfer, made at the next tick. The same id again answers the same
// transfer; with other terms, it is refused.
func (s *Sim) Transfer(token string, r TransferRequest) (Transfer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.byToken[token]
	if c == nil {
		return Transfer{}, fmt.Errorf("%w: unknown client", ErrNotFound)
	}
	if t := c.transfers[r.ID]; t != nil {
		if t.TransferRequest != r {
			return Transfer{}, fmt.Errorf("%w: transfer %s was another", ErrConflict, r.ID)
		}
		return t.Transfer, nil
	}
	if r.ID == "" || r.Amount <= 0 || len(r.ISPB) != 8 || r.Account == "" || r.TaxID == "" || r.Name == "" {
		return Transfer{}, fmt.Errorf("%w: a transfer needs an id, a positive amount, an ISPB, an account, a tax id and a name", ErrInvalid)
	}
	t := &transfer{Transfer: Transfer{TransferRequest: r, Status: TransferProcessing}}
	if r.Account == accountRefused {
		t.Status, t.Reason = TransferFailed, "AC01" // conta inexistente
	}
	c.transfers[r.ID] = t
	return t.Transfer, nil
}

// TransferStatusOf reads a transfer.
func (s *Sim) TransferStatusOf(token, id string) (Transfer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.byToken[token]
	if c == nil || c.transfers[id] == nil {
		return Transfer{}, fmt.Errorf("%w: transfer %s", ErrNotFound, id)
	}
	return c.transfers[id].Transfer, nil
}

// advanceTransfers makes the transfers waiting, and returns, a tick later, those to the
// account that sends them back.
func (s *Sim) advanceTransfers(c *client) {
	now := s.cfg.Now()
	day := s.today().Format(time.DateOnly)
	for _, t := range c.transfers {
		switch {
		case t.Status == TransferProcessing:
			t.Status, t.CompletedAt = TransferCompleted, &now
			t.returnOnTick = t.Account == accountReturned
			c.statement = append(c.statement, Entry{Date: day, Kind: "debit", Amount: t.Amount, Reference: t.ID, Description: "TRANSFERENCIA ENVIADA"})
		case t.Status == TransferCompleted && t.returnOnTick:
			t.Status, t.Reason, t.ReturnedAt, t.returnOnTick = TransferReturned, "AC04", &now, false // conta encerrada
			c.statement = append(c.statement, Entry{Date: day, Kind: "credit", Amount: t.Amount, Reference: t.ID, Description: "DEVOLUCAO TRANSFERENCIA"})
		}
	}
}

// Statement is a client's statement for a day.
func (s *Sim) Statement(token string, day string) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.byToken[token]
	if c == nil {
		return nil, fmt.Errorf("%w: unknown client", ErrNotFound)
	}
	out := []Entry{}
	for _, e := range c.statement {
		if e.Date == day {
			out = append(out, e)
		}
	}
	return out, nil
}
