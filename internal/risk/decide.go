package risk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/expr-lang/expr"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/iricardofernandes/jupiter/internal/risk/db"
)

// Input describes a payment attempt about to reach a rail.
type Input struct {
	Owner        Owner
	Attempt      string
	Intent       string
	Amount       int64
	Currency     string
	Brand        string
	BIN          string
	IP           string
	OffSession   bool
	Installments int
	// CardFingerprint identifies the card across every merchant, for velocity;
	// MerchantFingerprint is the one the merchant sees and lists.
	CardFingerprint     string
	MerchantFingerprint string
}

// Fired is a rule that matched, as the decision log shows it.
type Fired struct {
	ID          string `json:"id"`
	Action      Action `json:"action"`
	Description string `json:"description"`
	Expression  string `json:"expression,omitempty"`
}

type Decision struct {
	ID       string
	Attempt  string
	Intent   string
	Action   Action
	Rules    []Fired
	Features Features
	Created  time.Time
}

// Decide decides on an attempt, in the transaction that records the attempt, and
// records the decision and the attempt for the velocity counters.
func (s *Service) Decide(ctx context.Context, tx pgx.Tx, in Input) (Decision, error) {
	q := db.New(tx)
	now := s.cfg.Now().UTC()
	f, throttled, err := s.serialized(ctx, q, in, now)
	if err != nil {
		return Decision{}, err
	}
	d := Decision{ID: DecisionPrefix.New().String(), Attempt: in.Attempt, Intent: in.Intent, Action: Allow, Created: now}
	listed, err := s.lists(ctx, q, in)
	if err != nil {
		return Decision{}, err
	}
	platform, own, err := s.rules(ctx, q, in.Owner, f)
	if err != nil {
		return Decision{}, err
	}
	throttle, err := s.cardTesting(ctx, q, in.Owner, &f, throttled, now)
	if err != nil {
		return Decision{}, err
	}
	d.Rules = applyLists(listed, platform, own, throttle)
	for _, r := range d.Rules {
		if severity[r.Action] > severity[d.Action] {
			d.Action = r.Action
		}
	}
	d.Features = f
	return d, s.record(ctx, q, in, d)
}

// serialized takes the locks a decision needs and reads its features under them. One
// decision at a time per card, and per address at a merchant, so their velocity counts
// see every attempt before them. A merchant's decisions are serialized too while its
// declines run at the card-testing ratio or it is throttled, so the throttle starts on the
// attempt that crosses it and its rate counts every attempt; ordinary traffic decides in
// parallel, its merchant counts off by at most the attempts in flight.
func (s *Service) serialized(ctx context.Context, q *db.Queries, in Input, now time.Time) (Features, bool, error) {
	mode := "/" + strconv.FormatBool(in.Owner.Livemode)
	if err := q.LockMerchant(ctx, "card/"+in.CardFingerprint+mode); err != nil {
		return Features{}, false, err
	}
	if in.IP != "" {
		if err := q.LockMerchant(ctx, "ip/"+in.Owner.Merchant.String()+"/"+in.IP+mode); err != nil {
			return Features{}, false, err
		}
	}
	f, err := s.features(ctx, q, in, now)
	if err != nil {
		return Features{}, false, err
	}
	throttled, err := s.throttled(ctx, q, in.Owner, now)
	if err != nil || (!throttled && f.MerchantDeclineRatio10m < s.cfg.CardTesting.Ratio) {
		return f, throttled, err
	}
	if err := q.LockMerchant(ctx, "merchant/"+in.Owner.Merchant.String()+mode); err != nil {
		return Features{}, false, err
	}
	if f, err = s.features(ctx, q, in, now); err != nil {
		return Features{}, false, err
	}
	throttled, err = s.throttled(ctx, q, in.Owner, now)
	return f, throttled, err
}

func (s *Service) throttled(ctx context.Context, q *db.Queries, owner Owner, now time.Time) (bool, error) {
	_, err := q.ActiveThrottle(ctx, db.ActiveThrottleParams{MerchantID: owner.Merchant.String(), Livemode: owner.Livemode, Now: ts(now)})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading throttle: %w", err)
	}
	return true, nil
}

func (s *Service) features(ctx context.Context, q *db.Queries, in Input, now time.Time) (Features, error) {
	f := Features{
		Amount: in.Amount, Currency: in.Currency, Brand: in.Brand, BIN: in.BIN, IP: in.IP,
		Installments: in.Installments, OffSession: in.OffSession, Livemode: in.Owner.Livemode,
	}
	card, err := q.Velocity(ctx, db.VelocityParams{Card: in.CardFingerprint, Livemode: in.Owner.Livemode, HourAgo: ts(now.Add(-time.Hour)), DayAgo: ts(now.Add(-24 * time.Hour))})
	if err != nil {
		return Features{}, fmt.Errorf("card velocity: %w", err)
	}
	f.CardAttempts1h, f.CardAttempts24h, f.CardDeclines24h = int(card.CardAttempts1h), int(card.CardAttempts24h), int(card.CardDeclines24h)
	if in.IP != "" {
		ip, err := q.IPVelocity(ctx, db.IPVelocityParams{Ip: in.IP, MerchantID: in.Owner.Merchant.String(), Livemode: in.Owner.Livemode, HourAgo: ts(now.Add(-time.Hour)), DayAgo: ts(now.Add(-24 * time.Hour))})
		if err != nil {
			return Features{}, fmt.Errorf("address velocity: %w", err)
		}
		f.IPAttempts1h, f.IPCards24h = int(ip.IpAttempts1h), int(ip.IpCards24h)
	}
	m, err := q.MerchantVelocity(ctx, db.MerchantVelocityParams{
		MerchantID: in.Owner.Merchant.String(), Livemode: in.Owner.Livemode,
		MinuteAgo: ts(now.Add(-time.Minute)), TenMinutesAgo: ts(now.Add(-10 * time.Minute)),
	})
	if err != nil {
		return Features{}, fmt.Errorf("merchant velocity: %w", err)
	}
	f.MerchantAttempts1m, f.MerchantAttempts10m = int(m.Attempts1m), int(m.Attempts10m)
	if m.Decided10m > 0 {
		f.MerchantDeclineRatio10m = float64(m.Declines10m) / float64(m.Decided10m)
	}
	return f, nil
}

// lists returns the list entries the attempt matches, allow entries first.
func (s *Service) lists(ctx context.Context, q *db.Queries, in Input) ([]Fired, error) {
	items, err := q.MatchingListItems(ctx, db.MatchingListItemsParams{
		MerchantID: in.Owner.Merchant.String(), Livemode: in.Owner.Livemode,
		Card: in.MerchantFingerprint, Ip: in.IP, Bin: in.BIN,
	})
	if err != nil {
		return nil, fmt.Errorf("matching lists: %w", err)
	}
	out := make([]Fired, 0, len(items))
	for _, item := range items {
		action := Block
		if item.List == "allow" {
			action = Allow
		}
		out = append(out, Fired{
			ID: item.ID, Action: action,
			Description: fmt.Sprintf("The %s %s is on the %s list.", item.Kind, item.Value, item.List),
		})
	}
	return out, nil
}

// applyLists is what fired once the merchant's lists have their say. An allow entry,
// sorted first, wins over the merchant's block entries, as it does over its own rules,
// and sets aside the platform's milder actions; but not the platform's blocks nor the
// card-testing throttle: an entry for an address the merchant itself reports must not
// switch off what protects the networks and the other merchants. A block entry alone
// blocks.
func applyLists(listed, platform, own, throttle []Fired) []Fired {
	switch {
	case len(listed) == 0:
		return slices.Concat(platform, own, throttle)
	case listed[0].Action != Allow:
		return listed[:1]
	}
	out := []Fired{listed[0]}
	for _, f := range platform {
		if f.Action == Block {
			out = append(out, f)
		}
	}
	return append(out, throttle...)
}

// rules runs the platform's rules and the merchant's own, and answers what fired of each.
func (s *Service) rules(ctx context.Context, q *db.Queries, owner Owner, f Features) (platform, own []Fired, err error) {
	rows, err := q.ActiveRules(ctx, db.ActiveRulesParams{MerchantID: owner.Merchant.String(), Livemode: owner.Livemode})
	if err != nil {
		return nil, nil, fmt.Errorf("reading rules: %w", err)
	}
	var merchants []Rule
	var inert []Fired
	for _, r := range rows {
		program, err := Compile(r.Expression)
		if err != nil {
			// A rule the language no longer admits is skipped, and the log says so.
			inert = append(inert, Fired{ID: r.ID, Action: Allow, Description: "The rule is no longer allowed and was skipped: " + err.Error(), Expression: r.Expression})
			continue
		}
		merchants = append(merchants, Rule{ID: r.ID, Action: Action(r.Action), Description: r.Description, Expression: r.Expression, program: program})
	}
	return run(PlatformRules(), f), append(inert, run(merchants, f)...), nil
}

func run(rules []Rule, f Features) []Fired {
	var fired []Fired
	for _, r := range rules {
		out, err := expr.Run(r.program, f)
		if err != nil {
			// A rule that fails, such as by dividing by a zero feature, is skipped and
			// recorded, so one bad rule cannot stop a merchant's payments.
			fired = append(fired, Fired{ID: r.ID, Action: Allow, Description: "The rule could not be evaluated and was skipped: " + err.Error(), Expression: r.Expression})
			continue
		}
		if matched, _ := out.(bool); matched {
			fired = append(fired, Fired{ID: r.ID, Action: r.Action, Description: r.Description, Expression: r.Expression})
		}
	}
	return fired
}

// cardTesting throttles a merchant whose recent attempts look like card testing, and
// blocks what goes over the throttle's rate.
func (s *Service) cardTesting(ctx context.Context, q *db.Queries, owner Owner, f *Features, throttled bool, now time.Time) ([]Fired, error) {
	ct := s.cfg.CardTesting
	switch {
	case throttled:
		f.Throttled = true
	case f.MerchantAttempts10m >= ct.MinAttempts && f.MerchantDeclineRatio10m >= ct.Ratio:
		if err := q.InsertThrottle(ctx, db.InsertThrottleParams{
			MerchantID: owner.Merchant.String(), Livemode: owner.Livemode, StartedAt: ts(now), Until: ts(now.Add(ct.Throttle)),
			Ratio: f.MerchantDeclineRatio10m, Attempts: int32(f.MerchantAttempts10m), //nolint:gosec // a count of attempts
		}); err != nil {
			return nil, fmt.Errorf("starting throttle: %w", err)
		}
		f.Throttled = true
	}
	if !f.Throttled || f.MerchantAttempts1m < ct.Rate {
		return nil, nil
	}
	return []Fired{{
		ID: "card_testing_throttle", Action: Block,
		Description: fmt.Sprintf("The merchant is throttled for card testing: %.0f%% of its last %d attempts in ten minutes were declined "+
			"(threshold %.0f%% over at least %d), and it has already made %d attempts this minute (limit %d).",
			f.MerchantDeclineRatio10m*100, f.MerchantAttempts10m, ct.Ratio*100, ct.MinAttempts, f.MerchantAttempts1m, ct.Rate),
	}}, nil
}

func (s *Service) record(ctx context.Context, q *db.Queries, in Input, d Decision) error {
	rules, err := json.Marshal(nonNil(d.Rules))
	if err != nil {
		return err
	}
	features, err := json.Marshal(d.Features)
	if err != nil {
		return err
	}
	outcome := "pending"
	if d.Action == Block {
		outcome = "blocked"
	}
	if err := q.InsertAttempt(ctx, db.InsertAttemptParams{
		AttemptID: in.Attempt, MerchantID: in.Owner.Merchant.String(), Livemode: in.Owner.Livemode,
		CardFingerprint: in.CardFingerprint, Ip: in.IP, CreatedAt: ts(d.Created), Outcome: outcome,
	}); err != nil {
		return fmt.Errorf("recording attempt: %w", err)
	}
	return q.InsertDecision(ctx, db.InsertDecisionParams{
		ID: d.ID, AttemptID: in.Attempt, IntentID: in.Intent, MerchantID: in.Owner.Merchant.String(), Livemode: in.Owner.Livemode,
		Action: string(d.Action), Rules: rules, Features: features, CreatedAt: ts(d.Created),
	})
}

// RecordOutcome tells the velocity counters how an attempt ended: approved or declined.
func (s *Service) RecordOutcome(ctx context.Context, tx pgx.Tx, attemptID string, approved bool) error {
	outcome := "declined"
	if approved {
		outcome = "approved"
	}
	return db.New(tx).SetOutcome(ctx, db.SetOutcomeParams{AttemptID: attemptID, Outcome: outcome})
}

func nonNil(f []Fired) []Fired {
	if f == nil {
		return []Fired{}
	}
	return f
}

func ts(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: !t.IsZero()}
}
