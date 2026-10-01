package registry

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
)

var (
	ErrInvalid   = errors.New("registry: invalid request")
	ErrSettled   = errors.New("registry: the unit is settled")
	ErrNotFound  = errors.New("registry: not found")
	ErrBlock     = errors.New("registry: a block may only take the free amount")
	ErrForbidden = errors.New("registry: not allowed")
)

// Effect is what a contract does to the units it reaches (the Convenção's effect types).
type Effect string

const (
	OwnershipTransfer Effect = "ownership_transfer" // troca de titularidade
	Lien              Effect = "lien"               // ônus: gravame, trava, penhora
)

// Rule is how a contract divides a unit: by a fixed amount across the agenda it reaches,
// or by a percentage of each unit.
type Rule string

const (
	Fixed      Rule = "fixed"
	Percentage Rule = "percentage"
)

// UnitKey names a receivable unit (Res. BCB 264 art. 2º): the accreditor (credenciador or
// subcredenciador), the merchant (holder), the arrangement and the settlement date.
type UnitKey struct {
	Accreditor  string
	Holder      string
	Arrangement string
	Date        string // YYYY-MM-DD
}

func (k UnitKey) String() string {
	return k.Accreditor + "/" + k.Holder + "/" + k.Arrangement + "/" + k.Date
}

type Domicile struct {
	ISPB    string `json:"ispb"`
	Branch  string `json:"branch,omitempty"`
	Account string `json:"account"`
}

type Contract struct {
	ID          string
	Beneficiary string
	Holder      string
	Effect      Effect
	Rule        Rule
	// Amount is what a fixed contract keeps committed or paid across the units it
	// reaches; BasisPoints the share of each unit a percentage contract takes.
	Amount      int64
	BasisPoints int64
	// Arrangements and Accreditors limit the units reached; empty reaches all of the
	// holder's. From and To bound their settlement dates, inclusive; empty is unbounded.
	Arrangements []string
	Accreditors  []string
	From, To     string
	Domicile     Domicile
	seq          int64
	ended        bool
	paid         int64
}

type unit struct {
	key       UnitKey
	value     int64 // constituted, net: what the unit will settle for
	blocked   int64
	domicile  Domicile
	committed map[string]int64 // by contract
	settled   bool
	payments  []Payment
}

func (u *unit) free() int64 {
	f := u.value - u.blocked
	for _, c := range u.committed {
		f -= c
	}
	return f
}

// Payment is one line of a unit's settlement: to a contract's beneficiary, to the holder
// (the free amount), or kept by the accreditor (the blocked amount).
type Payment struct {
	To       string   `json:"to"`
	Contract string   `json:"contract,omitempty"`
	Amount   int64    `json:"amount"`
	Domicile Domicile `json:"domicile"`
}

// engine keeps the units and contracts and applies the Convenção's order to them:
// contracts take a unit's amounts first come, first served (item 3.13), a reduction
// takes the free amount first and then the latest contract's (item 3.14), and a block
// takes only what is free (Res. BCB 264 art. 8º). It is not safe for concurrent use.
type engine struct {
	units     map[UnitKey]*unit
	contracts map[string]*Contract
	order     []*Contract // by acceptance
	seq       int64
}

func newEngine() *engine {
	return &engine{units: map[UnitKey]*unit{}, contracts: map[string]*Contract{}}
}

// SetUnit registers a unit or updates it to value and blocked, both absolute. More value
// is constituted; less is a reduction. A settled unit cannot change.
func (e *engine) SetUnit(key UnitKey, value, blocked int64, domicile Domicile) error {
	if value < 0 || blocked < 0 || blocked > value {
		return fmt.Errorf("%w: value %d and blocked %d", ErrInvalid, value, blocked)
	}
	u := e.units[key]
	if u == nil {
		u = &unit{key: key, committed: map[string]int64{}}
	}
	if u.settled {
		return fmt.Errorf("%w: %s", ErrSettled, key)
	}
	// A block may take only what is free once the value has changed: checked before
	// anything changes, so a refused update changes nothing.
	free, kept := u.afterChange(value)
	if blocked > kept && blocked-kept > free {
		return fmt.Errorf("%w: %s will have %d free, the block asks for %d more", ErrBlock, key, free, blocked-kept)
	}
	e.units[key] = u
	u.domicile = domicile
	if value < u.value {
		e.reduce(u, u.value-value)
	} else {
		u.value = value
	}
	u.blocked = blocked
	e.fill()
	return nil
}

// afterChange is what would be free, and what would stay blocked, were u's value set to
// value: a reduction takes the free amount, then the contracts', then the block.
func (u *unit) afterChange(value int64) (free, blocked int64) {
	free = u.free()
	if value >= u.value {
		return free + value - u.value, u.blocked
	}
	r := u.value - value
	fromFree := min(r, max(free, 0))
	r -= fromFree
	var committed int64
	for _, c := range u.committed {
		committed += c
	}
	r -= min(r, committed)
	return free - fromFree, u.blocked - min(r, u.blocked)
}

// reduce takes r from u: its free amount, then its contracts from the latest accepted
// back, then its block.
func (e *engine) reduce(u *unit, r int64) {
	u.value -= r
	r -= min(r, max(u.free()+r, 0))
	for i := len(e.order) - 1; i >= 0 && r > 0; i-- {
		c := e.order[i]
		take := min(r, u.committed[c.ID])
		u.committed[c.ID] -= take
		r -= take
	}
	u.blocked -= min(r, u.blocked)
}

// Accept registers a contract, which takes, after every earlier one, what it reaches.
func (e *engine) Accept(c Contract) error {
	if err := validContract(c); err != nil {
		return err
	}
	if _, ok := e.contracts[c.ID]; ok {
		return fmt.Errorf("%w: contract %s exists", ErrInvalid, c.ID)
	}
	e.seq++
	c.seq = e.seq
	e.contracts[c.ID] = &c
	e.order = append(e.order, &c)
	e.fill()
	return nil
}

func validContract(c Contract) error {
	switch {
	case c.ID == "" || c.Beneficiary == "" || c.Holder == "":
		return fmt.Errorf("%w: a contract needs an id, a beneficiary and a holder", ErrInvalid)
	case c.Effect != OwnershipTransfer && c.Effect != Lien:
		return fmt.Errorf("%w: effect %q", ErrInvalid, c.Effect)
	case c.Rule == Fixed && c.Amount <= 0, c.Rule == Percentage && (c.BasisPoints <= 0 || c.BasisPoints > 10_000):
		return fmt.Errorf("%w: a fixed contract needs a positive amount, a percentage one 1 to 10000 basis points", ErrInvalid)
	case c.Rule != Fixed && c.Rule != Percentage:
		return fmt.Errorf("%w: rule %q", ErrInvalid, c.Rule)
	case c.From != "" && c.To != "" && c.To < c.From:
		return fmt.Errorf("%w: the period ends before it starts", ErrInvalid)
	}
	return nil
}

// End releases what a contract holds on unsettled units; later contracts may take it.
func (e *engine) End(contractID string) error {
	c := e.contracts[contractID]
	if c == nil {
		return fmt.Errorf("%w: contract %s", ErrNotFound, contractID)
	}
	if c.ended {
		return nil
	}
	c.ended = true
	for _, u := range e.units {
		if !u.settled {
			delete(u.committed, c.ID)
		}
	}
	e.fill()
	return nil
}

// fill lets each live contract, in the order accepted, take the free amounts of the
// unsettled units it reaches, earliest settlement date first, up to what it is owed: a
// fixed contract its amount less what it holds and was paid, a percentage one its share
// of each unit.
func (e *engine) fill() {
	units := e.unsettled()
	for _, c := range e.order {
		if c.ended {
			continue
		}
		need := c.owed(units)
		for _, u := range units {
			if !c.reaches(u.key) {
				continue
			}
			want := need
			if c.Rule == Percentage {
				want = u.value*c.BasisPoints/10_000 - u.committed[c.ID]
			}
			if take := min(want, u.free()); take > 0 {
				u.committed[c.ID] += take
				need -= take
			}
		}
	}
}

// owed is what a fixed contract still lacks: its amount less what it was paid and holds
// on the units not yet settled. A percentage contract is owed a share of each unit
// instead, which fill works out unit by unit.
func (c *Contract) owed(units []*unit) int64 {
	if c.Rule != Fixed {
		return 0
	}
	need := c.Amount - c.paid
	for _, u := range units {
		need -= u.committed[c.ID]
	}
	return need
}

func (e *engine) unsettled() []*unit {
	var out []*unit
	for _, u := range e.units {
		if !u.settled {
			out = append(out, u)
		}
	}
	slices.SortFunc(out, func(a, b *unit) int {
		return cmp.Or(cmp.Compare(a.key.Date, b.key.Date), cmp.Compare(a.key.String(), b.key.String()))
	})
	return out
}

func (c *Contract) reaches(k UnitKey) bool {
	return k.Holder == c.Holder &&
		(len(c.Arrangements) == 0 || slices.Contains(c.Arrangements, k.Arrangement)) &&
		(len(c.Accreditors) == 0 || slices.Contains(c.Accreditors, k.Accreditor)) &&
		(c.From == "" || k.Date >= c.From) && (c.To == "" || k.Date <= c.To)
}

// Settle pays a unit once, for amount, at most what it is worth net of its block: its
// contracts in the order accepted, then the holder; the block stays with the accreditor.
// What a contract was not paid, when amount falls short, it takes from other units.
func (e *engine) Settle(key UnitKey, amount int64) ([]Payment, error) {
	u := e.units[key]
	if u == nil {
		return nil, fmt.Errorf("%w: unit %s", ErrNotFound, key)
	}
	if u.settled {
		return nil, fmt.Errorf("%w: %s", ErrSettled, key)
	}
	if amount < 0 || amount > u.value-u.blocked {
		return nil, fmt.Errorf("%w: %s can settle for up to %d, not %d", ErrInvalid, key, u.value-u.blocked, amount)
	}
	left := amount
	var out []Payment
	for _, c := range e.order {
		pay := min(left, u.committed[c.ID])
		if pay > 0 {
			out = append(out, Payment{To: c.Beneficiary, Contract: c.ID, Amount: pay, Domicile: c.Domicile})
			c.paid += pay
			left -= pay
		}
	}
	if left > 0 {
		out = append(out, Payment{To: key.Holder, Amount: left, Domicile: u.domicile})
	}
	if u.blocked > 0 {
		out = append(out, Payment{To: key.Accreditor, Amount: u.blocked})
	}
	u.settled, u.payments = true, out
	e.fill()
	return out, nil
}

// Instructions are how a unit would settle for all it is worth now: who is paid, how
// much, and where. A settled unit answers how it was paid.
func (e *engine) Instructions(key UnitKey) ([]Payment, error) {
	u := e.units[key]
	if u == nil {
		return nil, fmt.Errorf("%w: unit %s", ErrNotFound, key)
	}
	if u.settled {
		return u.payments, nil
	}
	var out []Payment
	for _, c := range e.order {
		if amount := u.committed[c.ID]; amount > 0 {
			out = append(out, Payment{To: c.Beneficiary, Contract: c.ID, Amount: amount, Domicile: c.Domicile})
		}
	}
	if free := u.free(); free > 0 {
		out = append(out, Payment{To: key.Holder, Amount: free, Domicile: u.domicile})
	}
	if u.blocked > 0 {
		out = append(out, Payment{To: key.Accreditor, Amount: u.blocked})
	}
	return out, nil
}

// Position is a unit as the registry sees it.
type Position struct {
	Key       UnitKey
	Value     int64
	Blocked   int64
	Free      int64
	Committed []Commitment
	Settled   bool
	Payments  []Payment
	Domicile  Domicile
}

type Commitment struct {
	Contract    string `json:"contract"`
	Beneficiary string `json:"beneficiary"`
	Effect      Effect `json:"effect"`
	Amount      int64  `json:"amount"`
}

func (e *engine) position(u *unit) Position {
	p := Position{Key: u.key, Value: u.value, Blocked: u.blocked, Free: u.free(), Settled: u.settled, Payments: u.payments, Domicile: u.domicile}
	for _, c := range e.order {
		if amount := u.committed[c.ID]; amount > 0 {
			p.Committed = append(p.Committed, Commitment{Contract: c.ID, Beneficiary: c.Beneficiary, Effect: c.Effect, Amount: amount})
		}
	}
	return p
}

// Positions lists the units matching keep, earliest settlement date first.
func (e *engine) Positions(keep func(UnitKey) bool) []Position {
	var out []Position
	for _, u := range e.units {
		if keep(u.key) {
			out = append(out, e.position(u))
		}
	}
	slices.SortFunc(out, func(a, b Position) int {
		return cmp.Or(cmp.Compare(a.Key.Date, b.Key.Date), cmp.Compare(a.Key.String(), b.Key.String()))
	})
	return out
}
