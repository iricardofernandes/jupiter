// Package id generates and parses Jupiter's object identifiers, such as
// "pi_01k6c0q9a8f3v7w2x5y4z6b1cd".
//
// An identifier is a type prefix, an underscore and a UUIDv7 encoded as 26 lower-case
// Crockford base32 characters, the encoding of the TypeID specification. The prefix makes
// an identifier self-describing in logs, support tickets and API errors; the UUIDv7 makes
// identifiers sort in creation order, as strings and as bytes, which keeps B-tree
// indexes append-mostly and cursor pagination stable.
package id

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"uuid"
)

var (
	// ErrInvalidPrefix is returned for a prefix that is not 1 to 16 lower-case ASCII
	// letters.
	ErrInvalidPrefix = errors.New("id: invalid prefix")
	// ErrInvalid is returned for a string that is not a well-formed identifier, or that
	// has a different prefix than the one expected.
	ErrInvalid = errors.New("id: invalid identifier")
)

const (
	maxPrefixLen = 16
	suffixLen    = 26
	separator    = "_"
	// The Crockford base32 alphabet, lower case: no i, l, o or u.
	alphabet = "0123456789abcdefghjkmnpqrstvwxyz"
)

// Prefix is a validated type prefix such as "pi" or "txn". Modules declare their prefixes
// once, as package-level variables built with MustPrefix.
type Prefix string

// NewPrefix validates p as a prefix.
func NewPrefix(p string) (Prefix, error) {
	if p == "" || len(p) > maxPrefixLen {
		return "", fmt.Errorf("%w: %q", ErrInvalidPrefix, p)
	}
	for i := range len(p) {
		if p[i] < 'a' || p[i] > 'z' {
			return "", fmt.Errorf("%w: %q", ErrInvalidPrefix, p)
		}
	}
	return Prefix(p), nil
}

// MustPrefix is NewPrefix for package-level declarations; it panics on an invalid prefix.
func MustPrefix(p string) Prefix {
	prefix, err := NewPrefix(p)
	if err != nil {
		panic(err)
	}
	return prefix
}

// New returns a new identifier with this prefix. Identifiers from one process are
// strictly increasing unless the system clock moves backwards.
func (p Prefix) New() ID {
	return ID{prefix: p, uuid: uuid.NewV7()}
}

// FromUUID returns the identifier with this prefix for u. It exists for deterministic
// tests and simulations; production code uses New.
func (p Prefix) FromUUID(u uuid.UUID) ID {
	return ID{prefix: p, uuid: u}
}

// Parse parses s and checks that its prefix is p.
func (p Prefix) Parse(s string) (ID, error) {
	parsed, err := Parse(s)
	if err != nil {
		return ID{}, err
	}
	if parsed.prefix != p {
		return ID{}, fmt.Errorf("%w: %q does not have prefix %q", ErrInvalid, s, string(p))
	}
	return parsed, nil
}

// ID is a prefixed, time-ordered identifier. It is a comparable value; the zero value
// is not a valid identifier.
type ID struct {
	prefix Prefix
	uuid   uuid.UUID
}

// Parse parses any well-formed identifier. Use Prefix.Parse to also check its type.
func Parse(s string) (ID, error) {
	prefix, suffix, found := strings.Cut(s, separator)
	if !found {
		return ID{}, fmt.Errorf("%w: %q has no prefix", ErrInvalid, s)
	}
	p, err := NewPrefix(prefix)
	if err != nil {
		return ID{}, fmt.Errorf("%w: %q: %w", ErrInvalid, s, err)
	}
	u, err := decode(suffix)
	if err != nil {
		return ID{}, fmt.Errorf("%w: %q: %w", ErrInvalid, s, err)
	}
	return ID{prefix: p, uuid: u}, nil
}

// String returns the identifier's text form, or "" for the zero value.
func (i ID) String() string {
	if i.IsZero() {
		return ""
	}
	return string(i.prefix) + separator + encode(i.uuid)
}

// Prefix returns the identifier's type prefix.
func (i ID) Prefix() Prefix { return i.prefix }

// UUID returns the UUID the identifier encodes.
func (i ID) UUID() uuid.UUID { return i.uuid }

// Time returns the creation time recorded in a UUIDv7, at millisecond precision.
func (i ID) Time() time.Time {
	var ms int64
	for _, b := range i.uuid[:6] {
		ms = ms<<8 | int64(b)
	}
	return time.UnixMilli(ms)
}

// IsZero reports whether i is the zero value.
func (i ID) IsZero() bool { return i.prefix == "" }

// MarshalText implements encoding.TextMarshaler. The zero value is an error, so an
// unset identifier is never serialized as an empty string by accident.
func (i ID) MarshalText() ([]byte, error) {
	if i.IsZero() {
		return nil, fmt.Errorf("%w: zero value", ErrInvalid)
	}
	return []byte(i.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (i *ID) UnmarshalText(text []byte) error {
	parsed, err := Parse(string(text))
	if err != nil {
		return err
	}
	*i = parsed
	return nil
}

// encode writes the 128 bits of u as 26 base32 characters, most significant first. The
// 130 bits of output carry two leading zero bits.
func encode(u uuid.UUID) string {
	var out [suffixLen]byte
	var hi, lo uint64
	for _, b := range u[:8] {
		hi = hi<<8 | uint64(b)
	}
	for _, b := range u[8:] {
		lo = lo<<8 | uint64(b)
	}
	for i := suffixLen - 1; i >= 0; i-- {
		out[i] = alphabet[lo&0x1f]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return string(out[:])
}

// decode reverses encode, rejecting any string encode could not have produced.
func decode(s string) (uuid.UUID, error) {
	if len(s) != suffixLen {
		return uuid.UUID{}, fmt.Errorf("suffix has %d characters, want %d", len(s), suffixLen)
	}
	var hi, lo uint64
	for i := range len(s) {
		v := strings.IndexByte(alphabet, s[i])
		if v < 0 {
			return uuid.UUID{}, fmt.Errorf("character %q is not in the base32 alphabet", s[i])
		}
		if i == 0 && v > 7 {
			return uuid.UUID{}, errors.New("suffix overflows 128 bits")
		}
		hi = hi<<5 | lo>>59
		lo = lo<<5 | uint64(v)
	}
	var u uuid.UUID
	for i := 7; i >= 0; i-- {
		u[i] = byte(hi)
		hi >>= 8
		u[i+8] = byte(lo)
		lo >>= 8
	}
	return u, nil
}
