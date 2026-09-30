package pix

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/pkg/pixapi"
)

// brasilia is the time zone of due dates.
var brasilia = time.FixedZone("BRT", -3*60*60)

func chargePath(txid string, due bool) string {
	if due {
		return "/cobv/" + url.PathEscape(txid)
	}
	return "/cob/" + url.PathEscape(txid)
}

// CreateCharge creates an immediate charge (PUT /cob/{txid}) or one with a due date
// (PUT /cobv/{txid}). A txid is never used twice, so a charge the bank refuses to create
// again is read back: the first request did create it.
func (c *Connector) CreateCharge(ctx context.Context, r payments.PixChargeRequest) (payments.PixCharge, error) {
	var body any
	if r.Due != nil {
		body = c.cobv(r)
	} else {
		var cob pixapi.CobSolicitada
		cob.Chave, cob.Valor.Original = c.cfg.Key, pixapi.FormatValor(r.Amount.Minor())
		cob.Calendario.Expiracao = pixapi.Ptr(int32(r.ExpiresAfter.Seconds()))
		if r.Description != "" {
			cob.SolicitacaoPagador = pixapi.Ptr(r.Description)
		}
		body = cob
	}
	charge, err := c.putCharge(ctx, r.TxID, r.Due != nil, body)
	if errors.Is(err, payments.ErrPixRefused) {
		if existing, getErr := c.Charge(ctx, r.TxID, r.Due != nil); getErr == nil {
			return existing, nil
		}
	}
	return charge, err
}

func (c *Connector) cobv(r payments.PixChargeRequest) pixapi.CobVSolicitada {
	d := r.Due
	var cob pixapi.CobVSolicitada
	cob.Chave, cob.Valor.Original = c.cfg.Key, pixapi.FormatValor(r.Amount.Minor())
	_ = cob.Calendario.DataDeVencimento.UnmarshalText([]byte(d.Date))
	cob.Calendario.ValidadeAposVencimento = pixapi.Ptr(d.DaysAfter)
	cob.Devedor.Nome = pixapi.Ptr(d.PayerName)
	if len(d.PayerTaxID) == 11 {
		cob.Devedor.Cpf = pixapi.Ptr(d.PayerTaxID)
	} else {
		cob.Devedor.Cnpj = pixapi.Ptr(d.PayerTaxID)
	}
	if r.Description != "" {
		cob.SolicitacaoPagador = pixapi.Ptr(r.Description)
	}
	switch {
	case d.FineAmount > 0:
		fine := pixapi.New(&cob.Valor.Multa)
		fine.Modalidade, fine.ValorPerc = 1, pixapi.FormatValor(d.FineAmount)
	case d.FinePercent > 0:
		fine := pixapi.New(&cob.Valor.Multa)
		fine.Modalidade, fine.ValorPerc = 2, pixapi.FormatValor(d.FinePercent)
	}
	if d.InterestMonthlyPercent > 0 {
		interest := pixapi.New(&cob.Valor.Juros)
		interest.Modalidade, interest.ValorPerc = 3, pixapi.FormatValor(d.InterestMonthlyPercent) // percent a month, calendar days
	}
	if d.DiscountAmount > 0 {
		discount := pixapi.New(&cob.Valor.Desconto)
		discount.Modalidade = 1 // a fixed amount until a date
		item := pixapi.Append(&discount.DescontoDataFixa)
		_ = item.Data.UnmarshalText([]byte(d.DiscountUntil))
		item.ValorPerc = pixapi.FormatValor(d.DiscountAmount)
	}
	return cob
}

func (c *Connector) putCharge(ctx context.Context, txid string, due bool, body any) (payments.PixCharge, error) {
	if due {
		var out pixapi.CobVCompleta
		if err := c.call(ctx, "PUT", chargePath(txid, true), body, &out); err != nil {
			return payments.PixCharge{}, err
		}
		return chargeOfCobV(out)
	}
	var out pixapi.CobCompleta
	if err := c.call(ctx, "PUT", chargePath(txid, false), body, &out); err != nil {
		return payments.PixCharge{}, err
	}
	return chargeOfCob(out)
}

func (c *Connector) Charge(ctx context.Context, txid string, due bool) (payments.PixCharge, error) {
	if due {
		var out pixapi.CobVCompleta
		if err := c.call(ctx, "GET", chargePath(txid, true), nil, &out); err != nil {
			return payments.PixCharge{}, err
		}
		return chargeOfCobV(out)
	}
	var out pixapi.CobCompleta
	if err := c.call(ctx, "GET", chargePath(txid, false), nil, &out); err != nil {
		return payments.PixCharge{}, err
	}
	return chargeOfCob(out)
}

// RemoveCharge removes a charge (PATCH with status REMOVIDA_PELO_USUARIO_RECEBEDOR). The
// bank refuses to remove one that is no longer active, which is then read as it is.
func (c *Connector) RemoveCharge(ctx context.Context, txid string, due bool) (payments.PixCharge, error) {
	body := map[string]string{"status": string(pixapi.CobStatusREMOVIDAPELOUSUARIORECEBEDOR)}
	var err error
	var charge payments.PixCharge
	if due {
		var out pixapi.CobVCompleta
		if err = c.call(ctx, "PATCH", chargePath(txid, true), body, &out); err == nil {
			return chargeOfCobV(out)
		}
	} else {
		var out pixapi.CobCompleta
		if err = c.call(ctx, "PATCH", chargePath(txid, false), body, &out); err == nil {
			return chargeOfCob(out)
		}
	}
	if errors.Is(err, payments.ErrPixRefused) {
		return c.Charge(ctx, txid, due)
	}
	return charge, err
}

func chargeOfCob(out pixapi.CobCompleta) (payments.PixCharge, error) {
	if out.Txid == nil || out.Calendario == nil {
		return payments.PixCharge{}, errors.New("pix: a charge without its txid or calendar")
	}
	charge := payments.PixCharge{
		TxID: *out.Txid, Status: string(out.Status),
		ExpiresAt: out.Calendario.Criacao.Add(time.Duration(out.Calendario.Expiracao) * time.Second),
	}
	if out.PixCopiaECola != nil {
		charge.CopyPaste = *out.PixCopiaECola
	}
	if out.Pix != nil {
		for _, p := range *out.Pix {
			charge.Payments = append(charge.Payments, p.EndToEndId)
		}
	}
	return charge, nil
}

func chargeOfCobV(out pixapi.CobVCompleta) (payments.PixCharge, error) {
	if out.Txid == nil || out.Calendario == nil {
		return payments.PixCharge{}, errors.New("pix: a charge without its txid or calendar")
	}
	due, err := time.ParseInLocation(time.DateOnly, out.Calendario.DataDeVencimento.String(), brasilia)
	if err != nil {
		return payments.PixCharge{}, fmt.Errorf("pix: due date: %w", err)
	}
	charge := payments.PixCharge{
		TxID: *out.Txid, Status: string(out.Status),
		ExpiresAt: due.AddDate(0, 0, int(out.Calendario.ValidadeAposVencimento)+1),
	}
	if out.PixCopiaECola != nil {
		charge.CopyPaste = *out.PixCopiaECola
	}
	if out.Pix != nil {
		for _, p := range *out.Pix {
			charge.Payments = append(charge.Payments, p.EndToEndId)
		}
	}
	return charge, nil
}

// Payment reads a Pix received (GET /pix/{e2eid}), with its returns.
func (c *Connector) Payment(ctx context.Context, e2eID string) (payments.PixPayment, error) {
	if !pixapi.ValidEndToEndID(e2eID) {
		return payments.PixPayment{}, fmt.Errorf("%w: %q is not an endToEndId", payments.ErrPixNotFound, e2eID)
	}
	var out pixapi.Pix
	if err := c.call(ctx, "GET", "/pix/"+e2eID, nil, &out); err != nil {
		return payments.PixPayment{}, err
	}
	return paymentOf(out)
}

func paymentOf(p pixapi.Pix) (payments.PixPayment, error) {
	amount, err := brl(p.Valor)
	if err != nil {
		return payments.PixPayment{}, err
	}
	out := payments.PixPayment{E2EID: p.EndToEndId, Amount: amount, ReceivedAt: p.Horario}
	if p.Txid != nil {
		out.TxID = *p.Txid
	}
	if p.Devolucoes != nil {
		for _, d := range *p.Devolucoes {
			r, err := returnOf(d)
			if err != nil {
				return payments.PixPayment{}, err
			}
			out.Returns = append(out.Returns, r)
		}
	}
	return out, nil
}

func returnOf(d pixapi.Devolucao) (payments.PixReturn, error) {
	amount, err := brl(d.Valor)
	if err != nil {
		return payments.PixReturn{}, err
	}
	r := payments.PixReturn{ID: d.Id, Amount: amount, Status: payments.PixReturnProcessing}
	switch d.Status {
	case pixapi.DEVOLVIDO:
		r.Status = payments.PixReturnReturned
	case pixapi.NAOREALIZADO:
		r.Status = payments.PixReturnFailed
	case pixapi.EMPROCESSAMENTO:
	}
	if d.Motivo != nil {
		r.Reason = *d.Motivo
	}
	return r, nil
}

func brl(valor string) (money.Amount, error) {
	minor, err := pixapi.ParseValor(valor)
	if err != nil {
		return money.Amount{}, err
	}
	return money.New(minor, money.BRL)
}

func returnPath(e2eID, id string) string {
	return "/pix/" + url.PathEscape(e2eID) + "/devolucao/" + url.PathEscape(id)
}

// Return asks for a return (PUT /pix/{e2eid}/devolucao/{id}), of nature ORIGINAL.
func (c *Connector) Return(ctx context.Context, r payments.PixReturnRequest) (payments.PixReturn, error) {
	body := pixapi.DevolucaoSolicitada{Valor: pixapi.FormatValor(r.Amount.Minor()), Natureza: pixapi.Ptr(pixapi.DevolucaoSolicitadaNaturezaORIGINAL)}
	var out pixapi.Devolucao
	err := c.call(ctx, "PUT", returnPath(r.E2EID, r.ID), body, &out)
	switch {
	case errors.Is(err, payments.ErrPixNotFound):
		// The Pix itself is unknown: asking again will not change that.
		return payments.PixReturn{}, fmt.Errorf("%w: %w", payments.ErrPixRefused, err)
	case errors.Is(err, payments.ErrPixRefused):
		// A repeat may be refused because the first was made: only a return the bank
		// does not have is refused.
		if existing, statusErr := c.ReturnStatus(ctx, r.E2EID, r.ID); !errors.Is(statusErr, payments.ErrPixNotFound) {
			return existing, statusErr
		}
		return payments.PixReturn{}, err
	case err != nil:
		return payments.PixReturn{}, err
	}
	return returnOf(out)
}

func (c *Connector) ReturnStatus(ctx context.Context, e2eID, id string) (payments.PixReturn, error) {
	var out pixapi.Devolucao
	if err := c.call(ctx, "GET", returnPath(e2eID, id), nil, &out); err != nil {
		return payments.PixReturn{}, err
	}
	return returnOf(out)
}

// transferBody and transferAnswer are the bank's transfer interface, which the API Pix
// does not cover (see the simulator's README).
type transferBody struct {
	Valor     string `json:"valor"`
	Chave     string `json:"chave"`
	Descricao string `json:"descricao,omitempty"`
}

type transferAnswer struct {
	IDEnvio    string     `json:"idEnvio"`
	EndToEndID string     `json:"endToEndId"`
	Status     string     `json:"status"`
	Motivo     string     `json:"motivo"`
	Liquidacao *time.Time `json:"liquidacao"`
	Favorecido *struct {
		Nome string `json:"nome"`
	} `json:"favorecido"`
}

func (a transferAnswer) transfer() payments.PixTransfer {
	t := payments.PixTransfer{ID: a.IDEnvio, E2EID: a.EndToEndID, Reason: a.Motivo, Status: payments.PixTransferProcessing}
	switch a.Status {
	case "REALIZADO":
		t.Status = payments.PixTransferPaid
	case "NAO_REALIZADO":
		t.Status = payments.PixTransferFailed
	}
	if a.Liquidacao != nil {
		t.SettledAt = *a.Liquidacao
	}
	if a.Favorecido != nil {
		t.Recipient = a.Favorecido.Nome
	}
	return t
}

func (c *Connector) Transfer(ctx context.Context, r payments.PixTransferRequest) (payments.PixTransfer, error) {
	var out transferAnswer
	body := transferBody{Valor: pixapi.FormatValor(r.Amount.Minor()), Chave: r.Key, Descricao: r.Description}
	err := c.call(ctx, "PUT", "/transferencias/"+url.PathEscape(r.ID), body, &out)
	if errors.Is(err, payments.ErrPixRefused) {
		// A repeat may be refused because the first was made, and the money may have
		// left: only a transfer the bank does not have is refused.
		if existing, statusErr := c.TransferStatus(ctx, r.ID); !errors.Is(statusErr, payments.ErrPixNotFound) {
			return existing, statusErr
		}
		return payments.PixTransfer{}, err
	}
	if err != nil {
		return payments.PixTransfer{}, err
	}
	return out.transfer(), nil
}

func (c *Connector) TransferStatus(ctx context.Context, id string) (payments.PixTransfer, error) {
	var out transferAnswer
	if err := c.call(ctx, "GET", "/transferencias/"+url.PathEscape(id), nil, &out); err != nil {
		return payments.PixTransfer{}, err
	}
	return out.transfer(), nil
}
