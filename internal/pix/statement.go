package pix

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/reconciliation"
	"github.com/iricardofernandes/jupiter/pkg/pixapi"
)

// Statement is the bank's side of Jupiter's account at it: the day's SPI statement, a
// record a line, keyed as Jupiter keys its movements (pkg/pixapi/statement.go).
func (c *Connector) Statement(ctx context.Context, _ *pgxpool.Pool, _ bool, day time.Time) ([]reconciliation.Record, error) {
	var out pixapi.Statement
	if err := c.call(ctx, "GET", "/extrato?data="+url.QueryEscape(day.Format(time.DateOnly)), nil, &out); err != nil {
		return nil, err
	}
	records := make([]reconciliation.Record, 0, len(out.Lancamentos))
	for _, l := range out.Lancamentos {
		amount, err := pixapi.ParseValor(l.Valor)
		lineDay, dayErr := time.Parse(time.DateOnly, l.Data)
		if err != nil || dayErr != nil || l.ID == "" || (l.Tipo != pixapi.StatementCredit && l.Tipo != pixapi.StatementDebit) {
			return nil, fmt.Errorf("pix: a statement line that cannot be read: %q", l.ID)
		}
		direction := reconciliation.In
		if l.Tipo == pixapi.StatementDebit {
			direction = reconciliation.Out
		}
		records = append(records, reconciliation.Record{
			Identity: l.ID, Key: l.Referencia, Direction: direction, Amount: amount, Date: lineDay, Reference: l.Descricao,
		})
	}
	return records, nil
}
