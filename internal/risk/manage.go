package risk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/iricardofernandes/jupiter/internal/platform/page"
	"github.com/iricardofernandes/jupiter/internal/risk/db"
)

const maxRules = 100

type MerchantRule struct {
	Rule
	Created time.Time
}

// CreateRule adds a merchant rule, refusing one that does not compile.
func (s *Service) CreateRule(ctx context.Context, tx pgx.Tx, owner Owner, action Action, expression, description string) (MerchantRule, error) {
	expression = strings.TrimSpace(expression)
	switch {
	case !validAction(action):
		return MerchantRule{}, fmt.Errorf("%w: action must be allow, review, request_3ds or block", ErrInvalid)
	case expression == "" || len(expression) > maxLength:
		return MerchantRule{}, fmt.Errorf("%w: expression must be 1 to %d characters", ErrInvalid, maxLength)
	case len(description) > 500:
		return MerchantRule{}, fmt.Errorf("%w: description is longer than 500 characters", ErrInvalid)
	}
	q := db.New(tx)
	existing, err := q.ActiveRules(ctx, db.ActiveRulesParams{MerchantID: owner.Merchant.String(), Livemode: owner.Livemode})
	if err != nil {
		return MerchantRule{}, err
	}
	if len(existing) >= maxRules {
		return MerchantRule{}, fmt.Errorf("%w: a merchant can have at most %d rules", ErrInvalid, maxRules)
	}
	if err := Validate(expression); err != nil {
		return MerchantRule{}, err
	}
	row, err := q.InsertRule(ctx, db.InsertRuleParams{
		ID: RulePrefix.New().String(), MerchantID: owner.Merchant.String(), Livemode: owner.Livemode,
		Action: string(action), Expression: expression, Description: description, CreatedAt: ts(s.cfg.Now().UTC()),
	})
	if err != nil {
		return MerchantRule{}, fmt.Errorf("creating rule: %w", err)
	}
	return ruleOf(row), nil
}

func (s *Service) Rules(ctx context.Context, q db.DBTX, owner Owner) ([]MerchantRule, error) {
	rows, err := db.New(q).ActiveRules(ctx, db.ActiveRulesParams{MerchantID: owner.Merchant.String(), Livemode: owner.Livemode})
	if err != nil {
		return nil, err
	}
	out := make([]MerchantRule, 0, len(rows))
	for _, r := range rows {
		out = append(out, ruleOf(r))
	}
	return out, nil
}

func (s *Service) DeleteRule(ctx context.Context, tx pgx.Tx, owner Owner, ruleID string) (MerchantRule, error) {
	row, err := db.New(tx).DeleteRule(ctx, db.DeleteRuleParams{ID: ruleID, MerchantID: owner.Merchant.String(), Livemode: owner.Livemode, Now: ts(s.cfg.Now().UTC())})
	if errors.Is(err, pgx.ErrNoRows) {
		return MerchantRule{}, fmt.Errorf("%w: %s", ErrNotFound, ruleID)
	}
	if err != nil {
		return MerchantRule{}, err
	}
	return ruleOf(row), nil
}

func ruleOf(r db.RiskRule) MerchantRule {
	return MerchantRule{
		Rule:    Rule{ID: r.ID, Action: Action(r.Action), Description: r.Description, Expression: r.Expression},
		Created: r.CreatedAt.Time,
	}
}

type ListItem struct {
	ID      string
	List    string
	Kind    string
	Value   string
	Created time.Time
}

// maxListItems bounds a merchant's list entries in a mode.
const maxListItems = 10_000

var (
	binPattern         = regexp.MustCompile(`^[0-9]{6,8}$`)
	fingerprintPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// listValue checks an entry's value against its kind, and writes an address the way
// attempts are matched: a mapped IPv4 address as IPv4, an IPv6 one compressed.
func listValue(kind, value string) (string, error) {
	switch kind {
	case "ip":
		addr, err := netip.ParseAddr(value)
		if err != nil || addr.Zone() != "" {
			return "", fmt.Errorf("%w: value must be an IPv4 or IPv6 address", ErrInvalid)
		}
		return addr.Unmap().String(), nil
	case "bin":
		if !binPattern.MatchString(value) {
			return "", fmt.Errorf("%w: value must be a BIN of 6 to 8 digits", ErrInvalid)
		}
	case "card_fingerprint":
		if value = strings.ToLower(value); !fingerprintPattern.MatchString(value) {
			return "", fmt.Errorf("%w: value must be a card fingerprint, as payment methods show it", ErrInvalid)
		}
	default:
		return "", fmt.Errorf("%w: kind must be card_fingerprint, ip or bin", ErrInvalid)
	}
	return value, nil
}

func (s *Service) AddListItem(ctx context.Context, tx pgx.Tx, owner Owner, list, kind, value string) (ListItem, error) {
	if list != "allow" && list != "block" {
		return ListItem{}, fmt.Errorf("%w: list must be allow or block", ErrInvalid)
	}
	value, err := listValue(kind, strings.TrimSpace(value))
	if err != nil {
		return ListItem{}, err
	}
	q := db.New(tx)
	if err := q.LockListItems(ctx, owner.Merchant.String()+"/"+strconv.FormatBool(owner.Livemode)); err != nil {
		return ListItem{}, err
	}
	n, err := q.CountListItems(ctx, db.CountListItemsParams{MerchantID: owner.Merchant.String(), Livemode: owner.Livemode})
	if err != nil {
		return ListItem{}, err
	}
	if n >= maxListItems {
		return ListItem{}, fmt.Errorf("%w: a merchant has at most %d list entries in each mode", ErrInvalid, maxListItems)
	}
	row, err := q.InsertListItem(ctx, db.InsertListItemParams{
		ID: ListItemPrefix.New().String(), MerchantID: owner.Merchant.String(), Livemode: owner.Livemode,
		List: list, Kind: kind, Value: value, CreatedAt: ts(s.cfg.Now().UTC()),
	})
	if err != nil {
		return ListItem{}, fmt.Errorf("adding list item: %w", err)
	}
	return listItemOf(row), nil
}

func (s *Service) ListItems(ctx context.Context, q db.DBTX, owner Owner, r page.Request) ([]ListItem, bool, error) {
	rows, err := db.New(q).ListItems(ctx, db.ListItemsParams{
		MerchantID: owner.Merchant.String(), Livemode: owner.Livemode,
		StartingAfter: r.StartingAfter, EndingBefore: r.EndingBefore, MaxCount: r.Fetch(),
	})
	if err != nil {
		return nil, false, err
	}
	out := make([]ListItem, 0, len(rows))
	for _, row := range rows {
		out = append(out, listItemOf(row))
	}
	items, more := page.Trim(out, r)
	return items, more, nil
}

func (s *Service) RemoveListItem(ctx context.Context, tx pgx.Tx, owner Owner, itemID string) error {
	n, err := db.New(tx).DeleteListItem(ctx, db.DeleteListItemParams{ID: itemID, MerchantID: owner.Merchant.String(), Livemode: owner.Livemode})
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, itemID)
	}
	return nil
}

func listItemOf(r db.RiskListItem) ListItem {
	return ListItem{ID: r.ID, List: r.List, Kind: r.Kind, Value: r.Value, Created: r.CreatedAt.Time}
}

func (s *Service) Decision(ctx context.Context, q db.DBTX, owner Owner, decisionID string) (Decision, error) {
	row, err := db.New(q).GetDecision(ctx, db.GetDecisionParams{ID: decisionID, MerchantID: owner.Merchant.String(), Livemode: owner.Livemode})
	if errors.Is(err, pgx.ErrNoRows) {
		return Decision{}, fmt.Errorf("%w: %s", ErrNotFound, decisionID)
	}
	if err != nil {
		return Decision{}, err
	}
	return decisionOf(row)
}

// DecisionFor is the decision on an attempt, if the engine made one.
func (s *Service) DecisionFor(ctx context.Context, q db.DBTX, attemptID string) (Decision, bool, error) {
	row, err := db.New(q).DecisionForAttempt(ctx, attemptID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Decision{}, false, nil
	}
	if err != nil {
		return Decision{}, false, err
	}
	d, err := decisionOf(row)
	return d, true, err
}

type DecisionFilter struct {
	Action Action
	Intent string
}

func (s *Service) Decisions(ctx context.Context, q db.DBTX, owner Owner, filter DecisionFilter, r page.Request) ([]Decision, bool, error) {
	rows, err := db.New(q).ListDecisions(ctx, db.ListDecisionsParams{
		MerchantID: owner.Merchant.String(), Livemode: owner.Livemode, Action: string(filter.Action), IntentID: filter.Intent,
		StartingAfter: r.StartingAfter, EndingBefore: r.EndingBefore, MaxCount: r.Fetch(),
	})
	if err != nil {
		return nil, false, err
	}
	out := make([]Decision, 0, len(rows))
	for _, row := range rows {
		d, err := decisionOf(row)
		if err != nil {
			return nil, false, err
		}
		out = append(out, d)
	}
	items, more := page.Trim(out, r)
	return items, more, nil
}

func decisionOf(row db.RiskDecision) (Decision, error) {
	d := Decision{ID: row.ID, Attempt: row.AttemptID, Intent: row.IntentID, Action: Action(row.Action), Created: row.CreatedAt.Time}
	if err := json.Unmarshal(row.Rules, &d.Rules); err != nil {
		return Decision{}, fmt.Errorf("decision %s rules: %w", row.ID, err)
	}
	if err := json.Unmarshal(row.Features, &d.Features); err != nil {
		return Decision{}, fmt.Errorf("decision %s features: %w", row.ID, err)
	}
	return d, nil
}
