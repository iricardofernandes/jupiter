# 0003. Money as `int64` minor units with an explicit currency

- Status: Accepted
- Date: 2026-09-30

## Context

Floating point cannot represent R$ 0.10 exactly, so sums drift. Decimal types avoid that
but invite a subtler failure: an amount with more precision than its currency, such as
R$ 10.005, that has to be rounded somewhere nobody decided. Money is also always *of* a
currency, and adding reais to dollars must be impossible rather than merely unlikely.

Two operations lose precision by nature. A fee of 2.49% on R$ 100.01 is R$ 2.490249,
and dividing R$ 100.00 among three recipients leaves a centavo over. Every mature ledger
writeup handles both explicitly
([research: engineering writeups](../research/notes/06-engineering-writeups.md)).

## Decision

`internal/money` defines:

- **`Amount`**: an `int64` count of the currency's minor unit (centavos for BRL) and a
  `Currency`. It is an immutable value. Arithmetic across currencies returns
  `ErrCurrencyMismatch`; a result outside `int64` returns `ErrOverflow`, never a wrapped
  value. The zero value has no currency and every operation rejects it.
- **`Rate`**: an exact decimal multiplier parsed from a string such as `"0.0249"`, held as
  a rational number, so a multiplication loses nothing until it is rounded.
- **`RoundingMode`**: `HalfEven`, `HalfUp`, `HalfDown`, `Down`, `Up`, `Floor`, `Ceiling`.
  `Amount.MulRate` requires one; the zero value is not a mode, so a caller cannot round by
  omission.
- **`Amount.Allocate`**: splits by the largest-remainder method. Shares always sum to the
  amount, and a zero weight always receives zero. Property tests check both over random
  amounts and weights.
- **`Parse` and `Decimal`**: the decimal text form wire protocols use, such as Pix's
  `"600.00"`. Parsing rejects more decimal places than the currency has instead of
  rounding them.

`int64` minor units hold ±92 quadrillion reais, far beyond any single amount Jupiter will
see. A sum that could exceed it fails loudly.

## Alternatives rejected

- **`float64`.** Inexact by construction.
- **A decimal type for amounts** (`shopspring/decimal`, `cockroachdb/apd`,
  `govalues/decimal`). Exact, but it lets an amount carry precision its currency does not
  have, and costs an allocation per operation in the ledger's hottest path. Decimals stay
  where they belong: rates.
- **`math/big` integers for amounts.** No overflow at all, but every amount is a pointer
  to mutable state, and PostgreSQL's `bigint` would still be the storage limit.
- **Leaving rounding to each caller.** Rounding rules differ by context (banker's rounding
  for interest, truncation for some fees), so the mode must be chosen, but it must always
  be chosen visibly.

## Consequences

- Storage is `bigint` plus a three-letter currency code.
- Present-value calculations for anticipation (phase 10) need fractional exponents, which
  `Rate` does not provide. That phase adds them, and records the choice, when it has real
  formulas to test against.
- The currency list is short (BRL, USD, EUR, JPY). JPY is there so that nothing silently
  assumes two decimal places.
