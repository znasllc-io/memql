package automations

// precondition.go implements first-class automation preconditions
// (Epic 4 / memql#2139): the deterministic check whose miss is the clean
// self-healing repair trigger AND the cross-machine portability signal.
//
// A `precondition` block lives inside the `automation NAME { ... }` body
// alongside `step` blocks:
//
//	automation deployStaging {
//	  precondition envIsStaging {
//	    check: $config.MEMQL_ENV == "staging"
//	    literal: MEMQL_ENV
//	    description: "Only drive the staging deploy spine in staging."
//	  }
//	  step run { logic driveDeployment { event: event } }
//	}
//
// The struct-form automation rewriter (component/language/parser) only
// understands `step` blocks, so we extract + strip the precondition
// blocks here BEFORE the source reaches the rewriter, parse them into the
// Precondition struct, and re-attach them to the compiled Automation.
// This keeps the precondition a first-class construct without widening
// the core grammar. The check expression is evaluated at run time by the
// same deterministic boolean evaluator that powers Step.Condition.

import (
	"fmt"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"regexp"
	"strings"
)

// preconditionBlockHeader matches the opening of a `precondition NAME {`
// block at any indentation inside an automation body.
var preconditionBlockHeader = regexp.MustCompile(`(?m)^[ \t]*precondition[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)

// preconditionFieldPattern matches a `key: value` line inside a
// precondition block. Value runs to end-of-line; quotes are stripped by
// trimPreconditionValue so authors may quote or not.
var preconditionFieldPattern = regexp.MustCompile(`(?m)^[ \t]*(check|literal|description)[ \t]*:[ \t]*(.+?)[ \t]*$`)

// extractPreconditions finds every `precondition NAME { ... }` block in
// the automation source, parses each into a Precondition, and returns the
// list plus the source with those blocks removed (so the rewriter never
// sees them). Source with no precondition blocks is returned unchanged
// with a nil slice -- the common case carries zero overhead.
func extractPreconditions(source string) ([]*Precondition, string, error) {
	// Detect on a COMMENT-BLANKED view (memql#2872). This is the fourth
	// raw-text scan on the automation path, and it was the one missed when the
	// other three were fixed -- the commit that fixed them claimed "every gate
	// that scans raw construct text scans a comment-blanked view", and that
	// claim was false because of this function.
	//
	// It was unreachable from above the automation header until the preamble
	// walk started carrying comment bodies into the slice. After that, a
	// COMMENTED-OUT precondition became a LIVE one: measured end to end, a
	// `/* precondition envIsStaging { check: ... } */` above the automation
	// loaded with preconditions=1, no WARN. And it fails CLOSED -- a
	// precondition miss aborts the automation before any step runs -- so
	// commenting a precondition out ENFORCED it. The stripped source was left
	// as `/*\n\n*/`, still brace-balanced, so nothing downstream complained.
	//
	// BlankComments preserves byte offsets, so every index below indexes the
	// original identically: headers and braces are found on `scan`, bodies and
	// the stripped output are cut from `source`.
	scan := languageParser.BlankComments(source)
	matches := preconditionBlockHeader.FindAllStringSubmatchIndex(scan, -1)
	if len(matches) == 0 {
		return nil, source, nil
	}

	var out []*Precondition
	// Build the stripped source by copying everything OUTSIDE the
	// precondition blocks. Walk matches in order; each match gives the
	// header span, and we find its matching close brace to get the block
	// end.
	var stripped strings.Builder
	cursor := 0
	for _, m := range matches {
		headerStart := m[0]
		headerEnd := m[1]
		nameStart := m[2]
		nameEnd := m[3]
		name := source[nameStart:nameEnd]

		openIdx := strings.IndexByte(scan[headerStart:headerEnd], '{')
		if openIdx < 0 {
			return nil, source, fmt.Errorf("precondition %q: missing opening brace", name)
		}
		openIdx += headerStart
		closeIdx := matchingCloseBrace(scan, openIdx)
		if closeIdx < 0 {
			return nil, source, fmt.Errorf("precondition %q: missing closing brace", name)
		}

		// FIELDS are matched on the blanked view too (memql#2872 review).
		// parsePreconditionBody assigns on every regex match, so LAST MATCH
		// WINS -- a commented-out `check:` AFTER the live one silently
		// replaced it:
		//
		//	precondition p {
		//	  check: config.MEMQL_ENV == "staging"
		//	  /*
		//	  check: config.MEMQL_ENV == "PARKED"
		//	  */
		//	}
		//
		// measured as check="...PARKED". Ordering-dependent, so a commented
		// block BEFORE the live line was harmless and one AFTER it won. This
		// half is pre-existing (body comments were always inside the slice),
		// but it is the same defect in the same function and fixing only the
		// detection half would leave it half-done. Blanked lines cannot match
		// the anchored field pattern; captured VALUES are unaffected because
		// blanking never touches live source.
		body := scan[openIdx+1 : closeIdx]
		pc, err := parsePreconditionBody(name, body)
		if err != nil {
			return nil, source, err
		}
		out = append(out, pc)

		// Copy the gap before this block into the stripped source; skip
		// the block itself.
		stripped.WriteString(source[cursor:headerStart])
		cursor = closeIdx + 1
	}
	stripped.WriteString(source[cursor:])

	return out, stripped.String(), nil
}

// parsePreconditionBody reads the `key: value` fields of a precondition
// block body into a Precondition. `check` is required; `literal` and
// `description` are optional.
func parsePreconditionBody(name, body string) (*Precondition, error) {
	pc := &Precondition{ID: name}
	for _, fm := range preconditionFieldPattern.FindAllStringSubmatch(body, -1) {
		key := fm[1]
		val := trimPreconditionValue(fm[2])
		switch key {
		case "check":
			pc.Check = val
		case "literal":
			pc.Literal = val
		case "description":
			pc.Description = val
		}
	}
	if strings.TrimSpace(pc.Check) == "" {
		return nil, fmt.Errorf("precondition %q: a `check:` boolean expression is required", name)
	}
	return pc, nil
}

// trimPreconditionValue strips surrounding whitespace and a single pair
// of matching quotes from a precondition field value. The check
// expression is left otherwise verbatim so the evaluator sees the same
// grammar a Step.Condition would.
func trimPreconditionValue(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			// Only strip when the quotes wrap the WHOLE value -- a check
			// like `$config.X == "y"` must keep its inner quotes.
			inner := v[1 : len(v)-1]
			if !strings.ContainsAny(inner, "\"'") {
				return inner
			}
		}
	}
	return v
}

// matchingCloseBrace returns the index of the brace that closes the one
// at openIdx (which must point at a '{'), or -1 if unbalanced. A
// self-contained copy of the parser's helper (which is unexported), kept
// local so the automations package does not import parser internals.
// String literals are honored so a `{`/`}` inside a quoted check value
// does not skew the depth count.
func matchingCloseBrace(s string, openIdx int) int {
	depth := 0
	inStr := false
	var quote byte
	for i := openIdx; i < len(s); i++ {
		c := s[i]
		if inStr {
			if c == '\\' {
				i++ // skip the escaped char
				continue
			}
			if c == quote {
				inStr = false
			}
			continue
		}
		switch c {
		case '"', '\'':
			inStr = true
			quote = c
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// validatePreconditions rejects a malformed precondition set at load
// time: a missing id, a missing check, or a duplicate id within one
// automation. Catching these at load keeps the run-time miss path simple
// (every loaded precondition is well-formed) and surfaces authoring
// mistakes via the full-DSL load-test rather than a silent no-op.
func validatePreconditions(preconditions []*Precondition) error {
	seen := make(map[string]struct{}, len(preconditions))
	for _, pc := range preconditions {
		if pc == nil {
			continue
		}
		if strings.TrimSpace(pc.ID) == "" {
			return fmt.Errorf("precondition: missing id")
		}
		if strings.TrimSpace(pc.Check) == "" {
			return fmt.Errorf("precondition %q: missing check expression", pc.ID)
		}
		if _, dup := seen[pc.ID]; dup {
			return fmt.Errorf("precondition %q: duplicate id within automation", pc.ID)
		}
		seen[pc.ID] = struct{}{}
	}
	return nil
}

// EvaluatePreconditions runs each precondition's deterministic check
// against the supplied evaluator (the same one the steps use, already
// loaded with $event / $input / $config / $var context). It returns the
// FIRST precondition that misses (evaluates false or errors), or nil when
// all hold. A check that errors is treated as a miss -- the conservative
// choice for a self-healing trigger: an unevaluable literal on this
// machine is exactly the cross-machine drift a precondition exists to
// catch. The boolean second return is true when a miss occurred.
func EvaluatePreconditions(preconditions []*Precondition, eval *Evaluator) (*Precondition, bool) {
	for _, pc := range preconditions {
		if pc == nil || strings.TrimSpace(pc.Check) == "" {
			continue
		}
		ok, err := eval.EvaluateCondition(pc.Check)
		if err != nil || !ok {
			return pc, true
		}
	}
	return nil, false
}
