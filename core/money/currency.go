package money

// MinorUnits is the number of decimals a currency is counted in: 0, 3 or 2.
//
// The table is the list of minor units in ISO 4217, for the codes of the
// Admin API's CurrencyCode enum recorded in
// cmd/shopifyschema/testdata/schema-2026-07.json. A code is matched in upper
// case; a code the table has never seen, and the empty string, is 2. A larger
// exponent than a currency has only keeps more decimals, so an unknown code is
// never rounded too coarsely.
func MinorUnits(currency string) int {
	switch upperASCII(currency) {
	case "BIF", "CLP", "DJF", "GNF", "ISK", "JPY", "KMF", "KRW", "PYG", "RWF", "UGX", "VND", "VUV", "XAF", "XOF", "XPF":
		return 0
	case "BHD", "IQD", "JOD", "KWD", "LYD", "OMR", "TND":
		return 3
	}
	return 2
}

// RoundingScale is the scale a computed amount is rounded at: the larger of
// the amount's own scale and the currency's minor units. The written scale
// alone is not enough, because it is whatever the store wrote, and a whole
// amount is written with one decimal: a third of "1.0" would come out "0.3" in
// a currency counted in cents. Pass the result to MulRatio, so the one
// rounding rule is seen at every call.
func RoundingScale(a Amount, currency string) int {
	return max(a.Scale(), MinorUnits(currency))
}

// upperASCII upper-cases the letters a-z only. A currency code is ASCII, and a
// Unicode upper-casing would fold a few other letters onto it.
func upperASCII(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'a' && c <= 'z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if b[j] >= 'a' && b[j] <= 'z' {
					b[j] -= 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}
