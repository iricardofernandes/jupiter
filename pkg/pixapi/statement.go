package pixapi

// The API Pix has no account statement. A receiving bank gives its clients one, of the
// movements the SPI settled in their account; this is the simulator's format for it.

// Statement line types.
const (
	StatementCredit = "CREDITO"
	StatementDebit  = "DEBITO"
)

// StatementLine is one movement of the account. ID names the line, a duplicate included;
// Referencia is what the client knows it by: a Pix received's endToEndId, a transfer's
// idEnvio, a return's id.
type StatementLine struct {
	ID         string `json:"id"`
	Data       string `json:"data"`
	Tipo       string `json:"tipo"`
	Valor      string `json:"valor"`
	Referencia string `json:"referencia"`
	EndToEndID string `json:"endToEndId,omitempty"`
	Descricao  string `json:"descricao"`
}

type Statement struct {
	Lancamentos []StatementLine `json:"lancamentos"`
}
