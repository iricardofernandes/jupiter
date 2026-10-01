// Package slcapi is the wire format of the centralized settlement simulator (sim-slc),
// shared by the simulator and Jupiter's connector. Núclea's own layouts were not
// available to the research, so this format is the simulator's.
package slcapi

// Domicile is where an entry is paid.
type Domicile struct {
	ISPB    string `json:"ispb"`
	Branch  string `json:"branch,omitempty"`
	Account string `json:"account"`
}

// Entry is one payment of a grade: of a unit (holder, arrangement, settlement date) to a
// beneficiary, at its domicile; a contract's when Contract is set.
type Entry struct {
	ID             string   `json:"id"`
	Holder         string   `json:"holder"`
	Arrangement    string   `json:"arrangement"`
	SettlementDate string   `json:"settlement_date"`
	Beneficiary    string   `json:"beneficiary"`
	Contract       string   `json:"contract,omitempty"`
	Amount         int64    `json:"amount"`
	Domicile       Domicile `json:"domicile"`
}

// Grade statuses.
const (
	Accepted = "accepted"
	Settled  = "settled"
)

// Grade is a day's settlement as submitted, and how it went: Credited is what was paid
// to the participant's own settlement account, PaidOther what went to other
// institutions directly.
type Grade struct {
	Date      string  `json:"date"`
	Status    string  `json:"status,omitempty"`
	Entries   []Entry `json:"entries"`
	Total     int64   `json:"total,omitempty"`
	Credited  int64   `json:"credited,omitempty"`
	PaidOther int64   `json:"paid_other,omitempty"`
}

// Report is the notice of an anticipation: a unit's amount paid early, on a day.
type Report struct {
	ID             string `json:"id"`
	Holder         string `json:"holder"`
	Arrangement    string `json:"arrangement"`
	SettlementDate string `json:"settlement_date"`
	Amount         int64  `json:"amount"`
	AnticipatedOn  string `json:"anticipated_on"`
}

type ReportsRequest struct {
	Reports []Report `json:"reports"`
}

// Late is a deadline the participant missed.
type Late struct {
	Report string `json:"report"`
	Due    string `json:"due"`
	At     string `json:"at"`
}

type ComplianceResponse struct {
	Late []Late `json:"late"`
}
