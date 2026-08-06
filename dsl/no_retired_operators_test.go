package dsl

import (
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/dslfs"
)

var hasOperatorRe = regexp.MustCompile(`\bhas\b`)

// hasTopLevelComma reports whether s contains a comma outside any
// paren/brace/bracket nesting and outside string literals -- i.e. the retired
// `,`-as-OR separator (a comma inside `canonicalId(args.x, "...")` is a function
// argument separator at depth > 0 and is fine).
func hasTopLevelComma(s string) bool {
	depth := 0
	inStr := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"':
			inStr = !inStr
		case inStr:
		case c == '(' || c == '{' || c == '[':
			depth++
		case c == ')' || c == '}' || c == ']':
			depth--
		case c == ',' && depth == 0:
			return true
		}
	}
	return false
}

// TestNoRetiredOperatorForms is the #977 lock-in: filter clauses must use the
// single unified operator grammar. The retired forms are rejected tree-wide:
//   - `;` AND separator   -> use `&&`
//   - `,` OR separator     -> use `||`
//   - `has` membership     -> use `in`
//   - `?.` optional-chain  -> use `when(args.x) { ... }`
func TestNoRetiredOperatorForms(t *testing.T) {
	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	for _, p := range paths {
		file, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(file)
		file.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if idx := strings.Index(line, "//"); idx >= 0 {
				line = line[:idx]
			}
			trim := strings.TrimSpace(line)
			if !strings.HasPrefix(trim, "filter ") && !strings.HasPrefix(trim, "filter\t") {
				continue
			}
			clause := strings.TrimSpace(strings.TrimPrefix(trim, "filter"))
			ref := func(msg string) {
				t.Errorf("%s:%d retired filter operator (%s):\n  %s", p, i+1, msg, trim)
			}
			if strings.Contains(clause, "?.") {
				ref("`?.` is retired -- use `when(args.x) { ... }`")
			}
			if hasOperatorRe.MatchString(clause) {
				ref("`has` is retired -- use `<scalar> in <collection>`")
			}
			if strings.Contains(clause, ";") {
				ref("`;` AND separator is retired -- use `&&`")
			}
			if hasTopLevelComma(clause) {
				ref("`,` OR separator is retired -- use `||`")
			}
		}
	}
}
