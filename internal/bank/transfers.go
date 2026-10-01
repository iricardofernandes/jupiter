package bank

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/iricardofernandes/jupiter/internal/payments"
)

var _ payments.BankTransferRail = (*Connector)(nil)

type transferBody struct {
	ID      string `json:"id"`
	Amount  int64  `json:"amount"`
	ISPB    string `json:"ispb"`
	Branch  string `json:"branch"`
	Account string `json:"account"`
	TaxID   string `json:"tax_id"`
	Name    string `json:"name"`
}

type transferAnswer struct {
	ID          string     `json:"id"`
	Status      string     `json:"status"`
	Reason      string     `json:"reason"`
	CompletedAt *time.Time `json:"completed_at"`
}

// Transfer asks the bank for a transfer, named by its id. A refusal (the bank's 4xx)
// wraps payments.ErrPixRefused, the payouts' word for an answer that it failed.
func (c *Connector) Transfer(ctx context.Context, t payments.BankTransferRequest) (payments.PixTransfer, error) {
	d := t.Destination
	body, err := json.Marshal(transferBody{
		ID: t.ID, Amount: t.Amount.Minor(), ISPB: d.ISPB, Branch: d.Branch, Account: d.Account, TaxID: d.HolderTaxID, Name: d.HolderName,
	})
	if err != nil {
		return payments.PixTransfer{}, err
	}
	var out transferAnswer
	_, status, err := c.call(ctx, http.MethodPost, "/v1/transfers", "application/json", body, &out)
	if err != nil {
		// Only an explicit refusal is final: anything else may have been made, and is
		// asked after.
		if status == http.StatusBadRequest || status == http.StatusUnprocessableEntity {
			return payments.PixTransfer{}, fmt.Errorf("%w: %w", payments.ErrPixRefused, err)
		}
		return payments.PixTransfer{}, err
	}
	return transferOf(out), nil
}

// TransferStatus reads a transfer; one the bank does not have wraps payments.ErrPixNotFound.
func (c *Connector) TransferStatus(ctx context.Context, id string) (payments.PixTransfer, error) {
	var out transferAnswer
	_, status, err := c.call(ctx, http.MethodGet, "/v1/transfers/"+url.PathEscape(id), "", nil, &out)
	if status == http.StatusNotFound {
		return payments.PixTransfer{}, fmt.Errorf("%w: %w", payments.ErrPixNotFound, err)
	}
	if err != nil {
		return payments.PixTransfer{}, err
	}
	return transferOf(out), nil
}

func transferOf(a transferAnswer) payments.PixTransfer {
	t := payments.PixTransfer{ID: a.ID, Reason: a.Reason, Status: payments.PixTransferProcessing}
	switch a.Status {
	case "completed":
		t.Status = payments.PixTransferPaid
	case "failed":
		t.Status = payments.PixTransferFailed
	case "returned":
		t.Status = payments.PixTransferReturned
	}
	if a.CompletedAt != nil {
		t.SettledAt = *a.CompletedAt
	}
	return t
}
