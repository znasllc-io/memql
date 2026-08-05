package memql

// Owner-field provenance for row-authz (memql#2982).
//
// `@rowAuthz(owner="F")` asserts that F identifies the row's owner.
// That assertion is worthless -- worse, actively misleading -- if a
// caller can write F. This file answers, for a concept and a candidate
// field: does the SERVER stamp it, and can a caller ever displace it?
//
// # WHY THIS READS TEMPLATES AND NOT SOURCE
//
// The obvious check is "is F named in an `accept { ... }` block". It
// misses the live cases, and one of them it cannot see at all:
//
//   - `appendDocumentVersion` wrote a bare `args.ownerUserId` mirror in
//     a longhand `insert { }` with no `accept` block anywhere (memql#2989,
//     since fixed -- the shape remains expressible).
//   - `updateCalendarEvent` splats `args.payload` with NO overlay, so F
//     is caller-writable without appearing anywhere near an `accept`
//     block (memql#2988).
//
// `updateNote` and `updateCalendarEvent` are near-identical in source
// and differ entirely in whether memql#401's overlay-wins protection
// engages -- because that turns on which explicit fields survive the
// loader's hoist-and-delete pass into PayloadOverlayTemplate. A source
// scanner would have to re-derive that rule; the loaded template
// already carries its outcome. This is memql#2875's lesson a third
// time: derive from the loaded form, because the source spelling and
// the runtime behaviour are different questions.

import (
	"fmt"
	"sort"
	"strings"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// OwnerProvenance is the verdict on one (concept, field) pair.
type OwnerProvenance struct {
	Concept string `json:"concept"`
	Field   string `json:"field"`
	// ServerStamped is true only when every clause holds: some mutation
	// stamps the field from the actor, none accepts it from caller
	// args, every payload splat re-stamps it, and nothing was
	// unparseable.
	ServerStamped bool `json:"serverStamped"`
	// Reason explains a false verdict in the terms needed to act on it,
	// naming the mutation responsible.
	Reason string `json:"reason,omitempty"`
	// StampedBy names the mutations that stamp the field from the actor.
	StampedBy []string `json:"stampedBy,omitempty"`
	// WritableBy names the mutations through which a caller can write
	// the field. Non-empty implies ServerStamped is false.
	WritableBy []string `json:"writableBy,omitempty"`
}

// valueProvenance is how one template value gets its content.
type valueProvenance int

const (
	// provNone -- the value mentions neither caller args nor the actor
	// (a literal, `now`, a computed constant).
	provNone valueProvenance = iota
	// provStamp -- derived from the actor and not from caller args.
	provStamp
	// provAccept -- derived from caller args. A value mentioning BOTH
	// folds to this: `args.ownerUserId ?? actor.userId` is caller-
	// controllable, so the safe reading is the caller's.
	provAccept
	// provUnknown -- could not be classified. Treated as provAccept by
	// every caller; kept distinct so the diagnostic can say "could not
	// parse" rather than falsely asserting the caller controls it.
	provUnknown
)

// classifyTemplateValue decides where a rendered template value's
// content comes from.
//
// RECURSIVE, and deliberately so even though the tree has no live
// compound owner write today. A whole-string prefix test happens to
// give the right answer on every current mutation; it is one authored
// `ownerUserId: args.ownerUserId ?? actor.userId` away from silently
// passing a forgeable field, and that is precisely the shape an author
// reaches for when a mutation needs to serve two call paths.
//
// Quoted literals are stripped first: a description string containing
// the word "args." is prose, not a reference.
func classifyTemplateValue(v any) valueProvenance {
	switch t := v.(type) {
	case nil:
		return provNone
	case string:
		return classifyExpressionText(t)
	case map[string]any:
		// A nested object: the field is caller-controllable if ANY leaf
		// is, and stamped only if some leaf is stamped and none is
		// accepted.
		return foldProvenance(func(yield func(valueProvenance)) {
			for _, e := range t {
				yield(classifyTemplateValue(e))
			}
		})
	case []any:
		return foldProvenance(func(yield func(valueProvenance)) {
			for _, e := range t {
				yield(classifyTemplateValue(e))
			}
		})
	case bool, int, int64, float64:
		return provNone
	case langparser.ExpressionNode:
		return classifyParserExpression(t)
	default:
		// FAIL CLOSED. An earlier version rendered the value with
		// fmt.Sprintf("%v") and classified the result as text, which is
		// fail-OPEN in the one case that matters: a lowered
		// *ast.ArgRefExpr{Path:"ownerUserId"} prints as `&{ownerUserId}`,
		// which contains neither "args." nor "actor.userId", so a value
		// that IS a caller reference classified as provNone -- it then
		// contributed to neither StampedBy nor WritableBy, and a sibling
		// mutation that stamped the field carried the concept to a PASS.
		//
		// AST nodes in a value slot are a supported runtime shape:
		// evalValue has an explicit `case languageParser.ExpressionNode`
		// arm, IDTemplate is a lowered node for most mutations today, and
		// memql#2840 was precisely an actor reference landing in one.
		return provUnknown
	}
}

// classifyParserExpression walks a lowered expression node.
//
// It mirrors evalParserExpression's arms rather than re-deriving them:
// what this needs to know is exactly what that function resolves from,
// so the two must agree about which nodes read caller args and which
// read the actor. Anything it does not recognise returns provUnknown,
// so a new node kind surfaces as "could not classify" instead of
// silently reading as "no reference".
func classifyParserExpression(expr langparser.ExpressionNode) valueProvenance {
	switch t := expr.(type) {
	case nil:
		return provNone
	case *langparser.LiteralExpr:
		return provNone
	case *langparser.ArgRefExpr:
		// The parser routes BOTH `args.X` and `actor.X` through
		// ArgRefExpr with the prefix as the only discriminator, which is
		// the memql#2840 trap. Read the prefix the same way
		// evalParserExpression does.
		if strings.HasPrefix(t.Path, "actor.") {
			if strings.TrimPrefix(t.Path, "actor.") == "userId" {
				return provStamp
			}
			// Some other actor field: server-derived, but not the owner
			// identity. Not a caller reference either.
			return provNone
		}
		return provAccept
	case *langparser.VarRefExpr:
		// An engine variable: server-side, and not the actor.
		return provNone
	case *langparser.TimestampExprFunc:
		return provNone
	case *langparser.ConcatExpr:
		return foldProvenance(func(yield func(valueProvenance)) {
			for _, a := range t.Args {
				yield(classifyParserExpression(a))
			}
		})
	case *langparser.HashExpr:
		return classifyParserExpression(t.Target)
	case *langparser.CanonicalIdExpr:
		return classifyParserExpression(t.Value)
	default:
		return provUnknown
	}
}

func foldProvenance(each func(func(valueProvenance))) valueProvenance {
	out := provNone
	each(func(p valueProvenance) {
		switch {
		case p == provAccept || out == provAccept:
			out = provAccept
		case p == provUnknown || out == provUnknown:
			out = provUnknown
		case p == provStamp:
			out = provStamp
		}
	})
	return out
}

// classifyExpressionText classifies a rendered expression.
//
// Caller-reference wins over actor-reference: a value mentioning both
// is caller-controllable in at least one evaluation, and the point of
// this analysis is what a caller CAN do.
func classifyExpressionText(text string) valueProvenance {
	bare := stripQuotedLiterals(text)
	callerRef := strings.Contains(bare, "args.") || strings.Contains(bare, "ctx.")
	actorRef := strings.Contains(bare, "actor.userId")
	switch {
	case callerRef:
		return provAccept
	case actorRef:
		return provStamp
	default:
		return provNone
	}
}

// stripQuotedLiterals blanks the contents of double-quoted strings so
// prose inside a description cannot read as a reference.
func stripQuotedLiterals(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inStr := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inStr && c == '\\' && i+1 < len(s):
			b.WriteString("  ")
			i++
		case c == '"':
			inStr = !inStr
			b.WriteByte(c)
		case inStr:
			b.WriteByte(' ')
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// isPayloadSplat reports whether a PayloadTemplate is a whole-object
// splat (`args.payload`) rather than a field-by-field object literal.
//
// This is the distinction memql#401's overlay exists for, and the one
// that separates `updateNote` (safe) from `updateCalendarEvent`
// (memql#2988).
func isPayloadSplat(payload any) bool {
	if payload == nil {
		return false
	}
	_, isMap := payload.(map[string]any)
	return !isMap
}

// OwnerFieldProvenance decides, for every concept declaring an owner
// tier, whether that field is genuinely server-stamped.
//
// registry must be a loaded function registry; concepts is the loaded
// concept registry keyed by canonical id. Returns one verdict per
// declared owner tier, sorted by concept.
func OwnerFieldProvenance(registry *FunctionRegistry, declared map[string]string) []OwnerProvenance {
	if registry == nil || len(declared) == 0 {
		return nil
	}

	byConcept := map[string][]*Function{}
	for _, fn := range registry.List() {
		if fn == nil || fn.FunctionKind != "mutation" || fn.MutationTemplate == nil {
			continue
		}
		// NOTE: a @serverOnly mutation is deliberately still counted as a
		// caller path. A landing-review pass skipped them here, reasoning that
		// the annotation means "args cannot come from a caller by
		// construction". That is false: expandFunctionCall (engine.go) gates
		// on auth.OriginFromContext -- the origin of the CALL -- and never
		// inspects where a trusted Go call site got its args. updateUser is
		// the live proof: component/identity/admin/handlers.go stamps internal
		// origin on a REQUEST-DERIVED context and passes a userId read from an
		// HTTP form field.
		//
		// The repo already adjudicated this. call_origin.go says "Never call
		// it in a request handler on a context derived from an inbound
		// request", and call_origin_conformance_test.go allowlists the admin
		// package as a KNOWN EXCEPTION whose precondition is asserted by a
		// separate per-package test. So arg provenance is guaranteed by that
		// discipline, not by the annotation -- and a gate that assumed
		// otherwise would go quiet on exactly the shape the workertoken
		// allowlist entry warns about.
		c := strings.TrimSpace(fn.BoundConcept)
		if c == "" {
			c = strings.TrimSpace(fn.MutationTemplate.Concept)
		}
		if c == "" {
			continue
		}
		byConcept[c] = append(byConcept[c], fn)
	}

	out := make([]OwnerProvenance, 0, len(declared))
	for concept, field := range declared {
		out = append(out, ownerProvenanceFor(concept, field, byConcept[concept]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Concept < out[j].Concept })
	return out
}

// ownerProvenanceFor applies the five clauses to one (concept, field).
func ownerProvenanceFor(concept, field string, mutations []*Function) OwnerProvenance {
	res := OwnerProvenance{Concept: concept, Field: field}

	if len(mutations) == 0 {
		// No write path at all. Nothing can forge the field, but
		// nothing stamps it either, so the tier rests on seed data.
		// Report rather than pass silently.
		res.Reason = "no mutation writes this concept, so nothing stamps the owner field; " +
			"the tier rests on however the rows were seeded"
		return res
	}

	for _, fn := range mutations {
		tmpl := fn.MutationTemplate
		name := fn.Name

		// The SELF-OWNED clause (memql#3029). This file previously carried a
		// note explaining why it was deliberately ABSENT -- validateRowAuthz
		// required the owner to name a declared property, `id` is a reserved
		// intrinsic no concept can declare, so `@rowAuthz(owner="id")` could
		// not load and "a clause that cannot fire is not a safeguard, it is a
		// claim in the shape of one".
		//
		// That reasoning was right, and memql#3029 is exactly what invalidates
		// its premise: the self-owned form loads now, so the clause can fire
		// and has to exist. Restored rather than re-invented.
		//
		// For `owner="id"` the question is not "who writes a payload field"
		// but "can a caller write the ROW'S ID" -- so the subject is
		// IDTemplate, classified by the same classifier the payload arms use.
		// A lowered AST node there is the normal shape, and classifyTemplateValue
		// fails CLOSED on one it cannot read (memql#2840), which is the
		// behaviour this gate wants.
		// Compared as a literal, deliberately. The definition of this value
		// is langparser.RowAuthzSelfOwnedField, but naming that symbol here
		// puts this file on the row-authz surface, and TestRowAuthzIsInert
		// then demands it be added to the allow-list. That gate is a safety
		// property about ENFORCEMENT, and this file is a static analyzer that
		// a test drives -- widening the list for it would trade a real
		// invariant for a cosmetic one. memql#3029 requires the gate to stay
		// green and unmodified, so the literal stays and the constant is
		// named here in prose instead.
		if field == "id" {
			switch classifyTemplateValue(tmpl.IDTemplate) {
			case provStamp:
				res.StampedBy = append(res.StampedBy, name)
			case provAccept:
				res.WritableBy = append(res.WritableBy, name)
				if res.Reason == "" {
					res.Reason = fmt.Sprintf("%s takes the row id from caller args, so a caller "+
						"chooses which row is written", name)
				}
			case provUnknown:
				res.WritableBy = append(res.WritableBy, name)
				if res.Reason == "" {
					res.Reason = fmt.Sprintf("%s builds the row id from an expression this "+
						"analyzer cannot classify; failing closed", name)
				}
			}
			continue
		}

		explicit, hasExplicit := templateFieldValue(tmpl.PayloadTemplate, field)
		overlay, hasOverlay := tmpl.PayloadOverlayTemplate[field]

		switch {
		case hasOverlay:
			// Clause 1/2 via the overlay, which wins on collision.
			switch classifyTemplateValue(overlay) {
			case provStamp:
				res.StampedBy = append(res.StampedBy, name)
			case provAccept:
				res.WritableBy = append(res.WritableBy, name)
				if res.Reason == "" {
					res.Reason = fmt.Sprintf("%s writes the owner field from caller args", name)
				}
			case provUnknown:
				res.WritableBy = append(res.WritableBy, name)
				if res.Reason == "" {
					res.Reason = fmt.Sprintf("%s writes the owner field from an expression this "+
						"analyzer cannot classify; failing closed", name)
				}
			}

		case hasExplicit:
			switch classifyTemplateValue(explicit) {
			case provStamp:
				res.StampedBy = append(res.StampedBy, name)
			case provAccept:
				res.WritableBy = append(res.WritableBy, name)
				if res.Reason == "" {
					res.Reason = fmt.Sprintf("%s writes the owner field from caller args", name)
				}
			case provUnknown:
				res.WritableBy = append(res.WritableBy, name)
				if res.Reason == "" {
					res.Reason = fmt.Sprintf("%s writes the owner field from an expression this "+
						"analyzer cannot classify; failing closed", name)
				}
			}

		case isPayloadSplat(tmpl.PayloadTemplate):
			// CLAUSE 3, and the one no source-level check catches.
			//
			// The mutation splats a caller-supplied object whole and
			// does NOT re-stamp the owner field on top of it. memql#401's
			// overlay-wins protection is populated only from explicit
			// block fields, so with no overlay entry the caller's value
			// for this field lands in the row unchallenged.
			res.WritableBy = append(res.WritableBy, name)
			if res.Reason == "" {
				res.Reason = fmt.Sprintf("%s splats a caller-supplied payload with no overlay "+
					"re-stamping the owner field, so a caller can set it directly (memql#401's "+
					"overlay-wins protection only covers explicit block fields)", name)
			}
		}
	}

	res.ServerStamped = len(res.WritableBy) == 0 && len(res.StampedBy) > 0
	if !res.ServerStamped && res.Reason == "" && len(res.StampedBy) == 0 {
		res.Reason = "no mutation stamps the owner field from actor.userId"
	}
	return res
}

// templateFieldValue returns a named field's template from an object
// literal PayloadTemplate.
func templateFieldValue(payload any, field string) (any, bool) {
	m, ok := payload.(map[string]any)
	if !ok {
		return nil, false
	}
	v, present := m[field]
	return v, present
}
