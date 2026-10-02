// Package risk decides, before a payment reaches a rail, whether to let it through,
// flag it for review, ask the cardholder to authenticate, or block it. A decision comes
// from allow and block lists, velocity counters over sliding windows, the platform's
// rules and the merchant's own, written in a small expression language, and card-testing
// detection. Every decision is recorded with the rules that fired and the features they
// saw (ADR 0022).
package risk

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/parser"
	"github.com/expr-lang/expr/vm"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/risk/migrations"
)

var (
	DecisionPrefix = id.MustPrefix("rd")
	RulePrefix     = id.MustPrefix("rr")
	ListItemPrefix = id.MustPrefix("rli")
)

var (
	ErrInvalid  = errors.New("risk: invalid request")
	ErrNotFound = errors.New("risk: not found")
)

type Action string

const (
	Allow      Action = "allow"
	Review     Action = "review"
	Request3DS Action = "request_3ds"
	Block      Action = "block"
)

// severity orders actions: a decision takes the most severe of the rules that fired.
var severity = map[Action]int{Allow: 0, Review: 1, Request3DS: 2, Block: 3}

func validAction(a Action) bool {
	_, ok := severity[a]
	return ok
}

// Features are what rules see. The expr tags are the names rules use.
type Features struct {
	Amount       int64  `expr:"amount" json:"amount"`
	Currency     string `expr:"currency" json:"currency"`
	Brand        string `expr:"brand" json:"brand"`
	BIN          string `expr:"bin" json:"bin"`
	IP           string `expr:"ip" json:"ip"`
	CardIP       string `expr:"card_ip" json:"card_ip"`
	Installments int    `expr:"installments" json:"installments"`
	OffSession   bool   `expr:"off_session" json:"off_session"`
	Livemode     bool   `expr:"livemode" json:"livemode"`

	CardAttempts1h  int `expr:"card_attempts_1h" json:"card_attempts_1h"`
	CardAttempts24h int `expr:"card_attempts_24h" json:"card_attempts_24h"`
	CardDeclines24h int `expr:"card_declines_24h" json:"card_declines_24h"`
	IPAttempts1h    int `expr:"ip_attempts_1h" json:"ip_attempts_1h"`
	IPCards24h      int `expr:"ip_cards_24h" json:"ip_cards_24h"`

	MerchantAttempts1m      int     `expr:"merchant_attempts_1m" json:"merchant_attempts_1m"`
	MerchantAttempts10m     int     `expr:"merchant_attempts_10m" json:"merchant_attempts_10m"`
	MerchantDeclineRatio10m float64 `expr:"merchant_decline_ratio_10m" json:"merchant_decline_ratio_10m"`
	Throttled               bool    `expr:"throttled" json:"throttled"`
}

// Rule is one rule of the rules language: when Expression holds, Action applies.
type Rule struct {
	ID          string `json:"id"`
	Action      Action `json:"action"`
	Description string `json:"description"`
	Expression  string `json:"expression"`
	program     *vm.Program
}

// maxNodes and maxLength bound how large an expression may be.
const (
	maxNodes  = 200
	maxLength = 1000
)

// Compile checks an expression against the features, and that it yields a boolean.
// Rules run inside the payment's transaction, so what one costs must be bounded by its
// size: a tree of features, literals, lists and operators is. A variable is refused
// because a later node can use it twice, which lets a short expression double a string
// at every step.
func Compile(expression string) (*vm.Program, error) {
	if program, ok := programs.get(expression); ok {
		return program, nil
	}
	program, err := compile(expression)
	if err != nil {
		return nil, err
	}
	programs.put(expression, program)
	return program, nil
}

// Validate checks an expression as Compile does, without keeping its program: a
// merchant creating rules must not grow the cache with rules it may never keep.
func Validate(expression string) error {
	_, err := compile(expression)
	return err
}

func compile(expression string) (*vm.Program, error) {
	if len(expression) > maxLength {
		return nil, fmt.Errorf("%w: an expression is at most %d characters", ErrInvalid, maxLength)
	}
	tree, err := parser.Parse(expression)
	if err != nil {
		return nil, fmt.Errorf("%w: the expression does not compile: %w", ErrInvalid, err)
	}
	var check restricted
	ast.Walk(&tree.Node, &check)
	if check.err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, check.err)
	}
	program, err := expr.Compile(expression, expr.Env(Features{}), expr.AsBool(), expr.DisableAllBuiltins(), expr.MaxNodes(maxNodes))
	if err != nil {
		return nil, fmt.Errorf("%w: the expression does not compile: %w", ErrInvalid, err)
	}
	return program, nil
}

// allowedOperators are the binary operators of the rules language. Ranges (..) and
// matches, whose regular expression could be made costly to compile and run, are not.
var allowedOperators = map[string]bool{
	"==": true, "!=": true, "<": true, ">": true, "<=": true, ">=": true,
	"&&": true, "||": true, "and": true, "or": true,
	"+": true, "-": true, "*": true, "/": true, "%": true,
	"in": true, "contains": true, "startsWith": true, "endsWith": true,
}

type restricted struct {
	err error
}

func (r *restricted) Visit(node *ast.Node) {
	if r.err != nil {
		return
	}
	switch n := (*node).(type) {
	case *ast.IdentifierNode, *ast.IntegerNode, *ast.FloatNode, *ast.BoolNode, *ast.StringNode, *ast.NilNode,
		*ast.ConstantNode, *ast.ArrayNode, *ast.ConditionalNode:
	case *ast.UnaryNode:
		if n.Operator != "!" && n.Operator != "not" && n.Operator != "-" && n.Operator != "+" {
			r.err = fmt.Errorf("the operator %s is not allowed in rules", n.Operator)
		}
	case *ast.BinaryNode:
		if !allowedOperators[n.Operator] {
			r.err = fmt.Errorf("the operator %s is not allowed in rules", n.Operator)
		}
	case *ast.CallNode, *ast.BuiltinNode, *ast.PredicateNode:
		r.err = errors.New("functions are not allowed in rules")
	case *ast.VariableDeclaratorNode, *ast.SequenceNode:
		r.err = errors.New("variables are not allowed in rules")
	default:
		r.err = errors.New("only features, literals, lists and operators are allowed in rules")
	}
}

// maxPrograms bounds the compiled programs kept, by expression: well above the platform's
// rules and those of the merchants deciding at once.
const maxPrograms = 1 << 16

var programs = &programCache{byExpression: map[string]*vm.Program{}}

type programCache struct {
	mu           sync.RWMutex
	byExpression map[string]*vm.Program
}

func (c *programCache) get(expression string) (*vm.Program, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	p, ok := c.byExpression[expression]
	return p, ok
}

// put keeps a program, letting an arbitrary one go when the cache is full.
func (c *programCache) put(expression string, p *vm.Program) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.byExpression) >= maxPrograms {
		for evicted := range c.byExpression {
			delete(c.byExpression, evicted)
			break
		}
	}
	c.byExpression[expression] = p
}

//go:embed platform_rules.json
var platformRulesJSON []byte

var platformRules = mustRules(platformRulesJSON)

func mustRules(data []byte) []Rule {
	var rules []Rule
	if err := json.Unmarshal(data, &rules); err != nil {
		panic(err)
	}
	for i := range rules {
		program, err := Compile(rules[i].Expression)
		if err != nil || !validAction(rules[i].Action) {
			panic(fmt.Sprintf("risk: platform rule %s: %v", rules[i].ID, err))
		}
		rules[i].program = program
	}
	return rules
}

// PlatformRules are the rules every payment is checked against.
func PlatformRules() []Rule { return slices.Clone(platformRules) }

// CardTesting sets when a merchant counts as under card testing: in the last ten
// minutes, at least MinAttempts attempts of which at least Ratio were declined. It is
// then throttled for Throttle, during which only Rate attempts a minute go through.
type CardTesting struct {
	MinAttempts int
	Ratio       float64
	Throttle    time.Duration
	Rate        int
}

var DefaultCardTesting = CardTesting{MinAttempts: 20, Ratio: 0.2, Throttle: 30 * time.Minute, Rate: 3}

type Config struct {
	Now         func() time.Time
	CardTesting CardTesting
}

type Service struct {
	cfg Config
}

func New(cfg Config) *Service {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.CardTesting == (CardTesting{}) {
		cfg.CardTesting = DefaultCardTesting
	}
	return &Service{cfg: cfg}
}

func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return postgres.Migrate(ctx, pool, "risk", migrations.FS)
}

type Owner struct {
	Merchant id.ID
	Livemode bool
}
