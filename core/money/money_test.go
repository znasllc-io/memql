package money

import (
	"math"
	"math/big"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func mustParse(t testing.TB, s string) Amount {
	t.Helper()
	a, err := Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q): %v", s, err)
	}
	return a
}

// ratOf reads an amount's printed value as an exact rational, so a result is
// compared with arithmetic that shares no code with the package.
func ratOf(t testing.TB, a Amount) *big.Rat {
	t.Helper()
	r, ok := new(big.Rat).SetString(a.String())
	if !ok {
		t.Fatalf("amount prints %q, which is not a number", a.String())
	}
	return r
}

// roundHalfEven is the reference for MulRatio: r rounded to scale decimals,
// ties to the even last digit, returned as the exact rational it denotes.
func roundHalfEven(r *big.Rat, scale int) *big.Rat {
	scaled := new(big.Rat).Mul(r, new(big.Rat).SetInt(pow10(scale)))
	num := new(big.Int).Set(scaled.Num())
	den := scaled.Denom()
	neg := num.Sign() < 0
	num.Abs(num)
	q, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	twice := rem.Lsh(rem, 1)
	switch c := twice.Cmp(den); {
	case c > 0, c == 0 && q.Bit(0) == 1:
		q.Add(q, big.NewInt(1))
	}
	if neg {
		q.Neg(q)
	}
	return new(big.Rat).SetFrac(q, pow10(scale))
}

func TestParseAcceptsOnlyPlainDecimals(t *testing.T) {
	for _, ok := range []string{"0", "42", "42.00", "-0.10", "007.5", "1200", "1.250", "35.0"} {
		if _, err := Parse(ok); err != nil {
			t.Errorf("Parse(%q): %v", ok, err)
		}
	}
	// the last is two Arabic-Indic digits: only ASCII digits are an amount
	for _, bad := range []string{"", " 1", "1 ", "1e3", "1E3", ".5", "5.", "+5", "1,000", "NaN", "Inf", "0x10", "1.2.3", "-", "--1", "١٢", "1_000", "1\n", "-.5", "1.-5", "0b1", "\x00"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) was accepted", bad)
		}
	}
}

func TestParseThenStringIsTheInputInItsCanonicalSpelling(t *testing.T) {
	for _, s := range []string{"0", "42", "42.00", "-0.10", "1200", "1.250", "35.0", "0.005", "-0.005", "0.00", "10", "100.000", "-7", "123456789012345678901234567890.123456789"} {
		if got := mustParse(t, s).String(); got != s {
			t.Errorf("Parse(%q).String() = %q", s, got)
		}
	}
	// leading zeros of the whole part are dropped, the decimals are not
	for in, want := range map[string]string{"007.5": "7.5", "000": "0", "-007": "-7", "00.50": "0.50", "-000.10": "-0.10", "0": "0"} {
		if got := mustParse(t, in).String(); got != want {
			t.Errorf("Parse(%q).String() = %q, want %q", in, got, want)
		}
	}
	// a negative zero prints without its sign, at its own scale
	for in, want := range map[string]string{"-0.00": "0.00", "-0": "0", "-0.000": "0.000", "-000.0": "0.0"} {
		a := mustParse(t, in)
		if got := a.String(); got != want {
			t.Errorf("Parse(%q).String() = %q, want %q", in, got, want)
		}
		if a.Sign() != 0 {
			t.Errorf("Parse(%q).Sign() = %d, want 0", in, a.Sign())
		}
	}
}

func TestSignAndScaleOfParsedAmounts(t *testing.T) {
	for _, c := range []struct {
		in    string
		sign  int
		scale int
	}{{"5", 1, 0}, {"-5", -1, 0}, {"0", 0, 0}, {"0.001", 1, 3}, {"-0.001", -1, 3}, {"35.0", 1, 1}, {"0.00", 0, 2}} {
		a := mustParse(t, c.in)
		if a.Sign() != c.sign || a.Scale() != c.scale {
			t.Errorf("%q: sign %d scale %d, want %d and %d", c.in, a.Sign(), a.Scale(), c.sign, c.scale)
		}
	}
}

func TestParseErrorQuotesAtMost32BytesOfTheInput(t *testing.T) {
	cases := map[string]string{
		"ascii":       strings.Repeat("x", 200),
		"three bytes": strings.Repeat("€", 60), // 32 is not a multiple of 3
		"two bytes":   strings.Repeat("é", 60) + "!",
		"four bytes":  strings.Repeat("\U0001F600", 40),
		"bad utf8":    strings.Repeat("\xff", 100),
		"short bad":   "\xff",
	}
	for name, in := range cases {
		_, err := Parse(in)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		msg := err.Error()
		if !utf8.ValidString(msg) {
			t.Errorf("%s: the error is not valid UTF-8: %q", name, msg)
		}
		if len(in) > 32 {
			// the input is one repeated character, so any 33-byte run of the
			// input quoted in full would show as its first 33 bytes at least
			// when the character is a single byte; for the others the longest
			// quotable prefix is checked at whole characters.
			if strings.Contains(msg, in[:33]) && name == "ascii" {
				t.Errorf("%s: the error quotes more than 32 bytes: %q", name, msg)
			}
			// quoting escapes an invalid byte as four characters (\xff), so 32 bytes
			// can print as 128; a message beyond that quotes more than 32 bytes
			if len(msg) > 4*32+80 {
				t.Errorf("%s: the error is %d bytes long, so it quotes too much: %q", name, len(msg), msg)
			}
		}
	}
	// the three-byte characters: ten fit in 32 bytes, eleven do not
	_, err := Parse(strings.Repeat("€", 60))
	if err == nil {
		t.Fatal("sixty euro signs were accepted")
	}
	if strings.Contains(err.Error(), strings.Repeat("€", 11)) {
		t.Errorf("the error quotes eleven three-byte characters, 33 bytes: %q", err.Error())
	}
	// the cut falls between characters, so no half of one is quoted as a byte escape
	if strings.Contains(err.Error(), `\x`) {
		t.Errorf("the error quotes half a character: %q", err.Error())
	}
	// a short bad input is quoted whole
	_, err = Parse("1e3")
	if err == nil || !strings.Contains(err.Error(), "1e3") {
		t.Errorf("a short input is not quoted: %v", err)
	}
}

func TestOneTenthPlusTwoTenthsIsThreeTenths(t *testing.T) {
	got := mustParse(t, "0.1").Add(mustParse(t, "0.2")).String()
	if got != "0.3" {
		t.Errorf("0.1 + 0.2 = %s", got)
	}
}

func TestASumKeepsTheWiderScale(t *testing.T) {
	for _, c := range []struct{ a, b, add, sub string }{
		{"1.5", "2.25", "3.75", "-0.75"},
		{"42.00", "1", "43.00", "41.00"},
		{"1", "42.00", "43.00", "-41.00"},
		{"5.00", "5", "10.00", "0.00"},
		{"5", "5.00", "10.00", "0.00"},
		{"-1.5", "0.25", "-1.25", "-1.75"},
		{"0.001", "0.1", "0.101", "-0.099"},
	} {
		a, b := mustParse(t, c.a), mustParse(t, c.b)
		if got := a.Add(b).String(); got != c.add {
			t.Errorf("%s + %s = %s, want %s", c.a, c.b, got, c.add)
		}
		if got := a.Sub(b).String(); got != c.sub {
			t.Errorf("%s - %s = %s, want %s", c.a, c.b, got, c.sub)
		}
	}
	if got := mustParse(t, "5.00").Sub(mustParse(t, "5")).String(); got != "0.00" {
		t.Errorf("5.00 - 5 = %s, want 0.00", got)
	}
	if got := mustParse(t, "1.5").Add(mustParse(t, "2.25")).Scale(); got != 2 {
		t.Errorf("the sum's scale is %d, want 2", got)
	}
	// the narrower operand's scale is not what the sum keeps, whichever side it is on
	if got := mustParse(t, "1.234").Add(mustParse(t, "1")).Scale(); got != 3 {
		t.Errorf("1.234 + 1 has scale %d, want 3", got)
	}
	if got := mustParse(t, "1").Add(mustParse(t, "1.234")).Scale(); got != 3 {
		t.Errorf("1 + 1.234 has scale %d, want 3", got)
	}
}

func TestMulRatioRoundsHalfToEvenAtTheScaleItIsGiven(t *testing.T) {
	for _, c := range []struct {
		a        string
		num, den int64
		scale    int
		want     string
	}{
		{"0.05", 1, 2, 2, "0.02"}, {"0.15", 1, 2, 2, "0.08"}, {"-0.05", 1, 2, 2, "-0.02"},
		{"10.00", 1, 3, 2, "3.33"}, {"10.00", 2, 3, 2, "6.67"}, {"1.00", 2, 3, 2, "0.67"},
		{"1200", 1, 7, 0, "171"}, {"3.00", 2, 3, 2, "2.00"}, {"8.00", 2, 2, 2, "8.00"},
		{"1.0", 2, 3, 2, "0.67"},     // three units, two kept, a discount written with one decimal
		{"1.0", 2, 3, 1, "0.7"},      // what the written scale alone would give
		{"101.99", 1, 2, 2, "51.00"}, // 50.995 is a tie: to the even cent
		{"35.0", 1, 1, 2, "35.00"},   // a wider scale pads
		// ties to the even digit from both sides, positive and negative
		{"0.25", 1, 2, 1, "0.1"}, {"0.35", 1, 2, 1, "0.2"}, {"-0.25", 1, 2, 1, "-0.1"}, {"-0.35", 1, 2, 1, "-0.2"},
		{"0.5", 1, 1, 0, "0"}, {"1.5", 1, 1, 0, "2"}, {"2.5", 1, 1, 0, "2"}, {"-0.5", 1, 1, 0, "0"}, {"-1.5", 1, 1, 0, "-2"}, {"-2.5", 1, 1, 0, "-2"},
		// not a tie: just past it rounds away, just short rounds down
		{"0.051", 1, 2, 2, "0.03"}, {"0.049", 1, 2, 2, "0.02"},
		// a negative denominator or numerator is ordinary arithmetic
		{"0.05", 1, -2, 2, "-0.02"}, {"0.05", -1, 2, 2, "-0.02"}, {"0.05", -1, -2, 2, "0.02"},
		{"0.15", 1, -2, 2, "-0.08"}, {"-0.15", -1, -2, 2, "-0.08"}, {"-0.15", 1, -2, 2, "0.08"},
		{"10.00", -1, 3, 2, "-3.33"}, {"10.00", 1, -3, 2, "-3.33"}, {"10.00", -2, -3, 2, "6.67"},
		// a zero numerator, and a result that rounds to zero
		{"10.00", 0, 3, 2, "0.00"}, {"0.01", 1, 3, 2, "0.00"}, {"-0.01", 1, 3, 2, "0.00"},
		// the scale is the one it is given, narrower or wider than the amount's
		{"1.2345", 1, 1, 2, "1.23"}, {"1.2355", 1, 1, 2, "1.24"}, {"1.2345", 1, 1, 6, "1.234500"},
	} {
		got, err := mustParse(t, c.a).MulRatio(c.num, c.den, c.scale)
		if err != nil {
			t.Errorf("%s * %d/%d at %d: %v", c.a, c.num, c.den, c.scale, err)
			continue
		}
		if got.String() != c.want {
			t.Errorf("%s * %d/%d at %d = %s, want %s", c.a, c.num, c.den, c.scale, got, c.want)
		}
		if got.Scale() != c.scale {
			t.Errorf("%s * %d/%d at %d has scale %d", c.a, c.num, c.den, c.scale, got.Scale())
		}
	}
}

func TestARatioThatRoundsToZeroPrintsAndSignsAsZero(t *testing.T) {
	got, err := mustParse(t, "-0.01").MulRatio(1, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "0.00" || got.Sign() != 0 {
		t.Errorf("got %s sign %d, want 0.00 sign 0", got, got.Sign())
	}
	if !got.Equal(mustParse(t, "0")) {
		t.Error("a negative amount rounded to zero is not equal to zero")
	}
}

func TestMulRatioAgreesWithBigRatForRandomInputs(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	for i := 0; i < 20000; i++ {
		scale := rng.IntN(5)
		a := fromParts(t, rng.Int64N(2_000_001)-1_000_000, scale)
		num := rng.Int64N(41) - 20
		den := rng.Int64N(41) - 20
		if den == 0 {
			den = 3
		}
		to := rng.IntN(6)
		got, err := a.MulRatio(num, den, to)
		if err != nil {
			t.Fatal(err)
		}
		want := roundHalfEven(new(big.Rat).Mul(ratOf(t, a), big.NewRat(num, den)), to)
		if ratOf(t, got).Cmp(want) != 0 || got.Scale() != to {
			t.Fatalf("%s * %d/%d at %d = %s (scale %d), want %s", a, num, den, to, got, got.Scale(), want.FloatString(to))
		}
	}
}

// digitsOf is the unscaled integer of an amount, read straight from the struct.
func digitsOf(a Amount) *big.Int {
	if a.unscaled == nil {
		return new(big.Int)
	}
	return a.unscaled
}

// fromParts builds the amount unscaled/10^scale through Parse.
func fromParts(t testing.TB, unscaled int64, scale int) Amount {
	t.Helper()
	neg := unscaled < 0
	digits := strconv.FormatUint(absU(unscaled), 10)
	if len(digits) <= scale {
		digits = strings.Repeat("0", scale-len(digits)+1) + digits
	}
	s := digits
	if scale > 0 {
		s = digits[:len(digits)-scale] + "." + digits[len(digits)-scale:]
	}
	if neg {
		s = "-" + s
	}
	return mustParse(t, s)
}

func absU(n int64) uint64 {
	if n < 0 {
		return uint64(-(n + 1)) + 1
	}
	return uint64(n)
}

func TestMulAndMulRatioAreExactAtTheEdgesOfInt64(t *testing.T) {
	edges := []int64{math.MaxInt64, math.MinInt64, math.MaxInt64 - 1, math.MinInt64 + 1, 1, -1, 0}
	amounts := []string{"1.5", "-0.05", "9223372036854775807.99", "-9223372036854775808.01", "0.00", "12345678901234567890.123"}
	for _, as := range amounts {
		a := mustParse(t, as)
		for _, n := range edges {
			got := a.Mul(n)
			want := new(big.Rat).Mul(ratOf(t, a), new(big.Rat).SetInt64(n))
			if ratOf(t, got).Cmp(want) != 0 {
				t.Errorf("%s * %d = %s, want %s", as, n, got, want.FloatString(a.Scale()))
			}
			if got.Scale() != a.Scale() {
				t.Errorf("%s * %d changed the scale to %d", as, n, got.Scale())
			}
			for _, d := range edges {
				if d == 0 {
					continue
				}
				for _, scale := range []int{0, 2, 5} {
					got, err := a.MulRatio(n, d, scale)
					if err != nil {
						t.Errorf("%s * %d/%d: %v", as, n, d, err)
						continue
					}
					want := roundHalfEven(new(big.Rat).Mul(ratOf(t, a), new(big.Rat).SetFrac(big.NewInt(n), big.NewInt(d))), scale)
					if ratOf(t, got).Cmp(want) != 0 || got.Scale() != scale {
						t.Errorf("%s * %d/%d at %d = %s, want %s", as, n, d, scale, got, want.FloatString(scale))
					}
				}
			}
		}
	}
}

func TestAShareIsRoundedAtTheCurrencysMinorUnitsWhenTheAmountWasWrittenShorter(t *testing.T) {
	a, _ := Parse("1.0")
	for currency, want := range map[string]string{"EUR": "0.67", "JPY": "0.7", "KWD": "0.667", "": "0.67", "eur": "0.67"} {
		got, err := a.MulRatio(2, 3, RoundingScale(a, currency))
		if err != nil || got.String() != want {
			t.Errorf("%q: %s, %v; want %s", currency, got, err, want)
		}
	}
	// and never coarser than the amount was written: "1.2345" in EUR keeps four decimals
	b, _ := Parse("1.2345")
	if RoundingScale(b, "EUR") != 4 {
		t.Error("an amount written with four decimals is rounded at fewer")
	}
	// and never coarser than the amount was written, in a currency counted in whole units
	if RoundingScale(b, "JPY") != 4 || RoundingScale(b, "KWD") != 4 {
		t.Error("an amount written with four decimals is rounded at fewer in JPY or KWD")
	}
	// the currency wins when it counts more decimals
	if RoundingScale(mustParse(t, "5"), "KWD") != 3 || RoundingScale(mustParse(t, "5"), "USD") != 2 || RoundingScale(mustParse(t, "5"), "JPY") != 0 {
		t.Error("the currency's minor units are not the floor of the rounding scale")
	}
	if RoundingScale(Amount{}, "EUR") != 2 {
		t.Error("the zero value is not rounded at the currency's decimals")
	}
}

func TestTheCurrenciesCountedWithoutDecimalsAndWithThree(t *testing.T) {
	for _, c := range []string{"BIF", "CLP", "DJF", "GNF", "ISK", "JPY", "KMF", "KRW", "PYG", "RWF", "UGX", "VND", "VUV", "XAF", "XOF", "XPF"} {
		if MinorUnits(c) != 0 {
			t.Errorf("%s: %d", c, MinorUnits(c))
		}
		if MinorUnits(strings.ToLower(c)) != 0 {
			t.Errorf("%s in lower case: %d", c, MinorUnits(strings.ToLower(c)))
		}
	}
	for _, c := range []string{"BHD", "IQD", "JOD", "KWD", "LYD", "OMR", "TND"} {
		if MinorUnits(c) != 3 {
			t.Errorf("%s: %d", c, MinorUnits(c))
		}
		if MinorUnits(strings.ToLower(c)) != 3 {
			t.Errorf("%s in lower case: %d", c, MinorUnits(strings.ToLower(c)))
		}
	}
	// only ASCII letters are folded: these Unicode letters would fold to "ISK" and "JPY"
	for _, c := range []string{"EUR", "USD", "HUF", "TWD", "MGA", "XXX", "not a code", "", "\u0131\u017fk", "jpy "} {
		if MinorUnits(c) != 2 {
			t.Errorf("%q: %d", c, MinorUnits(c))
		}
	}
}

func TestMulRatioByZeroOrAtANegativeScaleIsRefused(t *testing.T) {
	a := mustParse(t, "10.00")
	if got, err := a.MulRatio(1, 0, 2); err == nil {
		t.Errorf("a zero denominator was accepted: %s", got)
	}
	if got, err := a.MulRatio(0, 0, 2); err == nil {
		t.Errorf("0/0 was accepted: %s", got)
	}
	if got, err := a.MulRatio(1, 3, -1); err == nil {
		t.Errorf("a scale of -1 was accepted: %s", got)
	}
	if got, err := a.MulRatio(1, 3, math.MinInt); err == nil {
		t.Errorf("the smallest scale was accepted: %s", got)
	}
	// scale 0 is a scale
	if got, err := a.MulRatio(1, 3, 0); err != nil || got.String() != "3" {
		t.Errorf("scale 0: %s, %v", got, err)
	}
}

func TestMulKeepsTheScale(t *testing.T) {
	for _, c := range []struct {
		a    string
		n    int64
		want string
	}{{"5.00", 0, "0.00"}, {"9.99", 2, "19.98"}, {"9.99", -2, "-19.98"}, {"-0.10", 3, "-0.30"}, {"-0.10", -3, "0.30"}, {"7", 3, "21"}, {"0.5", 1, "0.5"}, {"-5.00", 0, "0.00"}} {
		got := mustParse(t, c.a).Mul(c.n)
		if got.String() != c.want || got.Scale() != mustParse(t, c.a).Scale() {
			t.Errorf("%s * %d = %s (scale %d), want %s", c.a, c.n, got, got.Scale(), c.want)
		}
	}
}

func TestEqualIsByValue(t *testing.T) {
	eq := [][2]string{{"36.0", "36.00"}, {"36", "36.000"}, {"0", "0.00"}, {"-0.0", "0"}, {"-1.50", "-1.5"}, {"0.10", "0.1"}}
	for _, c := range eq {
		a, b := mustParse(t, c[0]), mustParse(t, c[1])
		if !a.Equal(b) || !b.Equal(a) {
			t.Errorf("%s should equal %s", c[0], c[1])
		}
	}
	ne := [][2]string{{"36.01", "36.00"}, {"36.0", "36.01"}, {"1", "-1"}, {"0.1", "0.01"}, {"10", "1"}, {"1.5", "-1.5"}, {"100", "1.00"}}
	for _, c := range ne {
		a, b := mustParse(t, c[0]), mustParse(t, c[1])
		if a.Equal(b) || b.Equal(a) {
			t.Errorf("%s should not equal %s", c[0], c[1])
		}
	}
	if !(Amount{}).Equal(mustParse(t, "0.00")) || !mustParse(t, "0.00").Equal(Amount{}) {
		t.Error("the zero value should equal 0.00")
	}
	if (Amount{}).Equal(mustParse(t, "0.01")) {
		t.Error("the zero value should not equal 0.01")
	}
}

func TestAMillionRandomSumsEqualBigRat(t *testing.T) {
	const steps = 1_000_000
	rng := rand.New(rand.NewPCG(20260711, 6))
	var sum Amount
	total := new(big.Rat)
	widest := 0
	scaleCost := [5]*big.Int{pow10(0), pow10(1), pow10(2), pow10(3), pow10(4)}
	for i := 0; i < steps; i++ {
		scale := rng.IntN(5)
		unscaled := rng.Int64N(2_000_000_001) - 1_000_000_000
		a := Amount{unscaled: big.NewInt(unscaled), scale: scale}
		r := new(big.Rat).SetFrac(big.NewInt(unscaled), scaleCost[scale])
		if scale > widest {
			widest = scale
		}
		if rng.IntN(4) == 0 {
			sum = sum.Sub(a)
			total.Sub(total, r)
		} else {
			sum = sum.Add(a)
			total.Add(total, r)
		}
		// the running amount is exactly the running rational, at every step
		got := new(big.Rat).SetFrac(digitsOf(sum), scaleCost[sum.scale])
		if got.Cmp(total) != 0 {
			t.Fatalf("step %d: %s, want %s", i, sum, total.FloatString(sum.scale))
		}
		if sum.Scale() != widest {
			t.Fatalf("step %d: scale %d, want the widest seen so far, %d", i, sum.Scale(), widest)
		}
		if i%50000 == 0 && ratOf(t, sum).Cmp(total) != 0 {
			t.Fatalf("step %d: the printed %s is not %s", i, sum, total.FloatString(sum.scale))
		}
	}
	if ratOf(t, sum).Cmp(total) != 0 {
		t.Fatalf("the final %s is not %s", sum, total.FloatString(sum.scale))
	}
}

func TestAZeroDecimalAndAThreeDecimalCurrencySumExactly(t *testing.T) {
	if got := mustParse(t, "1200").Add(mustParse(t, "800")).String(); got != "2000" {
		t.Errorf("1200 + 800 = %s", got)
	}
	if got := mustParse(t, "1.250").Add(mustParse(t, "0.005")).String(); got != "1.255" {
		t.Errorf("1.250 + 0.005 = %s", got)
	}
}

func TestTheZeroValueIsZeroAndNegativeZeroPrintsAsZero(t *testing.T) {
	var z Amount
	if z.String() != "0" || z.Sign() != 0 || z.Scale() != 0 {
		t.Errorf("zero value: %q sign %d scale %d", z.String(), z.Sign(), z.Scale())
	}
	if got := mustParse(t, "-0.00").String(); got != "0.00" {
		t.Errorf("Parse(-0.00) prints %q", got)
	}
	// the zero value is usable in every operation
	if got := z.Add(mustParse(t, "1.5")).String(); got != "1.5" {
		t.Errorf("zero + 1.5 = %s", got)
	}
	if got := mustParse(t, "1.5").Add(z).String(); got != "1.5" {
		t.Errorf("1.5 + zero = %s", got)
	}
	if got := z.Sub(mustParse(t, "1.5")).String(); got != "-1.5" {
		t.Errorf("zero - 1.5 = %s", got)
	}
	if got := z.Mul(5).String(); got != "0" {
		t.Errorf("zero * 5 = %s", got)
	}
	if got, err := z.MulRatio(1, 3, 2); err != nil || got.String() != "0.00" {
		t.Errorf("zero * 1/3 at 2 = %s, %v", got, err)
	}
	if got := z.Add(z).String(); got != "0" {
		t.Errorf("zero + zero = %s", got)
	}
}

func TestSmallAmountsPrintTheirLeadingZeroAndTheirSign(t *testing.T) {
	for _, c := range []struct {
		a        string
		num, den int64
		scale    int
		want     string
	}{
		{"0.05", 1, 10, 3, "0.005"},   // fewer digits than the scale
		{"-0.05", 1, 10, 3, "-0.005"}, // and negative
		{"-1.00", 1, 10, 2, "-0.10"},  // a negative amount smaller than one
		{"1.00", 1, 1000, 4, "0.0010"},
		{"0.01", 1, 1, 5, "0.01000"},
	} {
		got, err := mustParse(t, c.a).MulRatio(c.num, c.den, c.scale)
		if err != nil || got.String() != c.want {
			t.Errorf("%s * %d/%d at %d = %s, %v; want %s", c.a, c.num, c.den, c.scale, got, err, c.want)
		}
	}
}

func TestNoMethodChangesItsReceiver(t *testing.T) {
	for _, s := range []string{"42.00", "-0.10", "0", "9223372036854775807.99", "1.5"} {
		a := mustParse(t, s)
		other := mustParse(t, "7.125")
		before, beforeOther := a.String(), other.String()
		_ = a.Add(other)
		_ = a.Sub(other)
		_ = a.Add(a)
		_ = a.Sub(a)
		_ = a.Mul(3)
		_ = a.Mul(math.MinInt64)
		_, _ = a.MulRatio(2, 3, 1)
		_, _ = a.MulRatio(2, 0, 1)
		_ = a.Equal(other)
		_ = a.Sign()
		_ = a.Scale()
		_ = a.String()
		_ = other.Add(a)
		_ = other.Sub(a)
		if a.String() != before || other.String() != beforeOther {
			t.Errorf("%s: receiver is now %s, operand %s", before, a, other)
		}
	}
	// a result is its own value: using it never reaches back into its operands
	a, b := mustParse(t, "1.5"), mustParse(t, "2.5")
	sum := a.Add(Amount{})
	sum2 := Amount{}.Add(b)
	sum = sum.Add(mustParse(t, "100"))
	sum2 = sum2.Mul(10)
	same := a.Mul(1)
	same = same.Add(mustParse(t, "1"))
	if a.String() != "1.5" || b.String() != "2.5" || sum.String() != "101.5" || sum2.String() != "25.0" || same.String() != "2.5" {
		t.Errorf("results alias their operands: %s %s %s %s %s", a, b, sum, sum2, same)
	}
}

var plainDecimal = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`)

func FuzzParse(f *testing.F) {
	for _, s := range []string{"0", "42.00", "-0.10", "007.5", "", "-", ".5", "5.", "1e3", "١", "-0.00", "1.2.3", "99999999999999999999.99999999999999999999"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		a, err := Parse(s)
		if err != nil {
			if !utf8.ValidString(err.Error()) {
				t.Fatalf("Parse(%q): the error is not valid UTF-8: %q", s, err.Error())
			}
			return
		}
		if !plainDecimal.MatchString(s) {
			t.Fatalf("Parse(%q) was accepted", s)
		}
		back, err := Parse(a.String())
		if err != nil {
			t.Fatalf("Parse(%q).String() = %q does not parse: %v", s, a.String(), err)
		}
		if !back.Equal(a) || back.Scale() != a.Scale() {
			t.Fatalf("Parse(%q) = %s, and its printing parses to %s at scale %d, not %d", s, a, back, back.Scale(), a.Scale())
		}
	})
}
