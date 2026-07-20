package sense

import (
	"strings"

	"github.com/znasllc-io/memql/component/language/parser"
)

// The ONE enclosing-construct detector (#2626). Sense previously could
// not answer "what construct is the cursor inside?" -- three ad-hoc
// depth computations approximated it (a forward net-depth scan for
// signature positions, a backward unmatched-brace boolean, and a
// bespoke line scan for automation args), and CursorContext.ReceiverType
// was declared, read by the annotation completer, and never assigned,
// leaving the per-receiver filter dead.
//
// Lexer asymmetry the detector must respect: the lowercase construct
// keywords (query/mutate/logic/automation/shape/...) lex as plain
// IDENTIFIERS -- only `concept` and `use` are keyword tokens -- so
// header matching compares identifier literals against the dslspec
// construct set (dslSpec.ConstructByKeyword), never token types.

// EnclosingConstruct describes the construct the cursor sits inside
// (or, for a top-level annotation, the construct it precedes).
type EnclosingConstruct struct {
	// Keyword is the author-facing construct keyword (query, mutate,
	// logic, automation, concept, shape, ...).
	Keyword string
	// Receiver is the annotations-registry receiver key for that
	// construct (Query, Mutation, Logic, ...), empty when the construct
	// has no registry-backed receiver.
	Receiver string
	// Name is the construct's declared name when the header carries one
	// (the second identifier for concept-bound signatures).
	Name string
	// Concept is the signature-bound concept short name for a
	// concept-binding construct (`query <Concept> <name>`), empty
	// otherwise (#2628 uses it for field completion).
	Concept string
	// Blocks is the chain of enclosing named blocks INSIDE the construct
	// body, outermost first (args, filter, insert, stamp, step, ...).
	// Empty when the cursor sits directly in the construct body.
	Blocks []string
	// Preamble is true when the construct was resolved by looking DOWN
	// from a top-level annotation position rather than by enclosure.
	Preamble bool
}

// resolveEnclosingConstruct finds the construct enclosing the cursor by
// walking the token stream before the cursor, tracking brace depth and
// remembering the header that opened each still-open brace. The
// innermost unmatched brace whose opener is a construct header is the
// answer; the named blocks opened after it form the block chain.
func resolveEnclosingConstruct(tokens []parser.Token) (EnclosingConstruct, bool) {
	type frame struct {
		label     string // the identifier immediately before this `{`
		name      string // a second identifier before that (signature form)
		concept   string // the middle identifier of a concept-bound signature
		construct bool
	}
	var stack []frame
	for i, t := range tokens {
		switch t.Type {
		case parser.TokenBraceOpen:
			f := frame{}
			// The label is the identifier run preceding this brace.
			// `query todo todos {` -> label "query", name "todos";
			// `args {` -> label "args".
			var idents []string
			j := i - 1
			for ; j >= 0 && len(idents) < 3; j-- {
				tk := tokens[j]
				if tk.Type == parser.TokenIdentifier || tk.Type == parser.TokenKeywordConcept {
					idents = append([]string{tk.Literal}, idents...)
					continue
				}
				break
			}
			// A preceding `@name` annotation ends in an identifier that is
			// NOT part of the header (`@actor` above `shape x {` would
			// otherwise read as the label). Drop it.
			if j >= 0 && tokens[j].Type == parser.TokenAt && len(idents) > 0 {
				idents = idents[1:]
			}
			if len(idents) > 0 {
				f.label = idents[0]
				f.name = idents[len(idents)-1]
				if len(idents) >= 3 {
					f.concept = idents[1] // `query <Concept> <name> {`
				}
				if dslSpec.ConstructByKeyword(f.label) != nil {
					f.construct = true
				}
			}
			stack = append(stack, f)
		case parser.TokenBraceClose:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	// Innermost construct frame wins; blocks are the frames above it.
	for i := len(stack) - 1; i >= 0; i-- {
		if !stack[i].construct {
			continue
		}
		out := EnclosingConstruct{Keyword: stack[i].label, Name: stack[i].name, Concept: stack[i].concept}
		if c := dslSpec.ConstructByKeyword(out.Keyword); c != nil {
			out.Receiver = c.AnnotationReceiver
		}
		for _, f := range stack[i+1:] {
			if f.label != "" {
				out.Blocks = append(out.Blocks, f.label)
			}
		}
		return out, true
	}
	return EnclosingConstruct{}, false
}

// resolvePreambleConstruct answers the annotation case: a top-level `@`
// PRECEDES its construct, so the receiver is the next construct header
// BELOW the cursor. Returns false at EOF (no header follows), which
// preserves the union fallback in the annotation completer.
func resolvePreambleConstruct(source string, line int) (EnclosingConstruct, bool) {
	lines := strings.Split(source, "\n")
	for i := line - 1; i < len(lines); i++ {
		if i < 0 {
			continue
		}
		fields := strings.Fields(strings.TrimSpace(lines[i]))
		if len(fields) == 0 {
			continue
		}
		kw := fields[0]
		c := dslSpec.ConstructByKeyword(kw)
		if c == nil {
			continue
		}
		out := EnclosingConstruct{Keyword: kw, Receiver: c.AnnotationReceiver, Preamble: true}
		if len(fields) > 1 {
			out.Name = strings.TrimSuffix(fields[len(fields)-1], "{")
		}
		return out, true
	}
	return EnclosingConstruct{}, false
}
