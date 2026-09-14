package ast

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// FormatExpr prints an edition-2026 expression as canonical source: one space
// around every binary operator and around `?` and `:`, `x => body` for one
// lambda parameter and `(a, b) => body` otherwise, no space inside brackets.
//
// Parenthesis rule: a ParenExpr prints its parentheses; beyond those, a child is
// parenthesised exactly when the grammar would otherwise read it differently.
// So a tree the parser produced prints back to text that re-parses to the same
// tree, and a tree built by hand -- a rewrite, a migration -- prints to text
// that means what the tree means. The second property is the one the codemod
// rests on.
//
// A node outside the edition-2026 set prints as "<?Type>", which re-parses to
// nothing; callers print only v1 trees.
func FormatExpr(n ExpressionNode) string {
	var b strings.Builder
	formatInto(&b, n)
	return b.String()
}

// Precedence levels, loosest first, matching the parser's table (D9).
const (
	precLambda = iota
	precTernary
	precOr
	precAnd
	precCompare
	precCoalesce
	precAdditive
	precMultiplicative
	precUnary
	precPostfix
)

// binaryPrec returns the level of a binary operator.
func binaryPrec(op string) int {
	switch op {
	case "||":
		return precOr
	case "&&":
		return precAnd
	case "==", "!=", "<", "<=", ">", ">=", "in", "startsWith":
		return precCompare
	case "??":
		return precCoalesce
	case "+", "-":
		return precAdditive
	case "*", "/", "%":
		return precMultiplicative
	}
	return precPostfix
}

// nodePrec is the level a node binds at when it appears as an operand.
func nodePrec(n ExpressionNode) int {
	switch e := n.(type) {
	case *LambdaExpr:
		return precLambda
	case *TernaryExpr:
		return precTernary
	case *BinaryExpr:
		return binaryPrec(e.Op)
	case *UnaryExpr:
		return precUnary
	case *LiteralExpr:
		// A negative number literal is `-5` in source, which binds like a
		// unary minus, so it is parenthesised where a unary would be.
		switch v := e.Value.(type) {
		case int64:
			if v < 0 {
				return precUnary
			}
		case int:
			if v < 0 {
				return precUnary
			}
		case float64:
			if v < 0 {
				return precUnary
			}
		}
	}
	return precPostfix
}

// formatChild prints child, parenthesised when its level is below min.
func formatChild(b *strings.Builder, child ExpressionNode, min int) {
	if nodePrec(child) < min {
		b.WriteByte('(')
		formatInto(b, child)
		b.WriteByte(')')
		return
	}
	formatInto(b, child)
}

func formatInto(b *strings.Builder, n ExpressionNode) {
	switch e := n.(type) {
	case nil:
		// Nothing: an absent optional child.
	case *IdentExpr:
		b.WriteString(e.Name)
	case *MemberExpr:
		formatChild(b, e.Object, precPostfix)
		if e.Optional {
			b.WriteString(".?")
		} else {
			b.WriteByte('.')
		}
		b.WriteString(e.Field)
	case *CallExpr:
		if e.Kind != "" {
			b.WriteString(e.Kind)
			b.WriteByte(' ')
		}
		if e.Receiver != nil {
			formatChild(b, e.Receiver, precPostfix)
			b.WriteByte('.')
		}
		b.WriteString(e.Name)
		b.WriteByte('(')
		first := true
		for _, a := range e.Args {
			if !first {
				b.WriteString(", ")
			}
			first = false
			// An argument is a fresh expression context: nothing binds
			// across the comma, so only a lambda-looser node needs parens,
			// and none exists.
			formatInto(b, a)
		}
		for _, a := range e.Named {
			if !first {
				b.WriteString(", ")
			}
			first = false
			b.WriteString(a.Name)
			b.WriteString(": ")
			formatInto(b, a.Value)
		}
		b.WriteByte(')')
	case *UnaryExpr:
		b.WriteString(e.Op)
		formatChild(b, e.Operand, precUnary)
	case *BinaryExpr:
		p := binaryPrec(e.Op)
		if p == precCompare {
			// Non-associative: a comparison operand of a comparison is
			// always parenthesised.
			formatChild(b, e.Left, precCompare+1)
			b.WriteByte(' ')
			b.WriteString(e.Op)
			b.WriteByte(' ')
			formatChild(b, e.Right, precCompare+1)
			return
		}
		// Left-associative: the left child may sit at the same level,
		// the right child must bind tighter.
		formatChild(b, e.Left, p)
		b.WriteByte(' ')
		b.WriteString(e.Op)
		b.WriteByte(' ')
		formatChild(b, e.Right, p+1)
	case *TernaryExpr:
		// Right-associative. The condition must bind tighter than a
		// ternary; the then-branch is parenthesised when it is itself a
		// ternary, for the reader; the else-branch chains bare.
		formatChild(b, e.Condition, precTernary+1)
		b.WriteString(" ? ")
		formatChild(b, e.Then, precTernary+1)
		b.WriteString(" : ")
		formatChild(b, e.Else, precTernary)
	case *LambdaExpr:
		if len(e.Params) == 1 {
			b.WriteString(e.Params[0])
		} else {
			b.WriteByte('(')
			b.WriteString(strings.Join(e.Params, ", "))
			b.WriteByte(')')
		}
		b.WriteString(" => ")
		formatInto(b, e.Body)
	case *ListExpr:
		b.WriteByte('[')
		for i, el := range e.Elems {
			if i > 0 {
				b.WriteString(", ")
			}
			formatInto(b, el)
		}
		b.WriteByte(']')
	case *MapExpr:
		b.WriteByte('{')
		for i, en := range e.Entries {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(en.Key)
			b.WriteString(": ")
			formatInto(b, en.Value)
		}
		b.WriteByte('}')
	case *ParenExpr:
		b.WriteByte('(')
		formatInto(b, e.Inner)
		b.WriteByte(')')
	case *LiteralExpr:
		b.WriteString(FormatLiteral(e.Value))
	case *NilExpr:
		b.WriteString("nil")
	default:
		b.WriteString("<?")
		b.WriteString(strings.TrimPrefix(fmt.Sprintf("%T", n), "*ast."))
		b.WriteByte('>')
	}
}

// FormatLiteral prints a literal value the lexer reads back to the same value:
// a string quoted with the lexer's own escape set, an integer in decimal, a
// float with a fractional part or an exponent (so it cannot re-read as an
// integer), a boolean as true/false.
func FormatLiteral(v any) string {
	switch x := v.(type) {
	case string:
		return QuoteString(x)
	case bool:
		return strconv.FormatBool(x)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		s := strconv.FormatFloat(x, 'g', -1, 64)
		if !strings.ContainsAny(s, ".eEn") { // "n": NaN/Inf never reach here from source
			s += ".0"
		}
		return s
	case nil:
		return "nil"
	}
	return "<?literal>"
}

// QuoteString quotes s using exactly the escapes the MemQL lexer decodes
// (`\"`, `\\`, `\b`, `\f`, `\n`, `\r`, `\t`, `\uXXXX`). strconv.Quote is not
// usable: it emits `\a`, `\v`, `\x..` and `\U........`, which the lexer
// refuses. A non-BMP character is written literally (the lexer is
// rune-oriented), and any other control character as `\u00XX`.
func QuoteString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 || r == 0x7f || r == utf8.RuneError {
				b.WriteString(`\u`)
				h := strconv.FormatInt(int64(r), 16)
				b.WriteString(strings.Repeat("0", 4-len(h)))
				b.WriteString(h)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
