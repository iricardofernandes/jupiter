package pixapi

import "time"

// MED, the Mecanismo Especial de Devolução, lives in the DICT, between participants: the
// payer's bank reports an infraction against a Pix its client paid, and the receiving
// bank analyses it, blocks the money and returns it. A receiving bank's clients learn of
// it from their bank, which the API Pix does not cover. This is the format of the
// simulator's relay, after the DICT's own names; it is not a published specification.
// Its returns are devoluções of the API Pix, of nature MED_FRAUDE.

// Infraction report statuses and analysis results.
const (
	InfractionOpen    = "OPEN"
	InfractionClosed  = "CLOSED"
	AnalysisAgreed    = "AGREED"
	AnalysisDisagreed = "DISAGREED"
	ReasonFraud       = "FRAUD"
)

// Contestation statuses: a receiving client contests a MED return, which the payer's
// bank upholds or rejects.
const (
	ContestationPending  = "PENDING"
	ContestationUpheld   = "UPHELD"
	ContestationRejected = "REJECTED"
)

// Scopes of the relay.
const (
	ScopeInfractionRead  = "infracao.read"
	ScopeInfractionWrite = "infracao.write"
)

type InfractionReport struct {
	ID            string `json:"id"`
	EndToEndID    string `json:"endToEndId"`
	Reason        string `json:"reason"`
	ReportDetails string `json:"reportDetails,omitempty"`
	// Valor is what the payer claims.
	Valor           string `json:"valor"`
	Status          string `json:"status"`
	AnalysisResult  string `json:"analysisResult,omitempty"`
	AnalysisDetails string `json:"analysisDetails,omitempty"`
	// ContestedAt is when the payer contested the Pix in its bank's app; CreationTime when
	// the report reached the receiving bank, the notification window's end.
	ContestedAt  time.Time `json:"contestedAt"`
	CreationTime time.Time `json:"creationTime"`
	LastModified time.Time `json:"lastModified"`
	// The return the receiving client asked for when it agreed, and how it went.
	RefundID     string        `json:"refundId,omitempty"`
	RefundStatus string        `json:"refundStatus,omitempty"`
	RefundValor  string        `json:"refundValor,omitempty"`
	FundsTrace   []TraceHop    `json:"fundsTrace,omitempty"`
	Contestation *Contestation `json:"contestation,omitempty"`
}

// TraceHop is a Pix that took money the receiving client could not hold onward.
type TraceHop struct {
	EndToEndID string `json:"endToEndId"`
	Valor      string `json:"valor"`
}

type Contestation struct {
	Status  string `json:"status"`
	Details string `json:"details,omitempty"`
	// ReversalEndToEndID is the Pix that gave an upheld contestation's money back.
	ReversalEndToEndID string `json:"reversalEndToEndId,omitempty"`
}

// InfractionAnalysis is the receiving client's answer: AGREED, with the return it asks
// for (refundId, valor), or DISAGREED; with where the money it could not hold went.
type InfractionAnalysis struct {
	AnalysisResult  string     `json:"analysisResult"`
	AnalysisDetails string     `json:"analysisDetails,omitempty"`
	RefundID        string     `json:"refundId,omitempty"`
	Valor           string     `json:"valor,omitempty"`
	FundsTrace      []TraceHop `json:"fundsTrace,omitempty"`
}

type InfractionContestation struct {
	Details string `json:"details"`
}

type InfractionReports struct {
	InfractionReports []InfractionReport `json:"infractionReports"`
}
