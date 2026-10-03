package reconciliation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/reconciliation/db"
)

const (
	// firstDays is how far back a mode's first reconciliation reads the counterparties.
	firstDays = 7
	// oursLookback is how far before the first day read Jupiter's records are taken: what
	// a counterparty may still list late.
	oursLookback = 31
	// maxDays is the most days one reconciliation reads; one far behind catches up over
	// several.
	maxDays = 31
	// rereadDays are the days before the last reconciled that each run reads again: what a
	// counterparty adds to a day it had closed, or a file imported late.
	rereadDays = 3
	// maxUnmatched is the most unmatched records of a stream matched in one run.
	maxUnmatched = 50_000
	// maxRead is the most records one side of a stream gives in one read, and maxField the
	// longest identity, key or reference kept: what a counterparty can make Jupiter hold.
	maxRead  = 200_000
	maxField = 200
)

// Run says what one reconciliation did.
type Run struct {
	Through  time.Time
	Matched  int
	Opened   int
	Resolved int
}

// Reconcile reconciles a mode through a day: it reads each counterparty's records of the
// days since the last reconciliation and Jupiter's, matches every stream, opens breaks for
// what is due by the day and unmatched, and resolves those that match now. Reconciling a
// day again reads it again and changes only what changed. A stream that cannot be read is
// reported and left for the next run; the others go on. A mode far behind reconciles a
// month at a time.
func (s *Service) Reconcile(ctx context.Context, livemode bool, through time.Time) (Run, error) {
	through = date(through).Time
	last, err := db.New(s.cfg.Pool).LastRun(ctx, livemode)
	if err != nil {
		return Run{}, err
	}
	from, oursSince := through.AddDate(0, 0, -firstDays), through.AddDate(0, 0, -firstDays)
	if last.Valid && last.Time.Year() > 1 {
		from = last.Time.AddDate(0, 0, 1-rereadDays)
		oursSince = from.AddDate(0, 0, -oursLookback)
	}
	if from.After(through) {
		from = through
	}
	if limit := from.AddDate(0, 0, maxDays-1); through.After(limit) {
		through = limit
	}
	mode := s.mode(livemode)
	records, unreadable, failed, failures := s.read(ctx, livemode, mode.Streams, oursSince, from, through)
	var divergences map[string][]Divergence
	divergences, failures = s.divergences(ctx, livemode, mode, failures)
	var run Run
	err = postgres.InTx(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
		run = Run{Through: through}
		if err := s.apply(ctx, db.New(tx), livemode, mode, records, failed, divergences, len(failures) == 0, &run); err != nil {
			return err
		}
		return s.openUnreadable(ctx, db.New(tx), livemode, unreadable, &run)
	})
	return run, errors.Join(append(failures, err)...)
}

const maxUnreadable = 100

// unreadableRecord is a record that cannot be matched, and why.
type unreadableRecord struct {
	Record
	why string
}

// openUnreadable opens a break for each record that cannot be matched, once: it is kept
// for a person, and does not stop the reconciliation from going on, as a run left
// unrecorded would.
func (s *Service) openUnreadable(ctx context.Context, q *db.Queries, livemode bool, records []unreadableRecord, run *Run) error {
	// A file of nothing but such records opens so many breaks a run; the days are read
	// again by the next runs, which open the rest.
	opened := 0
	for _, r := range records {
		if opened == maxUnreadable {
			break
		}
		sum := sha256.Sum256(fmt.Appendf(nil, "%s|%s|%s|%s|%d|%s|%s", r.Side, r.Identity, r.Key, r.Direction, r.Amount, r.Date.Format(time.DateOnly), r.Reference))
		subject := string(r.Side) + "/" + hex.EncodeToString(sum[:8])
		exists, err := q.BreakOfSubjectExists(ctx, db.BreakOfSubjectExistsParams{
			Livemode: livemode, Counterparty: r.Counterparty, Stream: r.Stream, Kind: KindUnreadable, Subject: subject,
		})
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		run.Opened++
		opened++
		err = q.InsertBreak(ctx, db.InsertBreakParams{
			ID: BreakPrefix.New().String(), Livemode: livemode, Counterparty: r.Counterparty, Stream: r.Stream, Kind: KindUnreadable,
			Key: truncate(r.Key, maxField), Subject: subject, Detail: fmt.Sprintf("a %s record that cannot be matched: %s; identity %.40q", r.Side, r.why, r.Identity),
			MerchantID: r.Merchant, Amount: r.Amount, ValueDate: date(r.Date), Reasons: []byte("[]"),
			OpenedOn: date(run.Through), Now: ts(s.cfg.Now()),
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// apply records what was read and matches it, one mode at a time: the lock serializes
// the writes, and reading again what another run read changes nothing. A stream that
// could not be read whole is not matched, so its missing side opens no break; and the run
// is recorded only when every stream was read, so the next one reads those days again.
func (s *Service) apply(ctx context.Context, q *db.Queries, livemode bool, mode Mode, records []Record, failed map[string]bool,
	divergences map[string][]Divergence, complete bool, run *Run,
) error {
	if err := q.LockMode(ctx, "mode/"+strconv.FormatBool(livemode)); err != nil {
		return err
	}
	for _, r := range records {
		if err := s.insert(ctx, q, livemode, r); err != nil {
			return err
		}
	}
	for _, st := range mode.Streams {
		if failed[st.Counterparty+"/"+st.Name] {
			continue
		}
		if err := s.matchStream(ctx, q, livemode, st, run.Through, run); err != nil {
			return err
		}
	}
	if err := s.resolveMatched(ctx, q, livemode, run.Through, run); err != nil {
		return err
	}
	for counterparty, list := range divergences {
		if err := s.mirror(ctx, q, livemode, counterparty, list, run.Through, run); err != nil {
			return err
		}
	}
	if !complete {
		return nil
	}
	return q.InsertRun(ctx, db.InsertRunParams{
		Livemode: livemode, Day: date(run.Through), RanAt: ts(s.cfg.Now()), Matched: int32(run.Matched), //nolint:gosec // counts
		Opened: int32(run.Opened), Resolved: int32(run.Resolved), //nolint:gosec // counts
	})
}

// read gathers both sides of every stream, and which streams could not be read whole.
// A record that cannot be matched is answered apart.
func (s *Service) read(ctx context.Context, livemode bool, streams []Stream, oursSince, from, through time.Time) ([]Record, []unreadableRecord, map[string]bool, []error) {
	var out []Record
	var unreadable []unreadableRecord
	failed := map[string]bool{}
	var failures []error
	fail := func(st Stream, err error) {
		failed[st.Counterparty+"/"+st.Name] = true
		failures = append(failures, err)
	}
	for _, st := range streams {
		for _, ours := range st.Ours {
			got, err := ours(ctx, s.cfg.Pool, livemode, oursSince)
			if err == nil && len(got) > maxRead {
				err = fmt.Errorf("%d records, more than %d", len(got), maxRead)
			}
			if err != nil {
				fail(st, fmt.Errorf("%s %s, Jupiter's side: %w", st.Counterparty, st.Name, err))
				continue
			}
			ok, bad := valid(stamped(got, st, Ours))
			out, unreadable = append(out, ok...), append(unreadable, bad...)
		}
		for day := from; !day.After(through); day = day.AddDate(0, 0, 1) {
			got, err := st.Theirs(ctx, s.cfg.Pool, livemode, day)
			if err == nil && len(got) > maxRead {
				err = fmt.Errorf("%d records, more than %d", len(got), maxRead)
			}
			if err != nil {
				fail(st, fmt.Errorf("%s %s of %s: %w", st.Counterparty, st.Name, day.Format(time.DateOnly), err))
				break
			}
			ok, bad := valid(stamped(got, st, Theirs))
			out, unreadable = append(out, ok...), append(unreadable, bad...)
		}
	}
	return out, unreadable, failed, failures
}

// valid splits the records that can be matched, with an identity, a key and a direction,
// for a positive amount, from those that cannot; references are cut short.
func valid(records []Record) ([]Record, []unreadableRecord) {
	var ok []Record
	var bad []unreadableRecord
	for _, r := range records {
		why := ""
		switch {
		case r.Amount <= 0:
			why = "no positive amount"
		case r.Identity == "" || len(r.Identity) > maxField:
			why = "no identity of up to " + strconv.Itoa(maxField) + " characters"
		case r.Key == "" || len(r.Key) > maxField:
			why = "no key of up to " + strconv.Itoa(maxField) + " characters"
		case r.Direction != In && r.Direction != Out:
			why = "no direction"
		}
		if why != "" {
			bad = append(bad, unreadableRecord{Record: r, why: why})
			continue
		}
		r.Reference = truncate(r.Reference, maxField)
		ok = append(ok, r)
	}
	return ok, bad
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func stamped(records []Record, st Stream, side Side) []Record {
	for i := range records {
		records[i].Counterparty, records[i].Stream, records[i].Side = st.Counterparty, st.Name, side
	}
	return records
}

func (s *Service) divergences(ctx context.Context, livemode bool, mode Mode, failures []error) (map[string][]Divergence, []error) {
	out := map[string][]Divergence{}
	for counterparty, f := range mode.Divergences {
		list, err := f(ctx, s.cfg.Pool, livemode)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s divergences: %w", counterparty, err))
			continue
		}
		out[counterparty] = list
	}
	return out, failures
}

func (s *Service) insert(ctx context.Context, q *db.Queries, livemode bool, r Record) error {
	_, err := q.InsertRecord(ctx, db.InsertRecordParams{
		Livemode: livemode, Counterparty: r.Counterparty, Stream: r.Stream, Side: string(r.Side), Identity: r.Identity, Key: r.Key,
		Direction: string(r.Direction), Amount: r.Amount, ValueDate: date(r.Date), MerchantID: r.Merchant, Reference: r.Reference,
		Now: ts(s.cfg.Now()),
	})
	return err
}

// matchStream matches a stream's unmatched records and records what it found.
func (s *Service) matchStream(ctx context.Context, q *db.Queries, livemode bool, st Stream, through time.Time, run *Run) error {
	rows, err := q.UnmatchedRecords(ctx, db.UnmatchedRecordsParams{Livemode: livemode, Counterparty: st.Counterparty, Stream: st.Name, MaxCount: maxUnmatched + 1})
	if err != nil || len(rows) == 0 {
		return err
	}
	if len(rows) > maxUnmatched {
		s.cfg.Logger.WarnContext(ctx, "reconciliation: more unmatched records than one run matches; the oldest first",
			"counterparty", st.Counterparty, "stream", st.Name, "limit", maxUnmatched)
		rows = rows[:maxUnmatched]
	}
	var ours, theirs []Record
	keys := map[string]bool{}
	for _, row := range rows {
		r := recordOf(row)
		keys[r.Key] = true
		if r.Side == Ours {
			ours = append(ours, r)
		} else {
			theirs = append(theirs, r)
		}
	}
	matchedRows, err := q.MatchedWithKeys(ctx, db.MatchedWithKeysParams{Livemode: livemode, Counterparty: st.Counterparty, Stream: st.Name, Keys: setOf(keys)})
	if err != nil {
		return err
	}
	matched := make([]Record, len(matchedRows))
	for i, row := range matchedRows {
		matched[i] = recordOf(row)
	}
	out := Match(through, ours, theirs, matched)
	for _, p := range out.Pairs {
		if _, err := q.Pair(ctx, db.PairParams{A: p.Ours, B: p.Theirs, Rule: RuleExact, Day: date(through)}); err != nil {
			return err
		}
		run.Matched++
	}
	for _, f := range out.Findings {
		if err := s.record(ctx, q, livemode, f, through, run); err != nil {
			return err
		}
	}
	return nil
}

func setOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// record keeps a finding as its record's open break: a new one, or the one it had, of
// what it is now.
func (s *Service) record(ctx context.Context, q *db.Queries, livemode bool, f Finding, through time.Time, run *Run) error {
	reasons, err := json.Marshal(f.Reasons)
	if err != nil {
		return err
	}
	other := pgtype.Int8{}
	if f.Other != nil {
		other = pgtype.Int8{Int64: f.Other.ID, Valid: true}
		if err := s.supersede(ctx, q, f.Other.ID, f.Kind, through); err != nil {
			return err
		}
	}
	existing, err := q.OpenBreakOf(ctx, pgtype.Int8{Int64: f.Record.ID, Valid: true})
	switch {
	case err == nil:
		return q.UpdateBreak(ctx, db.UpdateBreakParams{
			ID: existing.ID, Kind: f.Kind, OtherRecordID: other, Detail: detailOf(f), Score: int32(f.Score), Reasons: reasons, Now: ts(s.cfg.Now()), //nolint:gosec // out of 100
		})
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	merchant := f.Record.Merchant
	if merchant == "" && f.Other != nil {
		merchant = f.Other.Merchant
	}
	if merchant == "" {
		if merchant, err = q.MerchantOfKey(ctx, db.MerchantOfKeyParams{
			Livemode: livemode, Counterparty: f.Record.Counterparty, Stream: f.Record.Stream, Key: f.Record.Key,
		}); err != nil {
			return err
		}
	}
	run.Opened++
	return q.InsertBreak(ctx, db.InsertBreakParams{
		ID: BreakPrefix.New().String(), Livemode: livemode, Counterparty: f.Record.Counterparty, Stream: f.Record.Stream, Kind: f.Kind,
		RecordID: pgtype.Int8{Int64: f.Record.ID, Valid: true}, OtherRecordID: other, Key: f.Record.Key, Detail: detailOf(f), MerchantID: merchant,
		Amount: f.Record.Amount, ValueDate: date(f.Record.Date), Score: int32(f.Score), Reasons: reasons, //nolint:gosec // out of 100
		OpenedOn: date(through), Now: ts(s.cfg.Now()),
	})
}

// supersede resolves a record's own open break: the record is in another break now.
func (s *Service) supersede(ctx context.Context, q *db.Queries, recordID int64, kind string, through time.Time) error {
	b, err := q.OpenBreakOf(ctx, pgtype.Int8{Int64: recordID, Valid: true})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = q.ResolveBreak(ctx, db.ResolveBreakParams{ID: b.ID, Resolution: "superseded by " + kind, Day: date(through), Now: ts(s.cfg.Now())})
	return err
}

func detailOf(f Finding) string {
	d := fmt.Sprintf("%s %s of %d, key %s, on %s", f.Record.Side, f.Record.Direction, f.Record.Amount, f.Record.Key, f.Record.Date.Format(time.DateOnly))
	if f.Other != nil {
		d += fmt.Sprintf("; the other side has %d, key %s, on %s", f.Other.Amount, f.Other.Key, f.Other.Date.Format(time.DateOnly))
	}
	return d
}

// resolveMatched resolves the open breaks whose records are matched now: what was late
// arrived.
func (s *Service) resolveMatched(ctx context.Context, q *db.Queries, livemode bool, through time.Time, run *Run) error {
	ids, err := q.OpenBreaksOfMatched(ctx, livemode)
	if err != nil {
		return err
	}
	for _, breakID := range ids {
		n, err := q.ResolveBreak(ctx, db.ResolveBreakParams{ID: breakID, Resolution: "matched", Day: date(through), Now: ts(s.cfg.Now())})
		if err != nil {
			return err
		}
		run.Resolved += int(n)
	}
	return nil
}

// mirror keeps a counterparty's divergences as breaks: one opened for each new one, and
// those no longer listed resolved.
func (s *Service) mirror(ctx context.Context, q *db.Queries, livemode bool, counterparty string, list []Divergence, through time.Time, run *Run) error {
	open, err := q.OpenSubjectBreaks(ctx, db.OpenSubjectBreaksParams{Livemode: livemode, Counterparty: counterparty})
	if err != nil {
		return err
	}
	have := map[string]string{}
	for _, b := range open {
		have[b.Subject] = b.ID
	}
	still := map[string]bool{}
	for _, d := range list {
		still[d.Subject] = true
		if have[d.Subject] != "" {
			continue
		}
		run.Opened++
		if err := q.InsertBreak(ctx, db.InsertBreakParams{
			ID: BreakPrefix.New().String(), Livemode: livemode, Counterparty: counterparty, Stream: "positions", Kind: KindDivergence,
			Key: d.Subject, Subject: d.Subject, Detail: d.Detail, MerchantID: d.Merchant, ValueDate: date(d.FoundOn), Reasons: []byte("[]"),
			OpenedOn: date(Day(d.FoundOn)), Now: ts(s.cfg.Now()),
		}); err != nil {
			return err
		}
	}
	for subject, breakID := range have {
		if still[subject] {
			continue
		}
		n, err := q.ResolveBreak(ctx, db.ResolveBreakParams{ID: breakID, Resolution: "fixed at the counterparty", Day: date(through), Now: ts(s.cfg.Now())})
		if err != nil {
			return err
		}
		run.Resolved += int(n)
	}
	return nil
}

func recordOf(row db.ReconciliationRecord) Record {
	return Record{
		ID: row.ID, Counterparty: row.Counterparty, Stream: row.Stream, Side: Side(row.Side), Identity: row.Identity, Key: row.Key,
		Direction: Direction(row.Direction), Amount: row.Amount, Date: row.ValueDate.Time, Merchant: row.MerchantID, Reference: row.Reference,
	}
}
