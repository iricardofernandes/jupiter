package reconciliation

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/platform/page"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/reconciliation/db"
)

const (
	maxNote     = 500
	maxOperator = 100
)

// Resolution codes a break shows a merchant: how it ended, without who or why.
const (
	ResolvedMatched     = "matched"
	ResolvedSuperseded  = "superseded"
	ResolvedFixed       = "fixed_at_counterparty"
	ResolvedByOperator  = "resolved_by_operator"
	ResolvedByConfirmed = "confirmed"
)

// ResolutionCode is how a break's resolution reads to a merchant.
func ResolutionCode(resolution string) string {
	switch {
	case resolution == "":
		return ""
	case resolution == "matched":
		return ResolvedMatched
	case strings.HasPrefix(resolution, "superseded"):
		return ResolvedSuperseded
	case resolution == "fixed at the counterparty":
		return ResolvedFixed
	case strings.HasPrefix(resolution, "confirmed by"):
		return ResolvedByConfirmed
	}
	return ResolvedByOperator
}

// Break is what did not match.
type Break struct {
	ID           id.ID
	Livemode     bool
	Counterparty string
	Stream       string
	Kind         string
	// Key is what the movement is known by: an RRN, a nosso número, an end-to-end id, a
	// day; for a divergence, what it is about.
	Key        string
	Merchant   string
	Amount     int64
	ValueDate  time.Time
	Detail     string
	Score      int
	Reasons    []string
	Status     string
	Resolution string
	OpenedOn   time.Time
	ResolvedOn time.Time
}

// Age is how many days a break has been open, or was before it was resolved.
func (b Break) Age(today time.Time) int {
	end := today
	if !b.ResolvedOn.IsZero() {
		end = b.ResolvedOn
	}
	return int(end.Sub(b.OpenedOn).Hours() / 24)
}

func breakOf(row db.ReconciliationBreak) (Break, error) {
	breakID, err := BreakPrefix.Parse(row.ID)
	if err != nil {
		return Break{}, err
	}
	b := Break{
		ID: breakID, Livemode: row.Livemode, Counterparty: row.Counterparty, Stream: row.Stream, Kind: row.Kind, Key: row.Key, Merchant: row.MerchantID,
		Amount: row.Amount, ValueDate: row.ValueDate.Time, Detail: row.Detail, Score: int(row.Score), Status: row.Status,
		Resolution: row.Resolution, OpenedOn: row.OpenedOn.Time, ResolvedOn: row.ResolvedOn.Time,
	}
	return b, json.Unmarshal(row.Reasons, &b.Reasons)
}

// Breaks lists a mode's breaks, newest first: a merchant's when merchant is set, of a
// status when status is.
func (s *Service) Breaks(ctx context.Context, q db.DBTX, livemode bool, merchant, status string, r page.Request) ([]Break, bool, error) {
	rows, err := db.New(q).ListBreaks(ctx, db.ListBreaksParams{
		Livemode: livemode, MerchantID: merchant, Status: status,
		StartingAfter: r.StartingAfter, EndingBefore: r.EndingBefore, MaxCount: r.Fetch(),
	})
	if err != nil {
		return nil, false, err
	}
	out := make([]Break, 0, len(rows))
	for _, row := range rows {
		b, err := breakOf(row)
		if err != nil {
			return nil, false, err
		}
		out = append(out, b)
	}
	items, more := page.Trim(out, r)
	return items, more, nil
}

// Resolve closes a break by hand, saying why and who: a duplicate the counterparty
// withdrew, a mismatch settled outside. Its records are taken as they are, and open no
// break again.
func (s *Service) Resolve(ctx context.Context, breakID id.ID, note, operator string) error {
	if strings.TrimSpace(note) == "" || strings.TrimSpace(operator) == "" || len(note) > maxNote || len(operator) > maxOperator {
		return fmt.Errorf("%w: a break is resolved with a note of up to %d characters and the operator's name", ErrInvalid, maxNote)
	}
	return postgres.InTx(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		b, err := q.GetBreak(ctx, breakID.String())
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: no open break %s", ErrNotFound, breakID)
		}
		if err != nil {
			return err
		}
		if err := q.LockMode(ctx, "mode/"+strconv.FormatBool(b.Livemode)); err != nil {
			return err
		}
		n, err := q.ResolveBreak(ctx, db.ResolveBreakParams{
			ID: b.ID, Resolution: operator + ": " + note, Day: date(Day(s.cfg.Now())), Now: ts(s.cfg.Now()),
		})
		if err == nil && n == 0 {
			err = fmt.Errorf("%w: no open break %s", ErrNotFound, breakID)
		}
		if err != nil {
			return err
		}
		return q.SettleRecords(ctx, db.SettleRecordsParams{BreakID: b.ID, A: b.RecordID.Int64, B: b.OtherRecordID.Int64})
	})
}

// Confirm accepts a probable match: its two records are matched, by the operator.
func (s *Service) Confirm(ctx context.Context, breakID id.ID, operator string) error {
	if strings.TrimSpace(operator) == "" || len(operator) > maxOperator {
		return fmt.Errorf("%w: a match is confirmed with the operator's name", ErrInvalid)
	}
	return postgres.InTx(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		b, err := q.GetBreak(ctx, breakID.String())
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (b.Status != "open" || b.Kind != ProbableMatch || !b.OtherRecordID.Valid)) {
			return fmt.Errorf("%w: no open probable match %s", ErrNotFound, breakID)
		}
		if err != nil {
			return err
		}
		if err := q.LockMode(ctx, "mode/"+strconv.FormatBool(b.Livemode)); err != nil {
			return err
		}
		if err := pairable(ctx, q, b); err != nil {
			return err
		}
		today := date(Day(s.cfg.Now()))
		if n, err := q.Pair(ctx, db.PairParams{A: b.RecordID.Int64, B: b.OtherRecordID.Int64, Rule: "confirmed by " + operator, Day: today}); err != nil || n != 2 {
			return cmp.Or(err, fmt.Errorf("%w: break %s's records were matched meanwhile", ErrInvalid, breakID))
		}
		_, err = q.ResolveBreak(ctx, db.ResolveBreakParams{ID: b.ID, Resolution: "confirmed by " + operator, Day: today, Now: ts(s.cfg.Now())})
		return err
	})
}

// pairable checks that a break names two unmatched records of one stream, one a side.
func pairable(ctx context.Context, q *db.Queries, b db.ReconciliationBreak) error {
	a, err := q.GetRecord(ctx, b.RecordID.Int64)
	if err != nil {
		return err
	}
	other, err := q.GetRecord(ctx, b.OtherRecordID.Int64)
	if err != nil {
		return err
	}
	if a.Livemode != other.Livemode || a.Counterparty != other.Counterparty || a.Stream != other.Stream || a.Side == other.Side ||
		a.MatchedWith.Valid || other.MatchedWith.Valid || a.SettledBy != "" || other.SettledBy != "" {
		return fmt.Errorf("%w: break %s does not pair two unmatched records of one stream", ErrInvalid, b.ID)
	}
	return nil
}

// Report is a day's reconciliation: per counterparty and stream, what of Jupiter's
// records of the day matched, and the breaks open at its end or opened or resolved on it.
type Report struct {
	Day      time.Time
	Livemode bool
	Merchant string
	Streams  []StreamReport
	Breaks   []Break
}

type StreamReport struct {
	Counterparty string
	Stream       string
	Matched      int64
	Amount       int64
	Open         int
	OpenedOnDay  int
	ResolvedOn   int
	// Ageing counts the breaks open at the day's end by how old they are: up to a day, up
	// to a week, up to a month, older.
	Ageing [4]int
}

// Report reports a day of a mode: for all, or for one merchant's records and breaks.
func (s *Service) Report(ctx context.Context, q db.DBTX, livemode bool, day time.Time, merchant string) (Report, error) {
	day = date(day).Time
	queries := db.New(q)
	matched, err := queries.MatchedOn(ctx, db.MatchedOnParams{Livemode: livemode, Day: date(day), MerchantID: merchant})
	if err != nil {
		return Report{}, err
	}
	rows, err := queries.BreaksForReport(ctx, db.BreaksForReportParams{Livemode: livemode, Day: date(day), MerchantID: merchant})
	if err != nil {
		return Report{}, err
	}
	out := Report{Day: day, Livemode: livemode, Merchant: merchant}
	index := map[[2]string]int{}
	stream := func(counterparty, name string) *StreamReport {
		k := [2]string{counterparty, name}
		if i, ok := index[k]; ok {
			return &out.Streams[i]
		}
		index[k] = len(out.Streams)
		out.Streams = append(out.Streams, StreamReport{Counterparty: counterparty, Stream: name})
		return &out.Streams[len(out.Streams)-1]
	}
	for _, m := range matched {
		sr := stream(m.Counterparty, m.Stream)
		sr.Matched, sr.Amount = m.Records, m.Amount
	}
	for _, row := range rows {
		b, err := breakOf(row)
		if err != nil {
			return Report{}, err
		}
		sr := stream(b.Counterparty, b.Stream)
		if b.OpenedOn.Equal(day) {
			sr.OpenedOnDay++
		}
		if b.ResolvedOn.Equal(day) {
			sr.ResolvedOn++
		}
		if b.Status == "open" || b.ResolvedOn.After(day) {
			sr.Open++
			sr.Ageing[bucket(b.Age(day))]++
			out.Breaks = append(out.Breaks, b)
		}
	}
	return out, nil
}

func bucket(age int) int {
	switch {
	case age <= 1:
		return 0
	case age <= 7:
		return 1
	case age <= 30:
		return 2
	}
	return 3
}

// RunDue reconciles each mode through yesterday, in Brasília: the counterparties have
// closed it. Running again the same day reads anything new.
func (s *Service) RunDue(ctx context.Context) ([]Run, error) {
	yesterday := Day(s.cfg.Now()).AddDate(0, 0, -1)
	var runs []Run
	var failures []error
	for _, livemode := range []bool{false, true} {
		if len(s.mode(livemode).Streams) == 0 && len(s.mode(livemode).Divergences) == 0 {
			continue
		}
		run, err := s.Reconcile(ctx, livemode, yesterday)
		runs = append(runs, run)
		failures = append(failures, err)
	}
	return runs, errors.Join(failures...)
}
