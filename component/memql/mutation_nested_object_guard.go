package memql

import (
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/language/ast"
)

// mutation_nested_object_guard.go -- memql#3617.
//
// A mutation that REBUILDS a nested object out of optional args destroys every
// leaf it was not passed, silently.
//
// The three facts that compose into the defect are each individually correct:
//
//  1. An absent optional arg resolves to missingValue{}, and evalValue's map
//     branch OMITS that key rather than writing a null. Right on its own -- a
//     null would be a value the caller never sent.
//  2. The read-merge inherits fields the delta OMITS, per top-level payload
//     field. Right on its own -- that is what makes a partial write partial.
//  3. A top-level object field is therefore replaced WHOLESALE, because the
//     delta did carry the key; only its interior shrank. @mergeFields is the
//     annotation that turns that replace into a deep merge.
//
// Put together, a legal call that omits one optional leaf writes an object
// missing that leaf, and the stored leaf is gone. memql#3605 is what that costs
// when the leaf is `credentials.backupEligible`: it reads back false, a synced
// authenticator asserts true, and WebAuthn refuses the login.
//
// The shape is detectable from the template alone, which is what this guard
// does. It is deliberately narrow:
//
//   - Only OBJECT LITERAL field templates. `credentials: args.credentials`
//     hands the whole object over as the caller supplied it, and replacing it
//     wholesale is exactly right; the tree has sixteen of those and none is a
//     defect.
//   - Only objects with MORE THAN ONE leaf. A single-leaf object has no
//     sibling to lose.
//   - Only leaves that are a BARE single-segment reference to a declared
//     OPTIONAL arg. `args.x ?? "d"` always produces a value so it cannot drop a
//     key, and a dotted path (`args.action.payload`) projects a caller-supplied
//     blob whose interior this mutation never declared.

// nestedObjectLeafRefs walks an object-literal field template and returns the
// bare single-segment arg names it reads, plus the total number of leaves.
//
// Leaves are counted across nesting, but only the TOP-LEVEL payload field is
// the merge unit -- mergePayloadFields keys on top-level names -- so a deeper
// object's leaves still belong to the top-level field that contains them.
func nestedObjectLeafRefs(tmpl map[string]any) (argNames []string, leafCount int) {
	for _, v := range tmpl {
		switch t := v.(type) {
		case map[string]any:
			inner, innerLeaves := nestedObjectLeafRefs(t)
			argNames = append(argNames, inner...)
			leafCount += innerLeaves
		default:
			leafCount++
			if name, ok := bareArgReference(v); ok {
				argNames = append(argNames, name)
			}
		}
	}
	return argNames, leafCount
}

// bareArgReference reports whether a template value is exactly `args.<name>` or
// `ctx.<name>` for a single-segment name, and returns that name. Anything with
// a dot in the path, a call wrapper, or a non-string template is not one.
func bareArgReference(v any) (string, bool) {
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	trimmed := strings.TrimSpace(s)
	var path string
	switch {
	case strings.HasPrefix(trimmed, "args."):
		path = strings.TrimPrefix(trimmed, "args.")
	case strings.HasPrefix(trimmed, "ctx."):
		path = strings.TrimPrefix(trimmed, "ctx.")
	default:
		return "", false
	}
	if path == "" || strings.ContainsAny(path, ". ()[]{}\",") {
		return "", false
	}
	return path, true
}

// optionalArgNames collects the declared args a function marks optional.
func optionalArgNames(fn *Function) map[string]struct{} {
	out := map[string]struct{}{}
	if fn == nil || fn.ArgsSchema == nil {
		return out
	}
	for _, f := range fn.ArgsSchema.Fields {
		if f != nil && f.Optional {
			out[f.Name] = struct{}{}
		}
	}
	return out
}

// destructiveNestedObjectFields returns the top-level payload fields a mutation
// rebuilds from optional args without declaring them merge-semantic. An empty
// result means the mutation cannot silently drop a stored leaf.
func destructiveNestedObjectFields(fn *Function) []string {
	if fn == nil || fn.MutationTemplate == nil {
		return nil
	}
	optional := optionalArgNames(fn)
	if len(optional) == 0 {
		return nil
	}
	merged := map[string]struct{}{}
	for _, f := range fn.MutationTemplate.MergeFields {
		merged[strings.TrimSpace(f)] = struct{}{}
	}

	var offenders []string
	consider := func(field string, value any) {
		obj, isObj := value.(map[string]any)
		if !isObj {
			return
		}
		if _, isMerged := merged[field]; isMerged {
			return
		}
		refs, leaves := nestedObjectLeafRefs(obj)
		if leaves < 2 {
			return
		}
		for _, name := range refs {
			if _, isOptional := optional[name]; isOptional {
				offenders = append(offenders, field)
				return
			}
		}
	}

	if payload, ok := fn.MutationTemplate.PayloadTemplate.(map[string]any); ok {
		for field, value := range payload {
			consider(field, value)
		}
	}
	for field, value := range fn.MutationTemplate.PayloadOverlayTemplate {
		consider(field, value)
	}
	sort.Strings(offenders)
	return offenders
}

// validateNestedObjectMergeSemantics reports a mutation whose template can
// silently destroy a stored leaf, naming the remedy that applies to ITS kind --
// they differ, because @mergeFields is a load-time error on an insert.
//
// ADVISORY, not a refusal. The loader logs it and moves on
// (unified_functions_loader.go): the remedy needs an author's judgement per
// case -- deep-merge, required leaves, or "wholesale replace is what this field
// means" -- and refusing boot would turn a latent hazard in a runtime-mounted
// product bundle into an outage. The engine's own tree is gated by
// TestNestedObjectFromOptionalArgs_InventoryIsPinned.
func validateNestedObjectMergeSemantics(fn *Function) error {
	offenders := destructiveNestedObjectFields(fn)
	if len(offenders) == 0 {
		return nil
	}
	remedy := `name the field in @mergeFields("` + offenders[0] + `") so the write deep-merges, ` +
		"or mark the leaf args @required"
	if fn.MutationTemplate.Kind == "" || fn.MutationTemplate.Kind == ast.MutationKindInsert {
		remedy = "mark the leaf args @required (@mergeFields is a load-time error on an " +
			"insert, so the destroying call has to be made unspellable instead)"
	}
	return fmt.Errorf(
		"rebuilds nested object field(s) %s from optional args without merge semantics: an "+
			"absent optional arg omits its leaf, and the top-level field is then written "+
			"wholesale, so every stored leaf the caller did not pass is destroyed with no "+
			"error (memql#3617). Fix: %s",
		strings.Join(offenders, ", "), remedy)
}
