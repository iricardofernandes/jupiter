// Package registryapi is the wire format of the receivables registry simulator
// (sim-registry), shared by the simulator and Jupiter's adapter. The registries' own
// layouts were not available to the research, so this format is the simulator's; its
// fields follow the Convenção entre Entidades Registradoras' minimum registration
// payload and agenda response.
package registryapi

// Arrangement codes for credit card arrangements, from the SPB table of payment
// arrangements. Only VCP and MCP (prepaid) were seen in a primary text by the research;
// the credit codes are as commonly published and unverified.
const (
	ArrangementVisaCredit       = "VCC"
	ArrangementMastercardCredit = "MCC"
	ArrangementEloCredit        = "ECC"
	ArrangementAmexCredit       = "ACC"
	ArrangementHipercardCredit  = "HCC"
)

type Domicile struct {
	ISPB    string `json:"ispb"`
	Branch  string `json:"branch,omitempty"`
	Account string `json:"account"`
}

// Unit is a receivable unit as an accreditor registers it. Value is the constituted
// amount, net of fees, reductions and settlements (Res. BCB 264 art. 3º §3º); Blocked
// the part the accreditor holds back (art. 8º). Both are absolute: a lower value is a
// reduction. ConstitutedOn is the latest sale day the update reflects, which the
// registry holds the next-business-day deadline against.
type Unit struct {
	Holder         string   `json:"holder"`
	Arrangement    string   `json:"arrangement"`
	SettlementDate string   `json:"settlement_date"`
	Value          int64    `json:"value"`
	Blocked        int64    `json:"blocked"`
	Domicile       Domicile `json:"domicile"`
	ConstitutedOn  string   `json:"constituted_on"`
}

type UnitsRequest struct {
	Units []Unit `json:"units"`
}

// UnitResult answers one unit of a request: Error is empty when it was taken.
type UnitResult struct {
	Holder         string `json:"holder"`
	Arrangement    string `json:"arrangement"`
	SettlementDate string `json:"settlement_date"`
	Error          string `json:"error,omitempty"`
}

type UnitsResponse struct {
	Results []UnitResult `json:"results"`
}

type Commitment struct {
	Contract    string `json:"contract"`
	Beneficiary string `json:"beneficiary"`
	Effect      string `json:"effect"`
	Amount      int64  `json:"amount"`
}

type Payment struct {
	To       string   `json:"to"`
	Contract string   `json:"contract,omitempty"`
	Amount   int64    `json:"amount"`
	Domicile Domicile `json:"domicile"`
}

// Position is a unit as the registry holds it: what is committed to each contract, in
// the order they were accepted, and what is free.
type Position struct {
	Accreditor     string       `json:"accreditor"`
	Holder         string       `json:"holder"`
	Arrangement    string       `json:"arrangement"`
	SettlementDate string       `json:"settlement_date"`
	Value          int64        `json:"value"`
	Blocked        int64        `json:"blocked"`
	Free           int64        `json:"free"`
	Committed      []Commitment `json:"committed"`
	Settled        bool         `json:"settled"`
	Payments       []Payment    `json:"payments,omitempty"`
	Domicile       Domicile     `json:"domicile"`
}

type PositionsResponse struct {
	Units []Position `json:"units"`
}

type InstructionsResponse struct {
	Payments []Payment `json:"payments"`
}

// Settlement is an accreditor's notice that it settled a unit, by the next business day
// (Convenção 5.3.6).
type Settlement struct {
	Holder         string `json:"holder"`
	Arrangement    string `json:"arrangement"`
	SettlementDate string `json:"settlement_date"`
	Amount         int64  `json:"amount"`
	SettledOn      string `json:"settled_on"`
}

type SettlementResponse struct {
	Payments []Payment `json:"payments"`
}

// OptIn is a merchant's authorization for a financier to see its agenda (Res. BCB 264
// art. 15).
type OptIn struct {
	Holder    string `json:"holder"`
	Financier string `json:"financier"`
}

type Contract struct {
	ID           string   `json:"id"`
	Holder       string   `json:"holder"`
	Effect       string   `json:"effect"`
	Rule         string   `json:"rule"`
	Amount       int64    `json:"amount,omitempty"`
	BasisPoints  int64    `json:"basis_points,omitempty"`
	Arrangements []string `json:"arrangements,omitempty"`
	Accreditors  []string `json:"accreditors,omitempty"`
	From         string   `json:"from,omitempty"`
	To           string   `json:"to,omitempty"`
	Domicile     Domicile `json:"domicile"`
	Beneficiary  string   `json:"beneficiary,omitempty"`
	Ended        bool     `json:"ended,omitempty"`
}

type HoldersResponse struct {
	// Holders with a live contract reaching the accreditor's units (the fortnightly
	// reconciliation, Res. BCB 264 art. 11).
	Holders []string `json:"holders"`
}

// Lateness is a deadline the registry saw an accreditor miss.
type Lateness struct {
	Kind           string `json:"kind"` // "update" or "settlement"
	Holder         string `json:"holder"`
	Arrangement    string `json:"arrangement"`
	SettlementDate string `json:"settlement_date"`
	Due            string `json:"due"`
	At             string `json:"at"`
}

type ComplianceResponse struct {
	Late []Lateness `json:"late"`
}

type Problem struct {
	Error string `json:"error"`
}
