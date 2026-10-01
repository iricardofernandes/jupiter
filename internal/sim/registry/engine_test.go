package registry

import (
	"errors"
	"fmt"
	"testing"

	"pgregory.net/rapid"
)

// model is the reference the engine is checked against: the same rules, written as
// plainly as possible over slices, with no caching of what can be recomputed.
type model struct {
	units     []*modelUnit
	contracts []*modelContract
}

type modelUnit struct {
	key            UnitKey
	value, blocked int64
	commits        []int64 // by contract index
	settled        bool
}

type modelContract struct {
	c     Contract
	paid  int64
	ended bool
}

func (m *model) unit(k UnitKey) *modelUnit {
	for _, u := range m.units {
		if u.key == k {
			return u
		}
	}
	return nil
}

func (m *model) committed(u *modelUnit) int64 {
	var sum int64
	for _, c := range u.commits {
		sum += c
	}
	return sum
}

func (m *model) grow(u *modelUnit) {
	for len(u.commits) < len(m.contracts) {
		u.commits = append(u.commits, 0)
	}
}

// ordered is the unsettled units by settlement date, then key.
func (m *model) ordered() []*modelUnit {
	var out []*modelUnit
	for _, u := range m.units {
		if !u.settled {
			out = append(out, u)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && (out[j].key.Date < out[j-1].key.Date ||
			(out[j].key.Date == out[j-1].key.Date && out[j].key.String() < out[j-1].key.String())); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func (m *model) fill() {
	for ci, mc := range m.contracts {
		if mc.ended {
			continue
		}
		for _, u := range m.ordered() {
			m.grow(u)
			if !mc.c.reaches(u.key) {
				continue
			}
			free := u.value - u.blocked - m.committed(u)
			var want int64
			if mc.c.Rule == Fixed {
				want = mc.c.Amount - mc.paid
				for _, v := range m.ordered() {
					m.grow(v)
					want -= v.commits[ci]
				}
			} else {
				want = u.value*mc.c.BasisPoints/10_000 - u.commits[ci]
			}
			if take := min(want, free); take > 0 {
				u.commits[ci] += take
			}
		}
	}
}

func (m *model) setUnit(k UnitKey, value, blocked int64) error {
	u := m.unit(k)
	isNew := u == nil
	if isNew {
		u = &modelUnit{key: k}
	}
	m.grow(u)
	if u.settled {
		return ErrSettled
	}
	// Work out the outcome on a copy; a refused block leaves everything as it was.
	trial := *u
	trial.commits = append([]int64(nil), u.commits...)
	m.change(&trial, value)
	if blocked-trial.blocked > trial.value-trial.blocked-m.committed(&trial) {
		return ErrBlock
	}
	if isNew {
		m.units = append(m.units, u)
	}
	m.change(u, value)
	u.blocked = blocked
	m.fill()
	return nil
}

// change sets u's value, taking a reduction from the free amount, then the contracts
// from the latest, then the block.
func (m *model) change(u *modelUnit, value int64) {
	if value < u.value {
		r := u.value - value
		free := u.value - u.blocked - m.committed(u)
		r -= min(r, free)
		for ci := len(m.contracts) - 1; ci >= 0; ci-- {
			take := min(r, u.commits[ci])
			u.commits[ci] -= take
			r -= take
		}
		u.blocked -= r
	}
	u.value = value
}

func (m *model) settle(k UnitKey, amount int64) map[string]int64 {
	u := m.unit(k)
	m.grow(u)
	paid := map[string]int64{}
	left := amount
	for ci, mc := range m.contracts {
		pay := min(left, u.commits[ci])
		paid[mc.c.ID] += pay
		mc.paid += pay
		left -= pay
	}
	paid["holder"] = left
	paid["accreditor"] = u.blocked
	u.settled = true
	m.fill()
	return paid
}

var (
	holders      = []string{"11111111000191", "22222222000191"}
	arrangements = []string{"VCC", "MCC"}
	dates        = []string{"2026-11-02", "2026-12-01", "2027-01-04", "2027-02-01"}
	accreditors  = []string{"33333333000191", "66666666000191"}
)

func genKey() *rapid.Generator[UnitKey] {
	return rapid.Custom(func(t *rapid.T) UnitKey {
		return UnitKey{
			Accreditor:  rapid.SampledFrom(accreditors).Draw(t, "accreditor"),
			Holder:      rapid.SampledFrom(holders).Draw(t, "holder"),
			Arrangement: rapid.SampledFrom(arrangements).Draw(t, "arrangement"),
			Date:        rapid.SampledFrom(dates).Draw(t, "date"),
		}
	})
}

func genContract(id string) *rapid.Generator[Contract] {
	return rapid.Custom(func(t *rapid.T) Contract {
		c := Contract{
			ID: id, Beneficiary: rapid.SampledFrom([]string{"44444444000191", "55555555000191"}).Draw(t, "beneficiary"),
			Holder: rapid.SampledFrom(holders).Draw(t, "holder"),
			Effect: rapid.SampledFrom([]Effect{OwnershipTransfer, Lien}).Draw(t, "effect"),
			Rule:   rapid.SampledFrom([]Rule{Fixed, Percentage}).Draw(t, "rule"),
		}
		if c.Rule == Fixed {
			c.Amount = rapid.Int64Range(1, 50_000).Draw(t, "amount")
		} else {
			c.BasisPoints = rapid.Int64Range(1, 10_000).Draw(t, "bps")
		}
		if rapid.Bool().Draw(t, "one arrangement") {
			c.Arrangements = []string{rapid.SampledFrom(arrangements).Draw(t, "arrangement")}
		}
		if rapid.Bool().Draw(t, "from") {
			c.From = rapid.SampledFrom(dates).Draw(t, "from date")
		}
		if rapid.Bool().Draw(t, "to") {
			c.To = rapid.SampledFrom(dates).Draw(t, "to date")
			if c.From > c.To {
				c.From, c.To = c.To, c.From
			}
		}
		if rapid.Bool().Draw(t, "one accreditor") {
			c.Accreditors = []string{rapid.SampledFrom(accreditors).Draw(t, "accreditor")}
		}
		return c
	})
}

// For any sequence of units constituted and reduced, contracts accepted and ended,
// blocks and settlements, each beneficiary is paid what the reference says, no unit is
// paid twice, and no amount is ever negative.
func TestPropertyWaterfall(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		e, m := newEngine(), &model{}
		paid, want := map[string]int64{}, map[string]int64{}
		steps := rapid.IntRange(1, 60).Draw(t, "steps")
		for step := range steps {
			switch rapid.IntRange(0, 9).Draw(t, "op") {
			case 0, 1, 2, 3:
				k := genKey().Draw(t, "key")
				value := rapid.Int64Range(0, 30_000).Draw(t, "value")
				blocked := rapid.Int64Range(0, value).Draw(t, "blocked")
				if rapid.IntRange(0, 2).Draw(t, "unblocked") > 0 {
					blocked = 0
				}
				err, merr := e.SetUnit(k, value, blocked, Domicile{}), m.setUnit(k, value, blocked)
				if (err == nil) != (merr == nil) || (merr != nil && !errors.Is(err, merr)) {
					t.Fatalf("SetUnit %v %d/%d: engine %v, model %v", k, value, blocked, err, merr)
				}
			case 4, 5:
				c := genContract(fmt.Sprintf("c%d", step)).Draw(t, "contract")
				if err := e.Accept(c); err != nil {
					t.Fatal(err)
				}
				m.contracts = append(m.contracts, &modelContract{c: c})
				m.fill()
			case 6:
				if len(m.contracts) == 0 {
					continue
				}
				i := rapid.IntRange(0, len(m.contracts)-1).Draw(t, "end")
				if err := e.End(m.contracts[i].c.ID); err != nil {
					t.Fatal(err)
				}
				m.contracts[i].ended = true
				for _, u := range m.units {
					if !u.settled {
						m.grow(u)
						u.commits[i] = 0
					}
				}
				m.fill()
			default:
				settleOne(t, e, m, paid, want)
			}
			checkAgree(t, e, m)
		}
		for who, amount := range want {
			if paid[who] != amount {
				t.Fatalf("%s was paid %d; the reference pays %d", who, paid[who], amount)
			}
		}
	})
}

func settleOne(t *rapid.T, e *engine, m *model, paid, want map[string]int64) {
	if len(m.units) == 0 {
		return
	}
	u := m.units[rapid.IntRange(0, len(m.units)-1).Draw(t, "unit")]
	due := u.value - u.blocked
	amount := due
	if rapid.IntRange(0, 3).Draw(t, "short") == 0 {
		amount = rapid.Int64Range(0, due).Draw(t, "amount")
	}
	payments, err := e.Settle(u.key, amount)
	if u.settled {
		if !errors.Is(err, ErrSettled) {
			t.Fatalf("settling %v twice: %v", u.key, err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	var sum, kept int64
	for _, p := range payments {
		if p.Amount <= 0 {
			t.Fatalf("a payment of %d", p.Amount)
		}
		switch {
		case p.Contract != "":
			paid[p.Contract] += p.Amount
			sum += p.Amount
		case p.To == u.key.Accreditor:
			paid["accreditor"] += p.Amount
			kept += p.Amount
		default:
			paid["holder"] += p.Amount
			sum += p.Amount
		}
	}
	if sum != amount || kept != u.blocked {
		t.Fatalf("settling %v for %d paid %d and kept %d of a %d block", u.key, amount, sum, kept, u.blocked)
	}
	for who, amount := range m.settle(u.key, amount) {
		want[who] += amount
	}
}

func checkAgree(t *rapid.T, e *engine, m *model) {
	for _, mu := range m.units {
		u := e.units[mu.key]
		m.grow(mu)
		if u.value != mu.value || u.blocked != mu.blocked || u.settled != mu.settled {
			t.Fatalf("%v: engine %d/%d settled %t, model %d/%d settled %t", mu.key, u.value, u.blocked, u.settled, mu.value, mu.blocked, mu.settled)
		}
		if u.free() < 0 || u.blocked < 0 {
			t.Fatalf("%v: free %d, blocked %d", mu.key, u.free(), u.blocked)
		}
		for ci, mc := range m.contracts {
			if u.committed[mc.c.ID] != mu.commits[ci] || mu.commits[ci] < 0 {
				t.Fatalf("%v: contract %s holds %d; the model says %d", mu.key, mc.c.ID, u.committed[mc.c.ID], mu.commits[ci])
			}
		}
	}
	for _, mc := range m.contracts {
		c := e.contracts[mc.c.ID]
		if c.paid != mc.paid {
			t.Fatalf("contract %s paid %d; the model says %d", c.ID, c.paid, mc.paid)
		}
		if c.Rule == Fixed {
			held := c.paid
			for _, u := range e.units {
				if !u.settled {
					held += u.committed[c.ID]
				}
			}
			if held > c.Amount {
				t.Fatalf("contract %s holds and was paid %d of %d", c.ID, held, c.Amount)
			}
		}
	}
}

func TestWaterfallExample(t *testing.T) {
	e := newEngine()
	k := UnitKey{"33333333000191", holders[0], "VCC", "2026-11-02"}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(e.SetUnit(k, 10_000, 0, Domicile{}))
	must(e.Accept(Contract{ID: "first", Beneficiary: "bank A", Holder: holders[0], Effect: Lien, Rule: Fixed, Amount: 4_000}))
	must(e.Accept(Contract{ID: "second", Beneficiary: "bank B", Holder: holders[0], Effect: OwnershipTransfer, Rule: Percentage, BasisPoints: 5_000}))
	// first 4000, second half of 10000 = 5000, free 1000.
	if p := e.position(e.units[k]); p.Free != 1_000 || p.Committed[0].Amount != 4_000 || p.Committed[1].Amount != 5_000 {
		t.Fatalf("after two contracts: %+v", p)
	}
	if err := e.SetUnit(k, 10_000, 2_000, Domicile{}); !errors.Is(err, ErrBlock) {
		t.Fatalf("a block beyond the free amount: %v", err)
	}
	must(e.SetUnit(k, 10_000, 500, Domicile{}))
	// A chargeback of 3000: the 500 free, then the second contract (the latest) 2500.
	must(e.SetUnit(k, 7_000, 500, Domicile{}))
	if p := e.position(e.units[k]); p.Free != 0 || p.Committed[0].Amount != 4_000 || p.Committed[1].Amount != 2_500 {
		t.Fatalf("after the reduction: %+v", p)
	}
	// Settling short: the first contract is paid in full before the second.
	payments, err := e.Settle(k, 5_000)
	must(err)
	if len(payments) != 3 || payments[0].Amount != 4_000 || payments[1].Amount != 1_000 || payments[2].Amount != 500 || payments[2].To != k.Accreditor {
		t.Fatalf("payments: %+v", payments)
	}
	if _, err := e.Settle(k, 1); !errors.Is(err, ErrSettled) {
		t.Fatalf("settling twice: %v", err)
	}
}

// Worked by hand from the Convenção's rules, independently of the engine and the model:
// first accepted is paid first (3.13), a reduction takes the free amount and then the
// latest contract (3.14), a fixed contract extends to units registered later, and a
// refused block changes nothing.
func TestConvencaoByHand(t *testing.T) {
	const acc = "33333333000191"
	h := holders[0]
	nov, dec := UnitKey{acc, h, "VCC", "2026-11-02"}, UnitKey{acc, h, "VCC", "2026-12-01"}
	e := newEngine()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(e.SetUnit(nov, 1_000, 0, Domicile{}))
	must(e.Accept(Contract{ID: "A", Beneficiary: "bank A", Holder: h, Effect: Lien, Rule: Fixed, Amount: 1_500}))
	must(e.Accept(Contract{ID: "B", Beneficiary: "bank B", Holder: h, Effect: OwnershipTransfer, Rule: Fixed, Amount: 300}))
	// A takes all of November's 1000 and waits for 500 more; B gets nothing yet.
	if p := e.position(e.units[nov]); p.Free != 0 || len(p.Committed) != 1 || p.Committed[0].Amount != 1_000 {
		t.Fatalf("November: %+v", p)
	}
	// December is registered later: A takes its 500 first, then B its 300, 200 free.
	must(e.SetUnit(dec, 1_000, 0, Domicile{}))
	if p := e.position(e.units[dec]); p.Free != 200 || p.Committed[0].Amount != 500 || p.Committed[1].Amount != 300 {
		t.Fatalf("December: %+v", p)
	}
	// A block of 300 on December is more than its 200 free: refused, nothing changes.
	if err := e.SetUnit(dec, 1_000, 300, Domicile{}); !errors.Is(err, ErrBlock) {
		t.Fatalf("a block beyond the free amount: %v", err)
	}
	// A chargeback of 400 on December: its 200 free, then 200 of B, the latest.
	must(e.SetUnit(dec, 600, 0, Domicile{}))
	if p := e.position(e.units[dec]); p.Free != 0 || p.Committed[0].Amount != 500 || p.Committed[1].Amount != 100 {
		t.Fatalf("December after the chargeback: %+v", p)
	}
	// November settles short, for 700: A is owed 1000 there, and is paid all 700.
	paid, err := e.Settle(nov, 700)
	must(err)
	if len(paid) != 1 || paid[0].Contract != "A" || paid[0].Amount != 700 {
		t.Fatalf("November's settlement: %+v", paid)
	}
	// A still lacks 300 of its 1500, which December has no free amount to give.
	if p := e.position(e.units[dec]); p.Committed[0].Amount != 500 {
		t.Fatalf("December after November settled short: %+v", p)
	}
}
