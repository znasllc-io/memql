// Package money is exact decimal arithmetic for amounts of money.
//
// An Amount is an unscaled big integer and a scale: 42.00 is 4200 at scale 2.
// Nothing here goes through a float, and nothing overflows: a sum of a store's
// sales is exact however many there are. The package is math/big and the
// standard library only.
//
// The one rounding rule is half to even, applied only by MulRatio, at a scale
// the caller passes in (see RoundingScale). Add, Sub and Mul are exact.
package money

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
	"unicode/utf8"
)

// maxQuoted is the most bytes of a bad input that an error from Parse quotes.
const maxQuoted = 32

// Amount is a decimal amount. The zero value is 0 at scale 0 and is usable.
// An Amount is immutable: no method changes its receiver or its operands, and
// no result shares storage with them.
type Amount struct {
	unscaled *big.Int // nil is 0
	scale    int      // the number of decimals, never negative
}

// Parse reads an amount written as -?digits[.digits] with ASCII digits and
// nothing else: no sign but the minus, no space, no exponent, no grouping, no
// bare point. Leading zeros of the whole part are dropped; the decimals written
// are the scale, so "35.0" is scale 1. The error quotes at most 32 bytes of
// the input.
func Parse(s string) (Amount, error) {
	rest := s
	neg := strings.HasPrefix(rest, "-")
	if neg {
		rest = rest[1:]
	}
	whole, frac, hasPoint := strings.Cut(rest, ".")
	if !allDigits(whole) || (hasPoint && !allDigits(frac)) {
		return Amount{}, notAnAmount(s)
	}
	u, ok := new(big.Int).SetString(whole+frac, 10)
	if !ok {
		return Amount{}, notAnAmount(s)
	}
	if neg {
		u.Neg(u)
	}
	return Amount{unscaled: u, scale: len(frac)}, nil
}

// allDigits reports whether s is one or more ASCII digits.
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// notAnAmount names the input without echoing an unbounded one: at most
// maxQuoted bytes, cut back to a whole character so the message stays valid
// UTF-8.
func notAnAmount(s string) error {
	quoted, more := s, ""
	if len(s) > maxQuoted {
		cut := maxQuoted
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		quoted, more = s[:cut], "..."
	}
	return fmt.Errorf("money: %q%s is not an amount (want digits with an optional minus and decimals)", quoted, more)
}

// Add is the exact sum, at the wider of the two scales.
func (a Amount) Add(b Amount) Amount {
	scale := max(a.scale, b.scale)
	return Amount{unscaled: new(big.Int).Add(a.at(scale), b.at(scale)), scale: scale}
}

// Sub is the exact difference, at the wider of the two scales.
func (a Amount) Sub(b Amount) Amount {
	scale := max(a.scale, b.scale)
	return Amount{unscaled: new(big.Int).Sub(a.at(scale), b.at(scale)), scale: scale}
}

// Mul is the exact product with a whole number, at a's scale.
func (a Amount) Mul(n int64) Amount {
	return Amount{unscaled: new(big.Int).Mul(a.digits(), big.NewInt(n)), scale: a.scale}
}

// MulRatio is a * num / den rounded half to even at scale decimals, so a tie
// goes to the even last digit. The sign of num and of den is ordinary
// arithmetic. A zero den and a scale below zero are the only errors.
//
// The caller chooses the scale, and RoundingScale is how: the written scale of
// an amount alone can be too coarse for its currency.
func (a Amount) MulRatio(num, den int64, scale int) (Amount, error) {
	if den == 0 {
		return Amount{}, errors.New("money: a ratio with a zero denominator")
	}
	if scale < 0 {
		return Amount{}, fmt.Errorf("money: a negative scale, %d", scale)
	}
	// a = u / 10^a.scale, so the result at scale is
	// u * num * 10^scale / (den * 10^a.scale), as one fraction of big integers.
	n := new(big.Int).Mul(a.digits(), big.NewInt(num))
	d := big.NewInt(den)
	if scale >= a.scale {
		n.Mul(n, pow10(scale-a.scale))
	} else {
		d.Mul(d, pow10(a.scale-scale))
	}
	neg := n.Sign() < 0 != (d.Sign() < 0)
	n.Abs(n)
	d.Abs(d)
	q, r := n.QuoRem(n, d, new(big.Int))
	switch c := r.Lsh(r, 1).Cmp(d); {
	case c > 0, c == 0 && q.Bit(0) == 1:
		q.Add(q, big.NewInt(1))
	}
	if neg {
		q.Neg(q)
	}
	return Amount{unscaled: q, scale: scale}, nil
}

// Scale is the number of decimals the amount keeps.
func (a Amount) Scale() int { return a.scale }

// Sign is -1, 0 or 1. A negative zero does not exist: it is 0.
func (a Amount) Sign() int { return a.digits().Sign() }

// Equal reports whether a and b are the same value at whatever scales:
// "36.0" equals "36.00".
func (a Amount) Equal(b Amount) bool {
	scale := max(a.scale, b.scale)
	return a.at(scale).Cmp(b.at(scale)) == 0
}

// String prints every digit of the scale, so "42.00" stays "42.00". A negative
// amount smaller than one prints its zero and its sign ("-0.10"); zero prints
// without a sign.
func (a Amount) String() string {
	u := a.digits()
	digits := new(big.Int).Abs(u).String()
	if len(digits) <= a.scale {
		digits = strings.Repeat("0", a.scale-len(digits)+1) + digits
	}
	if a.scale > 0 {
		digits = digits[:len(digits)-a.scale] + "." + digits[len(digits)-a.scale:]
	}
	if u.Sign() < 0 {
		return "-" + digits
	}
	return digits
}

// digits is the unscaled integer; the zero value's is 0. The caller must not
// change it.
func (a Amount) digits() *big.Int {
	if a.unscaled == nil {
		return new(big.Int)
	}
	return a.unscaled
}

// at is the unscaled integer rewritten at a scale at least a's own, as a new
// integer when it has to change and otherwise the shared one, to be read only.
func (a Amount) at(scale int) *big.Int {
	if scale == a.scale {
		return a.digits()
	}
	return new(big.Int).Mul(a.digits(), pow10(scale-a.scale))
}

// pow10 is 10^n for n >= 0, a new integer every time.
func pow10(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}
