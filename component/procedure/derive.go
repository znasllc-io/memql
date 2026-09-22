package procedure

import (
	"errors"
	"fmt"
	"strings"
)

// Derivation is a CHECKED expression explaining where a hole's value comes
// from. It is the answer shape of the one bounded model call of D6, and the
// only thing this module will accept from a model.
type Derivation struct {
	// Expr is the source, kept so a rejection can be read back.
	Expr string
	root *derivNode
}

// String returns the expression as written.
func (d Derivation) String() string { return d.Expr }

// IsZero reports whether nothing was parsed.
func (d Derivation) IsZero() bool { return d.root == nil }

type derivKind uint8

const (
	derivLiteral derivKind = iota
	derivRef
	derivCall
)

type derivNode struct {
	kind derivKind
	lit  string
	// ref
	step int
	path []string
	// call
	fn   string
	args []*derivNode
}

// allowedFns is the CLOSED function set, and its closedness is the safety
// property of this whole file.
//
// A model proposes an expression; this module decides whether the proposal
// holds. That decision is only meaningful because there is no expression the
// model can write that does anything other than read a recorded result and
// reshape a string. An open grammar -- anything with arbitrary calls, an eval,
// a file read -- would be code execution by another name, inside the one
// module whose claim is that it reaches nothing.
var allowedFns = map[string]int{
	"basename": 1,
	"dirname":  1,
	"lower":    1,
	"upper":    1,
	"trim":     1,
	"concat":   -1, // variadic, at least one
}

// ErrNotDerivation is returned for anything outside the closed grammar. It is
// an ordinary rejection: the caller leaves the hole free and learns the
// procedure anyway, because a model that answered badly must cost the run
// nothing.
var ErrNotDerivation = errors.New("procedure: not a derivation expression")

// ParseDerivation parses the closed grammar:
//
//	expr    := literal | ref | call
//	literal := "..."
//	ref     := ref( <stepIndex> [, "path"]... )
//	call    := basename(expr) | dirname(expr) | lower(expr) | upper(expr)
//	         | trim(expr) | concat(expr, ...)
func ParseDerivation(expr string) (Derivation, error) {
	p := &derivParser{src: strings.TrimSpace(expr)}
	if p.src == "" {
		return Derivation{}, fmt.Errorf("%w: empty", ErrNotDerivation)
	}
	n, err := p.parseExpr()
	if err != nil {
		return Derivation{}, err
	}
	p.skipSpace()
	if p.pos != len(p.src) {
		return Derivation{}, fmt.Errorf("%w: trailing %q", ErrNotDerivation, p.src[p.pos:])
	}
	return Derivation{Expr: expr, root: n}, nil
}

// CheckDerivation reports whether the derivation reproduces the hole's value
// in EVERY instance, and on how many it held.
//
// Both halves matter. "Every" is the acceptance rule of D13: a proposal that
// explains some instances and not others is rejected and the hole stays free.
// The COUNT is what lets a rejection be read -- a bare false says only that
// the model was wrong, and "held on 4 of 5" is the thing an operator looking
// at a stubborn hole actually needs.
func CheckDerivation(d Derivation, h Hole, instances [][]Action) (ok bool, heldOn int) {
	if d.IsZero() || len(instances) == 0 {
		return false, 0
	}
	if !refsOnlyEarlierSteps(d.root, h.StepIndex) {
		// A procedure cannot depend on a step that has not run yet.
		return false, 0
	}
	for _, in := range instances {
		if h.StepIndex >= len(in) {
			continue
		}
		want, found := in[h.StepIndex].Args.At(h.Path)
		if !found || want.Kind != KindLit {
			continue
		}
		got, evalOK := evalDeriv(d.root, in)
		if evalOK && got == want.Lit {
			heldOn++
		}
	}
	return heldOn == len(instances), heldOn
}

func refsOnlyEarlierSteps(n *derivNode, holeStep int) bool {
	switch n.kind {
	case derivRef:
		return n.step >= 0 && n.step < holeStep
	case derivCall:
		for _, a := range n.args {
			if !refsOnlyEarlierSteps(a, holeStep) {
				return false
			}
		}
	}
	return true
}

func evalDeriv(n *derivNode, in []Action) (string, bool) {
	switch n.kind {
	case derivLiteral:
		return n.lit, true
	case derivRef:
		if n.step >= len(in) {
			return "", false
		}
		for _, p := range resultPaths(in[n.step].ResultValue, nil) {
			if pathsEqual(p.path, n.path) {
				return p.value, true
			}
		}
		return "", false
	default:
		args := make([]string, 0, len(n.args))
		for _, a := range n.args {
			v, ok := evalDeriv(a, in)
			if !ok {
				return "", false
			}
			args = append(args, v)
		}
		switch n.fn {
		case "basename":
			s := args[0]
			if i := strings.LastIndex(s, "/"); i >= 0 {
				s = s[i+1:]
			}
			return s, true
		case "dirname":
			s := args[0]
			if i := strings.LastIndex(s, "/"); i > 0 {
				return s[:i], true
			}
			return "", true
		case "lower":
			return strings.ToLower(args[0]), true
		case "upper":
			return strings.ToUpper(args[0]), true
		case "trim":
			return strings.TrimSpace(args[0]), true
		case "concat":
			return strings.Join(args, ""), true
		}
		return "", false
	}
}

func pathsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- the parser -----------------------------------------------------------

type derivParser struct {
	src string
	pos int
}

func (p *derivParser) skipSpace() {
	for p.pos < len(p.src) && (p.src[p.pos] == ' ' || p.src[p.pos] == '\t' || p.src[p.pos] == '\n') {
		p.pos++
	}
}

func (p *derivParser) parseExpr() (*derivNode, error) {
	p.skipSpace()
	if p.pos >= len(p.src) {
		return nil, fmt.Errorf("%w: unexpected end", ErrNotDerivation)
	}
	if p.src[p.pos] == '"' {
		s, err := p.parseString()
		if err != nil {
			return nil, err
		}
		return &derivNode{kind: derivLiteral, lit: s}, nil
	}
	name, err := p.parseIdent()
	if err != nil {
		return nil, err
	}
	if err := p.expect('('); err != nil {
		return nil, err
	}
	if name == "ref" {
		return p.parseRefTail()
	}
	arity, allowed := allowedFns[name]
	if !allowed {
		return nil, fmt.Errorf("%w: %q is not in the closed function set", ErrNotDerivation, name)
	}
	var args []*derivNode
	for {
		a, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		args = append(args, a)
		p.skipSpace()
		if p.pos < len(p.src) && p.src[p.pos] == ',' {
			p.pos++
			continue
		}
		break
	}
	if err := p.expect(')'); err != nil {
		return nil, err
	}
	if arity >= 0 && len(args) != arity {
		return nil, fmt.Errorf("%w: %s takes %d argument(s), got %d", ErrNotDerivation, name, arity, len(args))
	}
	return &derivNode{kind: derivCall, fn: name, args: args}, nil
}

func (p *derivParser) parseRefTail() (*derivNode, error) {
	p.skipSpace()
	start := p.pos
	for p.pos < len(p.src) && p.src[p.pos] >= '0' && p.src[p.pos] <= '9' {
		p.pos++
	}
	if start == p.pos {
		return nil, fmt.Errorf("%w: ref's first argument is a step index", ErrNotDerivation)
	}
	step := 0
	for _, r := range p.src[start:p.pos] {
		step = step*10 + int(r-'0')
	}
	var path []string
	for {
		p.skipSpace()
		if p.pos < len(p.src) && p.src[p.pos] == ',' {
			p.pos++
			s, err := p.parseString()
			if err != nil {
				return nil, err
			}
			path = append(path, s)
			continue
		}
		break
	}
	if err := p.expect(')'); err != nil {
		return nil, err
	}
	return &derivNode{kind: derivRef, step: step, path: path}, nil
}

func (p *derivParser) parseIdent() (string, error) {
	p.skipSpace()
	start := p.pos
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			p.pos++
			continue
		}
		break
	}
	if start == p.pos {
		return "", fmt.Errorf("%w: expected a function name at %d", ErrNotDerivation, p.pos)
	}
	return p.src[start:p.pos], nil
}

func (p *derivParser) parseString() (string, error) {
	p.skipSpace()
	if p.pos >= len(p.src) || p.src[p.pos] != '"' {
		return "", fmt.Errorf("%w: expected a quoted string at %d", ErrNotDerivation, p.pos)
	}
	p.pos++
	var b strings.Builder
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		switch c {
		case '\\':
			if p.pos+1 >= len(p.src) {
				return "", fmt.Errorf("%w: dangling escape", ErrNotDerivation)
			}
			b.WriteByte(p.src[p.pos+1])
			p.pos += 2
		case '"':
			p.pos++
			return b.String(), nil
		default:
			b.WriteByte(c)
			p.pos++
		}
	}
	return "", fmt.Errorf("%w: unterminated string", ErrNotDerivation)
}

func (p *derivParser) expect(c byte) error {
	p.skipSpace()
	if p.pos >= len(p.src) || p.src[p.pos] != c {
		return fmt.Errorf("%w: expected %q at %d", ErrNotDerivation, string(c), p.pos)
	}
	p.pos++
	return nil
}
