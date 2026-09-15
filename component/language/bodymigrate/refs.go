package bodymigrate

// refs.go -- rewriting the references inside a body's expressions
// (epic memql#5370, task memql#5373).
//
// A name is its value in edition 2026 (D12), so every spelling that reached a
// step's value THROUGH something retires: `steps.x.result`, `x.result`, the
// three `.result` climbs into an action's envelope, the capitalised accessor
// methods, `first(x)`. Rows read their payload fields directly
// (`rows.first().email`), and an automation reads its args as `args.x` like a
// logic always has -- the G2 bare reading and G5's `event.payload.x` both land
// on `args.x`.
//
// The rewrite scans TOKENS, not the expression grammar: identifiers, the
// dotted and call tails that make a chain, strings, comments and brackets. So
// it reads the tree on either side of epic 2's expression rewrite, and it
// never re-prints an expression it did not change -- spacing, comments and
// operator spelling survive byte for byte outside the chains it rewrites.

import (
	"fmt"
	"strings"
)

// refContext is what one body's expressions are rewritten against.
type refContext struct {
	construct string            // "automation" | "logic"
	steps     map[string]string // statement name -> what it binds: "action", "query", "mutation", "logic", "builtin", "automation", "expr", "publish"
	args      map[string]bool   // an automation's declared args fields
	loopVars  map[string]bool   // loop variables in scope
	// eventFields collects every f an automation read as event.payload.f,
	// which the rewrite reads as args.f and so must declare.
	eventFields map[string]bool
	// logicEvent is set while a publishing logic's statements are written
	// into the automation that called it with `event: event`: the logic's
	// `args.event.payload.f` is then the automation's `args.f`.
	logicEvent bool
}

func (rc *refContext) withLoopVar(v string) *refContext {
	cp := *rc
	cp.loopVars = map[string]bool{}
	for k := range rc.loopVars {
		cp.loopVars[k] = true
	}
	cp.loopVars[v] = true
	return &cp
}

// ---- tokens ----------------------------------------------------------------------

type rtok struct {
	kind byte // 'i' identifier, 's' string, 'n' number, 'c' comment, 'w' space, 'o' operator or bracket
	text string
}

var multiOps = []string{"=>", "==", "!=", "<=", ">=", "&&", "||", "??", ":=", ".?"}

// tokenizeExpr splits expression text into tokens; concatenating their text
// reproduces src exactly.
func tokenizeExpr(src string) []rtok {
	var out []rtok
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			j := i
			for j < len(src) && (src[j] == ' ' || src[j] == '\t' || src[j] == '\n' || src[j] == '\r') {
				j++
			}
			out = append(out, rtok{'w', src[i:j]})
			i = j
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			j := strings.IndexByte(src[i:], '\n')
			if j < 0 {
				j = len(src) - i
			}
			out = append(out, rtok{'c', src[i : i+j]})
			i += j
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			j := strings.Index(src[i+2:], "*/")
			end := len(src)
			if j >= 0 {
				end = i + 2 + j + 2
			}
			out = append(out, rtok{'c', src[i:end]})
			i = end
		case c == '"':
			j := i + 1
			for j < len(src) {
				if src[j] == '\\' {
					j += 2
					continue
				}
				if src[j] == '"' {
					j++
					break
				}
				j++
			}
			if j > len(src) {
				j = len(src)
			}
			out = append(out, rtok{'s', src[i:j]})
			i = j
		case isIdentStartByte(c):
			j := i + 1
			for j < len(src) && isIdentByte(src[j]) {
				j++
			}
			out = append(out, rtok{'i', src[i:j]})
			i = j
		case c >= '0' && c <= '9':
			j := i + 1
			for j < len(src) && (src[j] >= '0' && src[j] <= '9' || src[j] == '.' && j+1 < len(src) && src[j+1] >= '0' && src[j+1] <= '9') {
				j++
			}
			out = append(out, rtok{'n', src[i:j]})
			i = j
		default:
			op := string(c)
			for _, m := range multiOps {
				if strings.HasPrefix(src[i:], m) {
					op = m
					break
				}
			}
			out = append(out, rtok{'o', op})
			i += len(op)
		}
	}
	return out
}

// significant skips whitespace and comments from i in direction dir and
// returns the index of the next significant token, or -1.
func significant(toks []rtok, i, dir int) int {
	for i >= 0 && i < len(toks) {
		if toks[i].kind != 'w' && toks[i].kind != 'c' {
			return i
		}
		i += dir
	}
	return -1
}

// matchTok returns the index of the token closing the bracket at open.
func matchTok(toks []rtok, open int) int {
	depth := 0
	for i := open; i < len(toks); i++ {
		if toks[i].kind != 'o' {
			continue
		}
		switch toks[i].text {
		case "(", "[", "{":
			depth++
		case ")", "]", "}":
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// words that are never references.
var refKeywords = map[string]bool{
	"true": true, "false": true, "nil": true, "null": true, "in": true, "startsWith": true,
	"if": true, "else": true, "for": true, "range": true, "return": true, "switch": true, "case": true,
	"default": true, "and": true, "or": true, "not": true, "where": true,
}

// roots are the reserved names a body reads without declaring.
var refRoots = map[string]bool{
	"args": true, "actor": true, "now": true, "config": true, "partition": true, "trace": true,
	"event": true, "input": true, "ctx": true, "item": false, "index": true, "var": true,
	"secret": true, "systemVar": true, "systemSecret": true, "automation": true,
}

// seg is one link of a reference chain: `.name`, `.?name`, an optional call
// `(...)`, and any `[...]` index after it.
type seg struct {
	name     string
	optional bool
	call     bool
	args     []rtok // the tokens inside the call parens, rewritten separately
	index    []string
}

// rewriteRefs rewrites one expression's text.
func rewriteRefs(src string, rc *refContext) (string, error) {
	return rc.rewriteTokens(tokenizeExpr(src), func(string) bool { return false })
}

// rewriteTokens rewrites a token run. outer reports the lambda parameters an
// enclosing expression has in scope.
func (rc *refContext) rewriteTokens(toks []rtok, outer func(string) bool) (string, error) {
	var b strings.Builder
	type level struct {
		open    string
		isMap   bool
		atStart bool
		bound   map[string]bool
	}
	stack := []level{{bound: map[string]bool{}, atStart: true}}
	top := func() *level { return &stack[len(stack)-1] }
	isBound := func(name string) bool {
		for i := len(stack) - 1; i >= 0; i-- {
			if stack[i].bound[name] {
				return true
			}
		}
		return outer(name)
	}
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		switch t.kind {
		case 'w', 'c', 's', 'n':
			b.WriteString(t.text)
			if t.kind != 'w' && t.kind != 'c' {
				top().atStart = false
			}
			continue
		case 'o':
			switch t.text {
			case "(", "[", "{":
				stack = append(stack, level{open: t.text, isMap: t.text == "{", atStart: true, bound: map[string]bool{}})
			case ")", "]", "}":
				if len(stack) > 1 {
					stack = stack[:len(stack)-1]
				}
				top().atStart = false
			case ",":
				top().atStart = true
				// A lambda's parameter scope ends at its argument's comma.
				top().bound = map[string]bool{}
			default:
				top().atStart = false
			}
			b.WriteString(t.text)
			continue
		}
		// An identifier.
		prev := significant(toks, i-1, -1)
		next := significant(toks, i+1, 1)
		if prev >= 0 && toks[prev].kind == 'o' && (toks[prev].text == "." || toks[prev].text == ".?") {
			b.WriteString(t.text) // a field or method name, not a root
			continue
		}
		// A key or a named argument: `name:` at the start of an element.
		if next >= 0 && toks[next].kind == 'o' && toks[next].text == ":" && top().atStart && len(stack) > 1 {
			b.WriteString(t.text)
			top().atStart = false
			continue
		}
		if refKeywords[t.text] {
			b.WriteString(t.text)
			top().atStart = false
			continue
		}
		// A lambda parameter: `x => ...`.
		if next >= 0 && toks[next].kind == 'o' && toks[next].text == "=>" {
			top().bound[t.text] = true
			b.WriteString(t.text)
			continue
		}
		// A construct call inside an expression: `query foo(...)`.
		if _, isKind := kindWords[t.text]; isKind && next >= 0 && toks[next].kind == 'i' {
			after := significant(toks, next+1, 1)
			if after >= 0 && toks[after].text == "(" {
				b.WriteString(t.text)
				for k := i + 1; k <= next; k++ {
					b.WriteString(toks[k].text)
				}
				i = next
				top().atStart = false
				continue
			}
		}
		// A function call: `name(...)`.
		if next == i+1 && toks[next].kind == 'o' && toks[next].text == "(" {
			closeIdx := matchTok(toks, next)
			if closeIdx < 0 {
				return "", fmt.Errorf("unbalanced parentheses after %s", t.text)
			}
			if (t.text == "first" || t.text == "last") && rc.isStepCallArg(toks[next+1:closeIdx]) {
				// first(x) / last(x) on a step: the accessor method.
				arg := strings.TrimSpace(joinToks(toks[next+1 : closeIdx]))
				rewritten, err := rc.rewriteChainText(arg+"."+t.text+"()", isBound)
				if err != nil {
					return "", err
				}
				b.WriteString(rewritten)
				i = closeIdx
				top().atStart = false
				continue
			}
			b.WriteString(t.text)
			top().atStart = false
			continue
		}
		// A chain rooted here.
		segs, end, err := parseChain(toks, i)
		if err != nil {
			return "", err
		}
		afterChain := significant(toks, end+1, 1)
		shorthand := len(stack) > 1 && top().isMap && top().atStart && !(afterChain >= 0 && toks[afterChain].text == ":")
		key := segs[len(segs)-1].name
		if shorthand && segs[len(segs)-1].call {
			return "", fmt.Errorf("map entry %s has no key", joinToks(toks[i:end+1]))
		}
		text, err := rc.rewriteChain(segs, isBound)
		if err != nil {
			return "", err
		}
		if shorthand {
			// `{ a.b.c }` is the shorthand entry `c: a.b.c`; edition 2026 map
			// literals spell every key.
			text = key + ": " + text
		}
		b.WriteString(text)
		top().atStart = false
		i = end
	}
	return b.String(), nil
}

func joinToks(toks []rtok) string {
	var b strings.Builder
	for _, t := range toks {
		b.WriteString(t.text)
	}
	return b.String()
}

// isStepCallArg reports whether a call's argument tokens are exactly one step name.
func (rc *refContext) isStepCallArg(toks []rtok) bool {
	s := strings.TrimSpace(joinToks(toks))
	_, ok := rc.steps[s]
	return ok && isBareIdent(s)
}

// parseChain reads a chain starting at the identifier at i: `a`, then any of
// `.f`, `.?f`, `(...)` on the last link, `[...]`. It returns the links and the
// index of the chain's last token.
func parseChain(toks []rtok, i int) ([]seg, int, error) {
	segs := []seg{{name: toks[i].text}}
	j := i + 1
	for j < len(toks) {
		t := toks[j]
		switch {
		case t.kind == 'o' && (t.text == "." || t.text == ".?") && j+1 < len(toks) && toks[j+1].kind == 'i':
			segs = append(segs, seg{name: toks[j+1].text, optional: t.text == ".?"})
			j += 2
		case t.kind == 'o' && t.text == "(":
			closeIdx := matchTok(toks, j)
			if closeIdx < 0 {
				return nil, 0, fmt.Errorf("unbalanced parentheses in %s", joinToks(toks[i:]))
			}
			last := &segs[len(segs)-1]
			if last.call {
				return segs, j - 1, nil
			}
			last.call = true
			last.args = toks[j+1 : closeIdx]
			j = closeIdx + 1
		case t.kind == 'o' && t.text == "[":
			closeIdx := matchTok(toks, j)
			if closeIdx < 0 {
				return nil, 0, fmt.Errorf("unbalanced brackets in %s", joinToks(toks[i:]))
			}
			last := &segs[len(segs)-1]
			last.index = append(last.index, joinToks(toks[j:closeIdx+1]))
			j = closeIdx + 1
		default:
			return segs, j - 1, nil
		}
	}
	return segs, j - 1, nil
}

// rewriteChainText is rewriteChain over a chain written as text.
func (rc *refContext) rewriteChainText(text string, isBound func(string) bool) (string, error) {
	toks := tokenizeExpr(text)
	segs, _, err := parseChain(toks, 0)
	if err != nil {
		return "", err
	}
	return rc.rewriteChain(segs, isBound)
}

var accessorLower = map[string]string{
	"First": "first", "Last": "last", "Empty": "empty", "Nodes": "nodes", "Len": "count",
	"first": "first", "last": "last", "empty": "empty", "nodes": "nodes", "count": "count",
}

// rewriteChain applies the reference rules to one chain.
func (rc *refContext) rewriteChain(in []seg, isBound func(string) bool) (string, error) {
	segs := append([]seg(nil), in...)
	root := segs[0].name
	if segs[0].call {
		// A call on the root itself is a function call (handled by the caller);
		// reaching here means `name(...)` followed a chain link. Emit as is.
		return rc.emitChain(segs, isBound)
	}
	full := func() string { s, _ := rc.emitChain(segs, isBound); return s }
	switch {
	case isBound(root):
		// A lambda parameter: nothing to rewrite at the root.
	case root == "steps" && !rc.isStep("steps"):
		if len(segs) < 3 || segs[2].name != "result" || segs[1].call {
			return "", fmt.Errorf("%s has no statement spelling (a statement's value is its name; .status, .error and .metadata have no successor)", full())
		}
		name := segs[1].name
		if _, ok := rc.steps[name]; !ok {
			return "", fmt.Errorf("%s names no step of this body", full())
		}
		rest := segs[3:]
		if segs[2].call || len(segs[2].index) > 0 {
			return "", fmt.Errorf("%s: an accessor on .result has no statement spelling", full())
		}
		out := []seg{{name: name}}
		stripped, err := rc.stripClimbs(name, rest, full)
		if err != nil {
			return "", err
		}
		segs = append(out, stripped...)
	case rc.isStep(root):
		if len(segs) > 1 && segs[1].name == "result" && !segs[1].call && len(segs[1].index) == 0 {
			stripped, err := rc.stripClimbs(root, segs[2:], full)
			if err != nil {
				return "", err
			}
			segs = append([]seg{segs[0]}, stripped...)
		} else if len(segs) > 1 && segs[1].name == "Ran" {
			return "", fmt.Errorf("%s has no statement spelling: test the name against nil", full())
		}
	case rc.loopVars[root]:
		if len(segs) > 2 && segs[1].name == "payload" && !segs[1].call && !segs[1].optional {
			segs = append([]seg{segs[0]}, segs[2:]...)
		}
	case rc.logicEvent && root == "args":
		if len(segs) >= 2 && segs[1].name == "event" && !segs[1].call {
			if len(segs) == 2 {
				segs = []seg{{name: "event"}}
				break
			}
			if len(segs) >= 4 && segs[2].name == "payload" && !segs[2].call && !segs[3].call {
				field := segs[3].name
				rc.eventFields[field] = true
				segs = append([]seg{{name: "args"}, {name: field, optional: segs[3].optional, index: segs[3].index}}, segs[4:]...)
				break
			}
			return "", fmt.Errorf("%s reads the event envelope, which an automation reads only through its args", full())
		}
		return "", fmt.Errorf("%s: the inlined logic reads an argument its automation does not pass", full())
	case rc.construct == "automation" && (root == "event" || root == "payload") && !rc.args[root]:
		evSegs := segs
		if root == "event" {
			if len(segs) == 1 {
				break // `event` as a whole value stays
			}
			if segs[1].name != "payload" || len(segs) < 3 || segs[1].call {
				break // event.topic / event.kind: G5 refuses them; nothing to rewrite
			}
			evSegs = segs[1:]
		}
		if len(evSegs) < 2 || evSegs[1].call {
			return "", fmt.Errorf("%s reads the whole payload, which has no args spelling", full())
		}
		field := evSegs[1].name
		rc.eventFields[field] = true
		segs = append([]seg{{name: "args"}, {name: field, optional: evSegs[1].optional, index: evSegs[1].index}}, evSegs[2:]...)
	case rc.construct == "automation" && rc.args[root]:
		segs = append([]seg{{name: "args"}}, segs...)
	}
	// Anywhere in the chain: capitalised accessors lower, and a row read
	// through .first() / .last() drops .payload.
	for k := 1; k < len(segs); k++ {
		if low, ok := accessorLower[segs[k].name]; ok && segs[k].call && strings.TrimSpace(joinToks(segs[k].args)) == "" {
			segs[k].name = low
		}
		if segs[k].call && (segs[k].name == "first" || segs[k].name == "last") &&
			k+2 < len(segs) && segs[k+1].name == "payload" && !segs[k+1].call && len(segs[k+1].index) == 0 {
			segs = append(segs[:k+1], segs[k+2:]...)
		}
	}
	return rc.emitChain(segs, isBound)
}

// isStep reports whether name is a statement name of this body.
func (rc *refContext) isStep(name string) bool {
	_, ok := rc.steps[name]
	return ok && !rc.loopVars[name]
}

// stripClimbs removes an action value's envelope climbs: an action statement's
// value is its capability's result, so `x.result.result.f` (the capability
// envelope, then the script's result) becomes `x.f`.
func (rc *refContext) stripClimbs(step string, rest []seg, full func() string) ([]seg, error) {
	if rc.steps[step] != "action" {
		return rest, nil
	}
	if len(rest) >= 2 && rest[0].name == "result" && rest[1].name == "result" && !rest[0].call && !rest[1].call {
		return rest[2:], nil
	}
	if len(rest) == 0 {
		return rest, nil
	}
	return nil, fmt.Errorf("%s reads an action's envelope, which has no statement spelling: an action's value is its capability's result, reached as %s.<field>", full(), step)
}

// emitChain prints a chain, rewriting call arguments recursively.
func (rc *refContext) emitChain(segs []seg, isBound func(string) bool) (string, error) {
	var b strings.Builder
	for k, s := range segs {
		if k > 0 {
			if s.optional {
				b.WriteString(".?")
			} else {
				b.WriteString(".")
			}
		}
		b.WriteString(s.name)
		if s.call {
			inner, err := rc.rewriteTokens(s.args, isBound)
			if err != nil {
				return "", err
			}
			b.WriteString("(" + inner + ")")
		}
		for _, ix := range s.index {
			b.WriteString(ix)
		}
	}
	return b.String(), nil
}
