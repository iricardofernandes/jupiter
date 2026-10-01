package cardnet

import (
	"strings"
	"time"
)

// Disputes travel over HTTP, outside ISO 8583, the way the networks' own dispute systems
// work (Visa's VROL, Mastercard's Mastercom); their messages here are the simulator's.
// A case moves through stages, and at each the acquirer answers by its deadline:
//
//	chargeback       the issuer disputes a completion; the acquirer represents or accepts
//	pre_arbitration  the issuer rejects the representment; the acquirer escalates or accepts
//	arbitration      the network rules
//
// The network tells the acquirer of every change (DisputeEvent, signed like token events)
// and answers for a case's current state at GET /v1/disputes/{id}.

// Dispute stages.
const (
	StageChargeback     = "chargeback"
	StagePreArbitration = "pre_arbitration"
	StageArbitration    = "arbitration"
)

// Dispute statuses: waiting for the acquirer's answer, waiting for the issuer's or the
// network's, or closed with an outcome.
const (
	DisputeOpen      = "open"
	DisputeResponded = "responded"
	DisputeClosed    = "closed"
)

// Outcomes of a closed case.
const (
	AcquirerWon = "acquirer_won"
	IssuerWon   = "issuer_won"
)

// Acquirer actions.
const (
	ActionRepresent = "represent"
	ActionAccept    = "accept"
	ActionEscalate  = "escalate"
)

// ReasonLiabilityCap is the representment reason that the dispute was opened after the
// participants' liability ended (Res. BCB 522/2025).
const ReasonLiabilityCap = "liability_cap"

// Dispute is a case as the network has it. Version counts its changes: a notice older
// than one already applied is stale.
type Dispute struct {
	ID                   string    `json:"id"`
	NetworkTransactionID string    `json:"network_transaction_id"`
	Acquirer             string    `json:"acquirer"`
	Merchant             string    `json:"merchant"`
	Amount               int64     `json:"amount"`
	Currency             string    `json:"currency"`
	ReasonCode           string    `json:"reason_code"`
	Category             string    `json:"category"`
	Stage                string    `json:"stage"`
	Status               string    `json:"status"`
	Outcome              string    `json:"outcome,omitempty"`
	RespondBy            time.Time `json:"respond_by,omitzero"`
	AuthorizedAt         time.Time `json:"authorized_at"`
	OpenedAt             time.Time `json:"opened_at"`
	Version              int       `json:"version"`
}

// Valid reports whether d is a case an acquirer can apply.
func (d Dispute) Valid() bool {
	switch {
	case d.ID == "" || len(d.ID) > 64 || d.NetworkTransactionID == "" || len(d.NetworkTransactionID) > 15,
		d.Amount <= 0 || d.Currency != "BRL" || d.ReasonCode == "" || len(d.ReasonCode) > 8,
		d.Stage != StageChargeback && d.Stage != StagePreArbitration && d.Stage != StageArbitration,
		d.Status != DisputeOpen && d.Status != DisputeResponded && d.Status != DisputeClosed,
		(d.Status == DisputeClosed) != (d.Outcome == AcquirerWon || d.Outcome == IssuerWon),
		d.Version < 1 || d.OpenedAt.IsZero():
		return false
	}
	return true
}

// DisputeAction is the acquirer's answer at a stage. The same action at the same stage
// again is answered with the case as it is.
type DisputeAction struct {
	Action   string `json:"action"`
	Stage    string `json:"stage"`
	Evidence string `json:"evidence,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// DisputeEvent tells an acquirer that a case changed.
type DisputeEvent struct {
	Type    string  `json:"type"` // dispute.updated
	Dispute Dispute `json:"dispute"`
}

// FraudReport is an issuer's report that a transaction was fraud, apart from any dispute
// (Visa's TC40, Mastercard's SAFE).
type FraudReport struct {
	ID                   string    `json:"id"`
	NetworkTransactionID string    `json:"network_transaction_id"`
	Acquirer             string    `json:"acquirer"`
	Amount               int64     `json:"amount"`
	FraudType            string    `json:"fraud_type"`
	ReportedAt           time.Time `json:"reported_at"`
}

// FraudReportEvent tells an acquirer of a fraud report.
type FraudReportEvent struct {
	Type   string      `json:"type"` // fraud_report.created
	Report FraudReport `json:"report"`
}

// Categories group reason codes the way Visa Claims Resolution does.
const (
	CategoryFraud           = "fraud"
	CategoryAuthorization   = "authorization"
	CategoryProcessingError = "processing_error"
	CategoryConsumer        = "consumer_dispute"
)

// CategoryOf is the category of a Visa (10.x to 13.x) or Mastercard (48xx) reason code,
// or "" for one it does not know.
func CategoryOf(code string) string {
	switch {
	case strings.HasPrefix(code, "10."), code == "4837", code == "4863", code == "4870", code == "4871":
		return CategoryFraud
	case strings.HasPrefix(code, "11."), code == "4808":
		return CategoryAuthorization
	case strings.HasPrefix(code, "12."), code == "4834", code == "4831", code == "4842":
		return CategoryProcessingError
	case strings.HasPrefix(code, "13."), code == "4853", code == "4855", code == "4859", code == "4860":
		return CategoryConsumer
	}
	return ""
}
