package id

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"uuid"
)

var (
	ErrInvalidPrefix = errors.New("id: invalid prefix")
	ErrInvalid       = errors.New("id: invalid identifier")
)

const (
	maxPrefixLen = 16
	suffixLen    = 26
	separator    = "_"
	alphabet     = "0123456789abcdefghjkmnpqrstvwxyz"
)

type Prefix string

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

func MustPrefix(p string) Prefix {
	prefix, err := NewPrefix(p)
	if err != nil {
		panic(err)
	}
	return prefix
}

func (p Prefix) New() ID {
	return ID{prefix: p, uuid: uuid.NewV7()}
}

func (p Prefix) FromUUID(u uuid.UUID) ID {
	return ID{prefix: p, uuid: u}
}

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

type ID struct {
	prefix Prefix
	uuid   uuid.UUID
}

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

func (i ID) String() string {
	if i.IsZero() {
		return ""
	}
	return string(i.prefix) + separator + encode(i.uuid)
}

func (i ID) Prefix() Prefix { return i.prefix }

func (i ID) UUID() uuid.UUID { return i.uuid }

func (i ID) Time() time.Time {
	var ms int64
	for _, b := range i.uuid[:6] {
		ms = ms<<8 | int64(b)
	}
	return time.UnixMilli(ms)
}

func (i ID) IsZero() bool { return i.prefix == "" }

// The zero value is an error so that an unset identifier never serializes as "".
func (i ID) MarshalText() ([]byte, error) {
	if i.IsZero() {
		return nil, fmt.Errorf("%w: zero value", ErrInvalid)
	}
	return []byte(i.String()), nil
}

func (i *ID) UnmarshalText(text []byte) error {
	parsed, err := Parse(string(text))
	if err != nil {
		return err
	}
	*i = parsed
	return nil
}

// 26 characters hold 130 bits; the two leading bits are always zero.
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
