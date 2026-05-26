package baseparser

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseNumericLiteral converts a TokenNumber-shaped literal string to
// its canonical Go representation:
//
//   - bare integer (`42`, `-7`, `0`) -> int64
//   - decimal / scientific (`3.14`, `1e9`, `2.5e-3`) -> float64
//
// Single source of truth for both runtime parsers (component/memql/
// parser.go and component/language/parser/parser.go) -- previously
// each had its own near-identical helper (parseNumberLiteral and
// parseNumericLiteral respectively), and the parallel implementations
// were exactly the divergence pattern the epic #218 retirement
// targets (#216 / #221 / #239 all required matching fixes in both
// helpers). Consolidated in #265.
//
// Downstream directive-arg consumers (paginate limit/offset,
// withDepth, etc.) that need an int should pass the returned `any`
// through the langparser-side `numericAsInt` helper rather than
// asserting `.(float64)` directly -- the assertion fails for integer
// literals now that they decode to int64.
func ParseNumericLiteral(lit string) (any, error) {
	if !strings.ContainsAny(lit, ".eE") {
		if i, err := strconv.ParseInt(lit, 10, 64); err == nil {
			return i, nil
		}
	}
	if f, err := strconv.ParseFloat(lit, 64); err == nil {
		return f, nil
	}
	return nil, fmt.Errorf("invalid numeric literal %q", lit)
}
