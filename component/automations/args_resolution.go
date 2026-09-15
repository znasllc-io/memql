package automations

// args_resolution.go -- what an automation's expressions may read beyond its
// statements (epic memql#5370), and the G5 source scan (memql#2367).
//
// A statement body's names are compiler.CheckBody's: it compiled at load, and
// an argument is read args.x. The two expressions an automation carries
// outside its body -- the trigger filter and each precondition -- are checked
// here: a free name in either is a root the run binds before its first
// statement (compiler.IsBodyRoot: args, actor, event, config, partition, now)
// or the filter's own parameter, so a misspelled root is a load error rather
// than a filter that decides false, or a precondition that misses, on every
// fire.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/compiler"
)

// validateOuterExpressionNames checks the trigger filter's and the
// preconditions' free names (see the file comment).
func validateOuterExpressionNames(a *Automation) error {
	check := func(where string, n ast.ExpressionNode) error {
		if n == nil {
			return nil
		}
		var unknown []string
		v1FreeNames(n, nil, func(id *ast.IdentExpr) {
			if !compiler.IsBodyRoot("automation", id.Name) {
				unknown = append(unknown, id.Name)
			}
		})
		if len(unknown) == 0 {
			return nil
		}
		sort.Strings(unknown)
		return fmt.Errorf("automation %q: unknown name %q in %s (`%s`) -- it reads args.<field>, actor, event, config, partition or now, and a filter its own parameter",
			a.Name, unknown[0], where, ast.FormatExpr(n))
	}
	if a.Trigger != nil && a.Trigger.FilterLambda != nil {
		if err := check("the trigger filter", a.Trigger.FilterLambda); err != nil {
			return err
		}
	}
	for _, pc := range a.Preconditions {
		if pc != nil {
			if err := check("precondition "+pc.ID, pc.checkExpr); err != nil {
				return err
			}
		}
	}
	return nil
}

// v1FreeNames calls visit for every IdentExpr in n that no enclosing lambda
// binds: the names n reads from its scope. A callee name is not an IdentExpr
// (CallExpr.Name), and neither is a member or a map key, so none of those is
// visited.
func v1FreeNames(n ast.ExpressionNode, bound map[string]bool, visit func(*ast.IdentExpr)) {
	switch e := n.(type) {
	case nil:
	case *ast.IdentExpr:
		if !bound[e.Name] {
			visit(e)
		}
	case *ast.LambdaExpr:
		inner := make(map[string]bool, len(bound))
		for k := range bound {
			inner[k] = true
		}
		for _, p := range e.Params {
			inner[p] = true
		}
		v1FreeNames(e.Body, inner, visit)
	case *ast.MemberExpr:
		v1FreeNames(e.Object, bound, visit)
	case *ast.CallExpr:
		v1FreeNames(e.Receiver, bound, visit)
		for _, a := range e.Args {
			v1FreeNames(a, bound, visit)
		}
		for _, na := range e.Named {
			v1FreeNames(na.Value, bound, visit)
		}
	case *ast.UnaryExpr:
		v1FreeNames(e.Operand, bound, visit)
	case *ast.BinaryExpr:
		v1FreeNames(e.Left, bound, visit)
		v1FreeNames(e.Right, bound, visit)
	case *ast.ListExpr:
		for _, el := range e.Elems {
			v1FreeNames(el, bound, visit)
		}
	case *ast.MapExpr:
		for _, en := range e.Entries {
			v1FreeNames(en.Value, bound, visit)
		}
	case *ast.ParenExpr:
		v1FreeNames(e.Inner, bound, visit)
	case *ast.TernaryExpr:
		v1FreeNames(e.Condition, bound, visit)
		v1FreeNames(e.Then, bound, visit)
		v1FreeNames(e.Else, bound, visit)
	}
}

// ---------------------------------------------------------------------------
// G5 (memql#2367): retired event.payload reads -- source-level scan
// ---------------------------------------------------------------------------

// eventPayloadReadPattern matches a live `event.<anything>` (or the
// `$event.<anything>` dollar form) read in authored automation source.
//
// DELIBERATELY WIDER THAN `event.payload.` (memql#3610). It used to match that
// prefix alone, which made it blind to the one spelling authors actually
// reached for: `event.node.payload.X` reads exactly like the shape of a
// graph-node event, and there is no `node` key anywhere in the envelope the CDC
// publisher builds -- so it resolved to nothing, the filter decided false, and
// the automation never fired. That is how the computer-use kill switch came to
// be inert, and how per-Plan workbench directories stopped being torn down.
//
// The narrow pattern could not have caught it: the broken spelling was not the
// retired one. Any dotted read off `event` is now refused, because in an
// automation body the payload binds to the args { } contract and is read
// args.<field>. A bare `event` passed along as a call argument
// (`logic f(event: event)`) carries no dot and is unaffected, as are logic
// bodies, which read `args.event.payload.X` on a different surface.
var eventPayloadReadPattern = regexp.MustCompile(`[$]?\bevent\.[A-Za-z_]`)

// scrubSourceForPayloadScan blanks string literals AND both comment forms so
// the retirement scan never fires on prose (@description text, header
// comments).
//
// The `/*` arm was missing (memql#2872 review). It went unreachable-from-above
// until the preamble walk started carrying block-comment bodies into the slice;
// after that, an ordinary note like
//
//	/* before #2367 this read event.payload.status directly */
//
// above an automation refused the whole tree -- and the diagnostic blamed the
// automation for a read that exists only in a comment. Byte-for-byte the class
// the $steps. gate fix closed.
//
// NOT replaced with BlankComments: this scrubber deliberately also blanks
// STRING LITERALS, which BlankComments leaves intact by design (it exists so
// header detectors see a comment-free view, not a literal-free one). The two
// answer different questions, so this keeps its own scan and just grows the
// missing arm.
func scrubSourceForPayloadScan(s string) string {
	var out strings.Builder
	i := 0
	for i < len(s) {
		switch {
		case s[i] == '"':
			// Escape state is TRACKED, not inferred from the preceding byte
			// (memql#2949). A one-byte lookback cannot tell an escaped quote
			// from a quote that follows a COMPLETED `\\` escape, so a literal
			// ending in a backslash pair read its own closing quote as escaped
			// and consumed on to the next quote -- blanking source that was
			// never inside a literal and failing the G5 scan OPEN.
			//
			// A literal MAY span lines, so this does NOT stop at a newline.
			// The lexer's scanString (component/language/parser/lexer.go) has
			// no newline case: it writes `\n` into the literal like any other
			// byte, so `"one<NL>two"` is ONE valid string token. An earlier
			// version of this arm stopped at `\n` on the belief that the
			// grammar forbade multi-line literals; it does not, and that guard
			// broke the gate in BOTH directions -- prose in a wrapped
			// @description was scanned as code (false refusal), and the
			// literal's real closing quote was re-read as an OPENING quote,
			// blanking a genuine retired read after it (the very fail-open
			// this function exists to prevent). memql#2949 review.
			//
			// An UNBALANCED quote -- no closing quote anywhere before EOF --
			// still costs only its own line rather than the rest of the file,
			// which is what memql#2949 asked for. Note the lexer refuses such
			// source outright ("unterminated string"), so compileMemQL aborts
			// before this scan ever runs; the fallback is belt-and-braces for
			// callers that scan source the parser has not accepted.
			//
			// Newlines inside the consumed span are PRESERVED, exactly as the
			// `/*` arm below does, so line accounting is unchanged.
			j := i + 1
			escaped := false
			closed := false
			firstNL := -1
			for j < len(s) {
				c := s[j]
				if c == '\n' && firstNL < 0 {
					firstNL = j
				}
				j++
				if escaped {
					escaped = false
					continue
				}
				if c == '\\' {
					escaped = true
					continue
				}
				if c == '"' {
					closed = true
					break // closing quote, consumed above
				}
			}
			if !closed && firstNL >= 0 {
				j = firstNL // unbalanced: stop at the newline, leave it to the default arm
			}
			for k := i; k < j; k++ {
				if s[k] == '\n' {
					out.WriteByte('\n')
				} else {
					out.WriteByte(' ')
				}
			}
			i = j
		case strings.HasPrefix(s[i:], "//"):
			j := strings.IndexByte(s[i:], '\n')
			if j < 0 {
				j = len(s) - i
			}
			out.WriteString(strings.Repeat(" ", j))
			i += j
		case strings.HasPrefix(s[i:], "/*"):
			// Newlines are preserved so the scan's line accounting is
			// unchanged; everything else in the span becomes a space. An
			// unterminated block comment runs to EOF, matching the lexer.
			end := strings.Index(s[i+2:], "*/")
			j := len(s) - i
			if end >= 0 {
				j = end + 4
			}
			for k := i; k < i+j; k++ {
				if s[k] == '\n' {
					out.WriteByte('\n')
				} else {
					out.WriteByte(' ')
				}
			}
			i += j
		default:
			out.WriteByte(s[i])
			i++
		}
	}
	return out.String()
}
