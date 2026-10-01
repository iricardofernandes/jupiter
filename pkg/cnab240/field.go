package cnab240

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// LineLength is the length of every CNAB 240 record.
const LineLength = 240

// line writes one record. Positions are FEBRABAN's: 1-based and inclusive. Numbers are
// zero-padded on the left, text space-padded on the right, dates DDMMAAAA; a value that
// does not fit is an error, never cut.
type line struct {
	b   [LineLength]byte
	err error
}

func newLine() *line {
	l := &line{}
	for i := range l.b {
		l.b[i] = ' '
	}
	return l
}

func (l *line) fail(start, end int, format string, args ...any) {
	if l.err == nil {
		l.err = fmt.Errorf("%w: positions %d-%d: %s", ErrInvalid, start, end, fmt.Sprintf(format, args...))
	}
}

// num writes a non-negative number.
func (l *line) num(start, end int, v int64) {
	if v < 0 {
		l.fail(start, end, "negative number %d", v)
		return
	}
	l.code(start, end, strconv.FormatInt(v, 10))
}

// code writes a code of digits or capital letters, right-aligned and zero-padded: a CPF,
// an alphanumeric CNPJ, an account.
func (l *line) code(start, end int, s string) {
	width := end - start + 1
	if len(s) > width {
		l.fail(start, end, "%q is longer than %d", s, width)
		return
	}
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'A' || c > 'Z') {
			l.fail(start, end, "%q is not digits and capital letters", s)
			return
		}
	}
	copy(l.b[start-1:end], strings.Repeat("0", width-len(s))+s)
}

// text writes printable ASCII, left-aligned and space-padded.
func (l *line) text(start, end int, s string) {
	width := end - start + 1
	if len(s) > width {
		l.fail(start, end, "%q is longer than %d", s, width)
		return
	}
	for i := range len(s) {
		if s[i] < ' ' || s[i] > '~' {
			l.fail(start, end, "%q is not printable ASCII", s)
			return
		}
	}
	copy(l.b[start-1:end], s+strings.Repeat(" ", width-len(s)))
}

// date writes a date as DDMMAAAA; a zero date as zeros.
func (l *line) date(start, end int, t time.Time) {
	if t.IsZero() {
		l.code(start, end, "0")
		return
	}
	l.text(start, end, t.Format("02012006"))
}

func (l *line) String() string { return string(l.b[:]) }

// reader reads one record: what the line writes, it reads back as written.
type reader struct {
	s   string
	err error
}

func (r *reader) fail(start, end int, format string, args ...any) {
	if r.err == nil {
		r.err = fmt.Errorf("%w: positions %d-%d: %s", ErrInvalid, start, end, fmt.Sprintf(format, args...))
	}
}

func (r *reader) raw(start, end int) string { return r.s[start-1 : end] }

func (r *reader) num(start, end int) int64 {
	s := r.raw(start, end)
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v < 0 || strings.TrimLeft(s, "0123456789") != "" {
		r.fail(start, end, "%q is not a number", s)
		return 0
	}
	return v
}

// code reads a code as written, leading zeros and all.
func (r *reader) code(start, end int) string {
	s := r.raw(start, end)
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'A' || c > 'Z') {
			r.fail(start, end, "%q is not digits and capital letters", s)
			return ""
		}
	}
	return s
}

func (r *reader) text(start, end int) string {
	s := r.raw(start, end)
	for i := range len(s) {
		if s[i] < ' ' || s[i] > '~' {
			r.fail(start, end, "%q is not printable ASCII", s)
			return ""
		}
	}
	return strings.TrimRight(s, " ")
}

func (r *reader) date(start, end int) time.Time {
	s := r.raw(start, end)
	if strings.Trim(s, "0") == "" {
		return time.Time{}
	}
	t, err := time.Parse("02012006", s)
	if err != nil {
		r.fail(start, end, "%q is not a date", s)
	}
	return t
}

// blank checks a filler is spaces.
func (r *reader) blank(start, end int) {
	if s := r.raw(start, end); strings.Trim(s, " ") != "" {
		r.fail(start, end, "%q should be blank", s)
	}
}

// literal checks a fixed value.
func (r *reader) literal(start, end int, want string) {
	if s := r.raw(start, end); s != want {
		r.fail(start, end, "%q, want %q", s, want)
	}
}
