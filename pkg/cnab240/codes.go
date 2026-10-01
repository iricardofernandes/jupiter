package cnab240

// Movement is what a remittance asks of a title (FEBRABAN table C004).
type Movement int

const (
	Entry                Movement = 1  // Entrada de títulos
	WriteOffRequest      Movement = 2  // Pedido de baixa
	RebateGrant          Movement = 4  // Concessão de abatimento
	RebateCancel         Movement = 5  // Cancelamento de abatimento
	DueDateChange        Movement = 6  // Alteração de vencimento
	DiscountGrant        Movement = 7  // Concessão de desconto
	DiscountCancel       Movement = 8  // Cancelamento de desconto
	Protest              Movement = 9  // Protestar
	InterestChange       Movement = 12 // Alteração de juros
	InterestWaiver       Movement = 13 // Dispensar juros
	FineChange           Movement = 14 // Alteração de multa
	FineWaiver           Movement = 15 // Dispensar multa
	OtherDataChange      Movement = 31 // Alteração de outros dados
	NominalAmountChange  Movement = 47 // Alteração do valor nominal
	PixQRCodeMaintenance Movement = 61 // Inclusão/manutenção de QR Code Pix
)

// Occurrence is what a return file reports of a title (FEBRABAN table C044).
type Occurrence int

const (
	EntryConfirmed       Occurrence = 2  // Entrada confirmada
	EntryRejected        Occurrence = 3  // Entrada rejeitada
	Settled              Occurrence = 6  // Liquidação
	WrittenOff           Occurrence = 9  // Baixa
	Outstanding          Occurrence = 11 // Em ser
	RebateConfirmed      Occurrence = 12 // Confirmação de abatimento
	DueDateChanged       Occurrence = 14 // Confirmação de alteração de vencimento
	SettledAfterWriteOff Occurrence = 17 // Liquidação após baixa ou título não registrado
	InstructionRejected  Occurrence = 26 // Instrução rejeitada
	OtherDataChanged     Occurrence = 27 // Confirmação de alteração de outros dados
	Charges              Occurrence = 28 // Débito de tarifas/custas
	PayerOccurrence      Occurrence = 29 // Ocorrências do pagador
	ChangeRejected       Occurrence = 30 // Alteração de dados rejeitada
)

// Paid reports whether the occurrence is a settlement: the title was paid.
func (o Occurrence) Paid() bool { return o == Settled || o == SettledAfterWriteOff }

// Reason explains an occurrence (FEBRABAN table C047): why an entry or instruction was
// rejected (group A), what was charged (group B), or how a title was settled or written
// off (group C). The same code means different things in each group.
type Reason string

// Group C reasons: the channel a title was settled through, or who wrote it off.
const (
	ChannelCounterCheque Reason = "30" // Liquidação no guichê de caixa em cheque
	ChannelCorrespondent Reason = "31" // paid at a banking agent
	ChannelATM           Reason = "32" // Liquidação em terminal de autoatendimento
	ChannelInternet      Reason = "33" // Liquidação via internet banking
	ChannelOfficeBanking Reason = "34" // Liquidação via office banking
	ChannelPhone         Reason = "37" // Liquidação via central telefônica
	ChannelPix           Reason = "61" // Liquidado via Pix
	WrittenOffByBank     Reason = "09" // Baixa comandada pelo banco
	WrittenOffByFile     Reason = "10" // Baixa comandada pelo cliente, por arquivo
	WrittenOffOnline     Reason = "11" // Baixa comandada pelo cliente, on-line
)

// Group A reasons used by the simulator. The research read only a few of the table's
// codes; these are the layout's own field-by-field rejection reasons.
const (
	InvalidBank        Reason = "01" // Código do banco inválido
	InvalidMovement    Reason = "05" // Código de movimento inválido
	InvalidOurNumber   Reason = "08" // Nosso número inválido
	DuplicateOurNumber Reason = "09" // Nosso número duplicado
	InvalidDueDate     Reason = "16" // Data de vencimento inválida
	PastDueDate        Reason = "17" // Data de vencimento anterior à de emissão
	InvalidAmount      Reason = "20" // Valor do título inválido
	InvalidPayerDoc    Reason = "46" // Tipo/número de inscrição do pagador inválidos
	InvalidPayerName   Reason = "47" // Nome do pagador não informado
)

// Pix QR Code reasons for hybrid boletos.
const (
	RegisteredWithPix    Reason = "P1" // Registrado com QR Code Pix
	RegisteredWithoutPix Reason = "P2" // Registrado sem QR Code Pix
	InvalidPixKey        Reason = "P3" // Chave Pix inválida
	PixKeyNotInDICT      Reason = "P4" // Chave Pix não cadastrada no DICT
)

// Document types of the payer and the company (tipo de inscrição).
const (
	CPF  = 1
	CNPJ = 2
)

// Operation is the batch header's operation: a remittance or a return.
const (
	Remittance = 'R'
	Return     = 'T'
)

// File codes in the file header.
const (
	CodeRemittance = 1
	CodeReturn     = 2
)
