package baseparser

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// onebyte_lookback_gate_test.go -- memql#3190's gate.
//
// THE DEFECT. Deciding whether a `"` closes a string literal by looking ONE
// BYTE BACK for a backslash:
//
//	if c == '"' && (i == 0 || line[i-1] != '\\') { ... }
//
// cannot distinguish an ESCAPED quote (`\"`) from a quote that follows a
// COMPLETED escape (`\\"`). On a literal ending in `\\` the scanner runs past
// the real closing quote and reads the following code as literal interior --
// so a scanner skips code, a validator fails open, or a rewriter corrupts a
// file, depending on what it feeds.
//
// WHY A GATE. This was fixed one site at a time in memql#2949, #2872 and
// #3045, and each fix left the rest of the tree untouched because nobody
// enumerated the sites. memql#3190 enumerated them -- there were NINE live
// ones -- and converted all nine. Without something that fails on the tenth,
// the pattern comes back: it is a natural way to write the check, it looks
// right, and it passes every test that does not contain a `\\`.
//
// The correct shape is to TRACK escape state: a backslash consumes the next
// byte, so `\\` spends itself and the next quote closes the literal.
// StripLineComment and BlankComments in this package are the reference; so are
// parser.BlankCommentsAndStrings, automations.splitCoalesceArgs and
// parser.splitTopLevelArgs.
//
// # Why the AST and not a regex
//
// A regex over source text hits every doc comment that quotes the buggy
// pattern in order to describe it. There were SIX before this change
// (rewriter.go, evaluator.go, split_escape_state_test.go,
// split_coalesce_escape_test.go, scrub_source_test.go and
// scripts/migrations/event_payload_args/main.go) and there are more now, since
// each of the nine converted sites records what it replaced -- so an exclusion
// list would need maintaining forever. Parsing WITHOUT ParseComments makes
// comments structurally absent, and a pattern inside a string literal is one
// BasicLit rather than an index expression, so code and prose are
// distinguished by construction instead.
//
// It also does not care what the variable is called or how the caller guards
// the first byte. The nine real sites spanned FOUR receiver names (`s`,
// `line`, `source`, `remaining`), two index names (`i`, `end`) and three guard
// shapes (`i == 0 ||`, `end == 1 ||`, none at all); a regex tuned to one
// spelling would have found a subset, which is exactly how gates in this repo
// have shipped narrower than the lexer before.

// lookbackFinding is one flagged expression.
type lookbackFinding struct {
	File string // path relative to the scanned root
	Line int
	Expr string // the offending expression, rendered
}

func (f lookbackFinding) String() string {
	return fmt.Sprintf("%s:%d: %s", f.File, f.Line, f.Expr)
}

// findOneByteLookbackScans parses every .go file under root and returns each
// comparison of a one-byte lookback (`X[<anything>-1]`) against a backslash
// literal, in either operand order and under either `!=` or `==`.
func findOneByteLookbackScans(root string) ([]lookbackFinding, error) {
	var out []lookbackFinding
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "vendor", "node_modules", "bin", "dist":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(info.Name(), ".go") {
			return nil
		}
		fset := token.NewFileSet()
		// No ParseComments: comments describing the pattern are then not part
		// of the tree at all.
		//
		// A parse error is not a gate failure -- a testdata fixture may be
		// deliberately unparseable, and such a file is not compiled code. The
		// partial tree go/parser still returns IS scanned, so a lookback in a
		// file that merely fails to parse further down is not a hiding place.
		file, _ := parser.ParseFile(fset, path, nil, 0)
		if file == nil {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		ast.Inspect(file, func(n ast.Node) bool {
			bin, ok := n.(*ast.BinaryExpr)
			if !ok || (bin.Op != token.NEQ && bin.Op != token.EQL) {
				return true
			}
			if (isOneByteLookback(bin.X) && isBackslashLiteral(bin.Y)) ||
				(isOneByteLookback(bin.Y) && isBackslashLiteral(bin.X)) {
				out = append(out, lookbackFinding{
					File: filepath.ToSlash(rel),
					Line: fset.Position(bin.Pos()).Line,
					Expr: renderExpr(bin),
				})
			}
			return true
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Line < out[j].Line
	})
	return out, nil
}

// isOneByteLookback reports whether e indexes something one byte back:
// `X[<anything> - 1]`. The indexed expression and the index variable can be
// named anything -- that is the point, the nine real sites used five different
// names.
func isOneByteLookback(e ast.Expr) bool {
	idx, ok := e.(*ast.IndexExpr)
	if !ok {
		return false
	}
	sub, ok := idx.Index.(*ast.BinaryExpr)
	if !ok || sub.Op != token.SUB {
		return false
	}
	lit, ok := sub.Y.(*ast.BasicLit)
	return ok && lit.Kind == token.INT && lit.Value == "1"
}

// isBackslashLiteral reports whether e is a rune literal for `\`, in any
// spelling Go accepts (`'\\'`, `'\x5c'`, `'\'`, `'\134'`).
func isBackslashLiteral(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.CHAR {
		return false
	}
	r, _, _, err := strconv.UnquoteChar(strings.Trim(lit.Value, "'"), '\'')
	if err != nil {
		return false
	}
	return r == '\\'
}

// renderExpr prints a comparison compactly enough for a failure message.
func renderExpr(bin *ast.BinaryExpr) string {
	return fmt.Sprintf("%s %s %s", exprText(bin.X), bin.Op, exprText(bin.Y))
}

func exprText(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.BasicLit:
		return v.Value
	case *ast.IndexExpr:
		return exprText(v.X) + "[" + exprText(v.Index) + "]"
	case *ast.BinaryExpr:
		return exprText(v.X) + v.Op.String() + exprText(v.Y)
	case *ast.SelectorExpr:
		return exprText(v.X) + "." + v.Sel.Name
	case *ast.CallExpr:
		return exprText(v.Fun) + "(...)"
	default:
		return "<expr>"
	}
}

// TestNoOneByteLookbackQuoteScan is the gate. It fails on a NEW one-byte
// lookback anywhere in the module.
//
// If this fails on code you just wrote: the scan is wrong on any literal that
// ends in a completed `\\` escape. Track escape state instead --
//
//	escaped := false
//	for i := 0; i < len(s); i++ {
//	    switch {
//	    case escaped:      escaped = false   // the escape is spent
//	    case s[i] == '\\': escaped = true
//	    case s[i] == '"':  ...               // this one really closes it
//	    }
//	}
//
// -- or delegate to a shared scanner that already does: StripLineComment /
// BlankComments / CommentSpans here, or parser.BlankCommentsAndStrings.
// Delegating is not always right (a caller needing offsets into the ORIGINAL,
// or accepting `'` as well as `"`, has a real reason to stay local); tracking
// escape state locally is always right. Record which one you chose and why, as
// the nine converted sites do.
func TestNoOneByteLookbackQuoteScan(t *testing.T) {
	root := moduleRootForGate(t)
	found, err := findOneByteLookbackScans(root)
	if err != nil {
		t.Fatalf("scanning %s: %v", root, err)
	}
	if len(found) > 0 {
		var sb strings.Builder
		for _, f := range found {
			sb.WriteString("  " + f.String() + "\n")
		}
		t.Fatalf("memql#3190: %d one-byte-lookback string scan(s):\n%s\n"+
			"A one-byte lookback cannot tell an escaped quote (`\\\"`) from a quote following "+
			"a COMPLETED `\\\\` escape, so a literal ending in a backslash pair never leaves "+
			"string state and the scanner reads the code after it as literal interior. "+
			"Track escape state, or delegate to a scanner that does (baseparser.StripLineComment / "+
			"BlankComments, parser.BlankCommentsAndStrings). See the doc comment on this test.",
			len(found), sb.String())
	}
}

// TestOneByteLookbackGateFindsEveryKnownSpelling validates the gate's own
// pattern in the POSITIVE direction, against a corpus built from the nine real
// pre-fix sites (all five receiver names, both guard shapes) plus spellings
// nobody in the tree used but the next author might.
//
// This is the half of the validation a gate usually skips, and skipping it is
// how the scanners in this repo keep shipping narrower than the grammar they
// police.
func TestOneByteLookbackGateFindsEveryKnownSpelling(t *testing.T) {
	positives := []struct {
		name string
		code string
	}{
		// The three group-A strip*Comment copies, verbatim in shape.
		{"line[i-1] with an i == 0 guard", `if c == '"' && (i == 0 || line[i-1] != '\\') { _ = c }`},
		// declared_usage_validator.go.
		{"source[i-1] with an i == 0 guard", `if c == '"' && (i == 0 || source[i-1] != '\\') { _ = c }`},
		// sense/tokenize.go.
		{"line[i-1] indexed directly", `if line[i] == '"' && (i == 0 || line[i-1] != '\\') { _ = i }`},
		// shape.go, the quoted-string arm.
		{"s[end-1] with an end == 1 guard", `if s[end] == q && (end == 1 || s[end-1] != '\\') { _ = q }`},
		// shape.go, the three depth scanners.
		{"remaining[end-1] with no guard", `if inQuote && ch == q && remaining[end-1] != '\\' { _ = ch }`},
		{"s[end-1] with no guard", `if inQuote && ch == q && s[end-1] != '\\' { _ = ch }`},
		// Spellings the tree did not use.
		{"equality direction", `if s[j-1] == '\\' { _ = j }`},
		{"reversed operands", `if '\\' != s[i-1] { _ = i }`},
		{"hex escape spelling", `if s[i-1] != '\x5c' { _ = i }`},
		{"octal escape spelling", `if s[i-1] != '\134' { _ = i }`},
		{"unicode escape spelling", `if s[i-1] != '\u005c' { _ = i }`},
		{"unformatted spacing", `if s[ i - 1 ] != '\\' { _ = i }`},
		{"field receiver", `if p.buf[i-1] != '\\' { _ = i }`},
		{"function-call receiver", `if bytesOf(s)[i-1] != '\\' { _ = i }`},
		{"non-trivial index expression", `if s[start+n-1] != '\\' { _ = n }`},
	}
	for _, tc := range positives {
		t.Run("finds/"+tc.name, func(t *testing.T) {
			if n := scanSnippet(t, tc.code); n != 1 {
				t.Errorf("gate found %d hits in %q, want 1 -- the gate is NARROWER than the defect it polices", n, tc.code)
			}
		})
	}
}

// TestOneByteLookbackGateIgnoresProseAndCorrectCode validates the NEGATIVE
// direction: the gate must not fire on a comment that describes the pattern
// (there are six such comments in the tree, and a naive regex hits every one),
// on a fixture string containing it, or on a correct escape-tracking scan.
func TestOneByteLookbackGateIgnoresProseAndCorrectCode(t *testing.T) {
	negatives := []struct {
		name string
		code string
	}{
		{
			name: "line comment describing the pattern",
			code: "// The scan used to look one byte back (`s[i-1] != '\\\\'`), which cannot\n// tell an escaped quote from a completed escape.\n_ = 1",
		},
		{
			name: "block comment describing the pattern",
			code: "/* `(i == 0 || line[i-1] != '\\\\')` is the defect. */\n_ = 1",
		},
		{
			name: "fixture string containing the pattern",
			code: "src := `if c == '\"' && (i == 0 || line[i-1] != '\\\\') { }`\n_ = src",
		},
		{
			name: "interpreted string containing the pattern",
			code: `msg := "s[i-1] != '\\\\'"` + "\n_ = msg",
		},
		{
			name: "correct escape tracking",
			code: "escaped := false\nfor i := 0; i < len(s); i++ {\n\tswitch {\n\tcase escaped:\n\t\tescaped = false\n\tcase s[i] == '\\\\':\n\t\tescaped = true\n\tcase s[i] == '\"':\n\t\t_ = i\n\t}\n}",
		},
		{
			name: "a lookback against something that is not a backslash",
			code: `if s[i-1] != '"' { _ = i }`,
		},
		{
			name: "a two-byte lookback is a different question",
			code: `if s[i-2] != '\\' { _ = i }`,
		},
		{
			name: "counting backslashes forwards is not a lookback",
			code: `if s[i] == '\\' { _ = i }`,
		},
	}
	for _, tc := range negatives {
		t.Run("ignores/"+tc.name, func(t *testing.T) {
			if n := scanSnippet(t, tc.code); n != 0 {
				t.Errorf("gate found %d hit(s) in:\n%s\nwant 0", n, tc.code)
			}
		})
	}
}

// The gate must see the whole module, not just this package -- eight of the
// nine converted sites live elsewhere. A root that scans nothing would make it
// pass vacuously, which is the classic way a source gate rots.
func TestOneByteLookbackGateScansTheWholeModule(t *testing.T) {
	root := moduleRootForGate(t)
	var goFiles int
	for _, dir := range []string{
		"component/automations/steps",
		"component/memql",
		"component/memql/sense",
		"component/language/pagination",
		"dsl",
		"scripts",
	} {
		if _, err := os.Stat(filepath.Join(root, dir)); err != nil {
			t.Errorf("gate root %s does not contain %s: %v", root, dir, err)
		}
	}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "vendor", "node_modules", "bin", "dist":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(info.Name(), ".go") {
			goFiles++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	if goFiles < 500 {
		t.Errorf("gate scanned %d .go files -- far too few for this module; the root is wrong", goFiles)
	}
}

// scanSnippet wraps a fragment in a compilable file, writes it to a temp
// directory and runs the gate's scanner over it.
func scanSnippet(t *testing.T, body string) int {
	t.Helper()
	dir := t.TempDir()
	src := "package fixture\n\nfunc fixture(s string, line string, source string, remaining string, i, j, n, start, end int, c, ch, q byte, p struct{ buf string }) {\n" +
		body + "\n}\n\nfunc bytesOf(s string) string { return s }\n"
	path := filepath.Join(dir, "fixture.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	// The scanner tolerates an unparseable file (a testdata fixture may be one
	// deliberately); a fixture in THIS test must still parse, or a typo in it
	// would read as "the gate found nothing".
	if _, err := parser.ParseFile(token.NewFileSet(), path, nil, 0); err != nil {
		t.Fatalf("fixture does not parse: %v\nsource:\n%s", err, src)
	}
	found, err := findOneByteLookbackScans(dir)
	if err != nil {
		t.Fatalf("scanning fixture: %v\nsource:\n%s", err, src)
	}
	return len(found)
}

// moduleRootForGate ascends from this file's directory until it finds go.mod.
func moduleRootForGate(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve caller path")
	}
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found walking up from test file")
		}
		dir = parent
	}
}
