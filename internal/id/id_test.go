package id_test

import (
	"errors"
	"strings"
	"testing"
	"time"
	"uuid"

	"pgregory.net/rapid"

	"github.com/iricardofernandes/jupiter/internal/id"
)

var paymentIntent = id.MustPrefix("pi")

func TestNewPrefix(t *testing.T) {
	for _, p := range []string{"pi", "txn", "whsec", "abcdefghijklmnop"} {
		if _, err := id.NewPrefix(p); err != nil {
			t.Errorf("NewPrefix(%q): %v", p, err)
		}
	}
	for _, p := range []string{"", "Pi", "p_i", "pi1", "pi-", "abcdefghijklmnopq", "é"} {
		if _, err := id.NewPrefix(p); !errors.Is(err, id.ErrInvalidPrefix) {
			t.Errorf("NewPrefix(%q) error = %v, want ErrInvalidPrefix", p, err)
		}
	}
}

func TestMustPrefixPanicsOnInvalidPrefix(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("MustPrefix did not panic")
		}
	}()
	id.MustPrefix("Bad")
}

func TestNewHasPrefixAndLength(t *testing.T) {
	s := paymentIntent.New().String()
	if !strings.HasPrefix(s, "pi_") || len(s) != len("pi_")+26 {
		t.Fatalf("New() = %q, want pi_ followed by 26 characters", s)
	}
}

// The suffix encodes a UUID as 26 Crockford base32 characters, as the TypeID
// specification does; these vectors follow from that encoding.
func TestEncodingVectors(t *testing.T) {
	tests := []struct {
		uuid, suffix string
	}{
		{"00000000-0000-0000-0000-000000000000", "00000000000000000000000000"},
		{"00000000-0000-0000-0000-000000000001", "00000000000000000000000001"},
		{"00000000-0000-0000-0000-00000000000a", "0000000000000000000000000a"},
		{"00000000-0000-0000-0000-000000000010", "0000000000000000000000000g"},
		{"00000000-0000-0000-0000-000000000020", "00000000000000000000000010"},
		{"ffffffff-ffff-ffff-ffff-ffffffffffff", "7zzzzzzzzzzzzzzzzzzzzzzzzz"},
		// "valid-alphabet" from the TypeID specification's spec/valid.yml.
		{"0110c853-1d09-52d8-d73e-1194e95b5f19", "0123456789abcdefghjkmnpqrs"},
	}
	for _, tt := range tests {
		u := uuid.MustParse(tt.uuid)
		got := paymentIntent.FromUUID(u).String()
		if want := "pi_" + tt.suffix; got != want {
			t.Errorf("FromUUID(%s) = %q, want %q", tt.uuid, got, want)
		}
		parsed, err := id.Parse("pi_" + tt.suffix)
		if err != nil || parsed.UUID() != u {
			t.Errorf("Parse(pi_%s) = %v, %v; want %s", tt.suffix, parsed.UUID(), err, tt.uuid)
		}
	}
}

func TestParse(t *testing.T) {
	original := paymentIntent.New()
	parsed, err := id.Parse(original.String())
	if err != nil || parsed != original {
		t.Fatalf("Parse(%q) = %v, %v", original, parsed, err)
	}
	if parsed.Prefix() != paymentIntent {
		t.Fatalf("Prefix() = %q, want pi", parsed.Prefix())
	}

	invalid := []string{
		"",
		"pi",
		"pi_",
		"_01h2xcejqtf2nbrexx3vqjhp41",
		"pi_01h2xcejqtf2nbrexx3vqjhp4",   // 25 characters
		"pi_01h2xcejqtf2nbrexx3vqjhp411", // 27 characters
		"pi_81h2xcejqtf2nbrexx3vqjhp41",  // first character above 7 overflows 128 bits
		"pi_01H2XCEJQTF2NBREXX3VQJHP41",  // upper case
		"pi_01h2xcejqtf2nbrexx3vqjhpu1",  // 'u' is not in the alphabet
		"PI_01h2xcejqtf2nbrexx3vqjhp41",
		"p-i_01h2xcejqtf2nbrexx3vqjhp41",
	}
	for _, s := range invalid {
		if _, err := id.Parse(s); !errors.Is(err, id.ErrInvalid) {
			t.Errorf("Parse(%q) error = %v, want ErrInvalid", s, err)
		}
	}
}

func TestPrefixParseRejectsOtherPrefixes(t *testing.T) {
	refund := id.MustPrefix("re")
	other := refund.New()
	if _, err := paymentIntent.Parse(other.String()); !errors.Is(err, id.ErrInvalid) {
		t.Fatalf("pi.Parse(%q) error = %v, want ErrInvalid", other, err)
	}
	if _, err := refund.Parse(other.String()); err != nil {
		t.Fatalf("re.Parse(%q): %v", other, err)
	}
}

func TestTime(t *testing.T) {
	before := time.Now().Truncate(time.Millisecond)
	got := paymentIntent.New().Time()
	after := time.Now()
	if got.Before(before) || got.After(after) {
		t.Fatalf("Time() = %v, want between %v and %v", got, before, after)
	}
}

func TestTextMarshaling(t *testing.T) {
	original := paymentIntent.New()
	text, err := original.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText: %v", err)
	}
	var decoded id.ID
	if err := decoded.UnmarshalText(text); err != nil || decoded != original {
		t.Fatalf("UnmarshalText(%q) = %v, %v", text, decoded, err)
	}
	if err := decoded.UnmarshalText([]byte("nope")); !errors.Is(err, id.ErrInvalid) {
		t.Fatalf("UnmarshalText(nope) error = %v, want ErrInvalid", err)
	}
}

func TestZeroValue(t *testing.T) {
	var zero id.ID
	if !zero.IsZero() || zero.String() != "" {
		t.Fatalf("zero ID: IsZero=%t String=%q", zero.IsZero(), zero.String())
	}
	if _, err := zero.MarshalText(); !errors.Is(err, id.ErrInvalid) {
		t.Fatalf("MarshalText on zero ID error = %v, want ErrInvalid", err)
	}
	if paymentIntent.New().IsZero() {
		t.Fatal("new ID reports IsZero")
	}
}

// IDs generated later sort after earlier ones as plain strings, which is what keeps
// database indexes append-mostly and cursor pagination stable.
func TestPropertyGenerationOrderIsStringOrder(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(2, 500).Draw(t, "n")
		previous := paymentIntent.New().String()
		for range n {
			next := paymentIntent.New().String()
			if next <= previous {
				t.Fatalf("%q generated after %q but does not sort after it", next, previous)
			}
			previous = next
		}
	})
}

// Every UUID round-trips through its string form.
func TestPropertyRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		var u uuid.UUID
		copy(u[:], rapid.SliceOfN(rapid.Byte(), 16, 16).Draw(t, "bytes"))
		original := paymentIntent.FromUUID(u)
		parsed, err := paymentIntent.Parse(original.String())
		if err != nil || parsed != original {
			t.Fatalf("Parse(%q) = %v, %v", original, parsed, err)
		}
	})
}
