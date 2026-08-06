package baseparser

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// no_lookback_scan_test.go -- memql#3120's fourth acceptance item, and the only
// one that changes the future.
//
// The one-byte-lookback string scan has now been fixed FOUR times -- memql#2949,
// memql#2872, memql#3045, memql#3046 -- and each fix left the rest of the tree
// untouched, because the pattern is easy to write and nothing objected. This
// gate objects.
//
// Matched on the AST, deliberately, rather than by grepping lines. Every
// document and test in this repo that DISCUSSES the bug necessarily contains
// the pattern as text -- including this file -- so a textual gate would either
// fire on its own explanation or need an allow-list that grows until it hides
// a real hit. `go/parser` sees expressions and never sees a comment or a
// string, so the distinction is structural instead of negotiated.

// lookbackFinding is one `s[i-1] == '\\'` (or `!=`) comparison.
type lookbackFinding struct {
	file string
	line int
	expr string
}

// isOneByteLookbackEscapeTest reports whether n is a comparison of some
// `x[<expr>-1]` against the character literal '\\'.
func isOneByteLookbackEscapeTest(n ast.Node) bool {
	cmp, ok := n.(*ast.BinaryExpr)
	if !ok || (cmp.Op != token.EQL && cmp.Op != token.NEQ) {
		return false
	}

	lit, ok := cmp.Y.(*ast.BasicLit)
	if !ok || lit.Kind != token.CHAR || lit.Value != `'\\'` {
		return false
	}

	index, ok := cmp.X.(*ast.IndexExpr)
	if !ok {
		return false
	}

	offset, ok := index.Index.(*ast.BinaryExpr)
	if !ok || offset.Op != token.SUB {
		return false
	}
	one, ok := offset.Y.(*ast.BasicLit)
	return ok && one.Kind == token.INT && one.Value == "1"
}

// repoRoot walks up from the working directory to the directory holding go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod found above the working directory")
		}
		dir = parent
	}
}

func TestNoOneByteLookbackEscapeScans(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()

	var findings []lookbackFinding
	var scanned int

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "vendor", "bin", "dist":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			// Generated or build-tag-excluded files still parse; a genuine
			// syntax error is someone else's failing test.
			return nil
		}
		scanned++

		rel, _ := filepath.Rel(root, path)
		ast.Inspect(file, func(n ast.Node) bool {
			if !isOneByteLookbackEscapeTest(n) {
				return true
			}
			pos := fset.Position(n.Pos())
			findings = append(findings, lookbackFinding{
				file: rel,
				line: pos.Line,
				expr: exprText(n),
			})
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	// Vacuity guard. A gate that only asserts on findings cannot tell a clean
	// tree from a walk that visited nothing -- which is precisely how this bug
	// survived four fixes.
	if scanned < 500 {
		t.Fatalf("parsed only %d .go files under %s; the walk is broken and this gate proved nothing", scanned, root)
	}

	if len(findings) == 0 {
		return
	}

	sort.Slice(findings, func(a, b int) bool {
		if findings[a].file != findings[b].file {
			return findings[a].file < findings[b].file
		}
		return findings[a].line < findings[b].line
	})

	var sb strings.Builder
	sb.WriteString("one-byte-lookback escape test(s) found (memql#3120):\n\n")
	for _, f := range findings {
		sb.WriteString("  " + f.file + ":" + itoaLine(f.line) + "  " + f.expr + "\n")
	}
	sb.WriteString(`
Deciding whether a quote is escaped by looking ONE BYTE BACK cannot tell an
escaped quote (\") from a quote following a COMPLETED escape (\\"). A literal
ending in a backslash pair then never leaves string state, and everything after
it is treated as literal interior.

Track escape state instead -- consume the escaped byte together with its
backslash:

    if inString {
        if c == '\\' { i++; continue }
        if c == '"'  { inString = false }
        continue
    }

If the source you scan uses ` + "`\"`" + ` only and you want a trailing line comment
removed, call baseparser.StripLineComment rather than writing a twelfth copy.`)

	t.Error(sb.String())
}

// exprText renders a matched comparison compactly for the failure message.
func exprText(n ast.Node) string {
	cmp := n.(*ast.BinaryExpr)
	index := cmp.X.(*ast.IndexExpr)
	offset := index.Index.(*ast.BinaryExpr)

	var sb strings.Builder
	sb.WriteString(identText(index.X))
	sb.WriteString("[")
	sb.WriteString(identText(offset.X))
	sb.WriteString("-1] ")
	sb.WriteString(cmp.Op.String())
	sb.WriteString(` '\\'`)
	return sb.String()
}

func identText(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return identText(v.X) + "." + v.Sel.Name
	default:
		return "?"
	}
}

func itoaLine(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
