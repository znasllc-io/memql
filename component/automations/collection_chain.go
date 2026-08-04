package automations

import (
	"strings"

	"github.com/znasllc-io/memql/component/memql"
)

// collection_chain.go extends the Story 4 (#2302 / ADR §2.2) in-memory
// collection / lambda surface (`component/memql/collection_method.go`) into the
// remaining automation-path contexts: multi-statement logic bodies (#2317) and
// the forEach filter + outer-arg substitution (#2318). It reuses the shared
// `memql.EvaluateCollectionChainString` evaluator -- it never re-implements the
// method chain -- and only adds the automations-side wiring: base-receiver
// resolution against the local Evaluator, query-result-envelope normalisation,
// and outer-arg threading.

// genuineCollectionMethods is the collection-operator set MINUS the four names
// that lexically overlap the step-result accessors (`first` / `last` / `count`
// / `empty`, resolved by resolvePath's step-accessor cases). A source carrying
// only one of those overlapping accessors over a known step (`rows.count()`) is
// a step-result accessor, NOT a collection chain, and must keep its legacy
// resolution. Kept in sync with memql.collectionMethodNames.
var genuineCollectionMethods = []string{
	"where", "select", "single", "any", "all",
	"sum", "min", "max", "avg",
	"orderby", "orderbydesc", "take", "skip",
	"distinct", "groupby", "reduce", "contains",
}

// isGenuineCollectionChain reports whether src is UNAMBIGUOUSLY a Story 4
// collection-method / lambda chain -- it carries an arrow lambda (`=>`) or a
// non-accessor collection operator call (`.where(`, `.select(`, ...). A bare
// `step.count()` / `step.first()` / `step.empty()` / `step.last()` is
// deliberately NOT genuine so it stays on the legacy step-result-accessor path
// (logic_runner's tryEvaluateReturnLocally + resolvePath). This is stricter
// than memql.LooksLikeCollectionChain on purpose: it gates the NEW logic-body /
// forEach-filter short-circuits without hijacking existing single-method
// step-result accessors.
func isGenuineCollectionChain(src string) bool {
	if strings.Contains(src, "=>") {
		return true
	}
	lower := strings.ToLower(src)
	for _, m := range genuineCollectionMethods {
		if strings.Contains(lower, "."+m+"(") {
			return true
		}
	}
	return false
}

// resolveCollectionBase resolves a collection-chain base-receiver path
// (`args.members`, `rows`, `loadUsers.result`) against the local Evaluator and
// normalises a query-result envelope to its node slice, so the in-memory
// collection methods iterate the rows rather than the wrapper (#2317). It is the
// resolveBase callback handed to memql.EvaluateCollectionChainString.
func (e *Evaluator) resolveCollectionBase(basePath string) (any, error) {
	val, err := e.resolveCollectionBaseRaw(basePath)
	if err != nil {
		return nil, err
	}
	return normalizeCollectionValue(val), nil
}

// resolveCollectionBaseRaw resolves the base-receiver path to its raw value. A
// custom-var / context root (`args`, `event`, `ctx`, `input`, `item`) that is
// NOT a recorded step id is resolved via the `$`-path so `args.members` reads
// the seeded arg map -- EvaluateStepReference returns the literal string for an
// unprefixed custom root. A step id (or anything else) keeps the standard
// step-reference resolution.
func (e *Evaluator) resolveCollectionBaseRaw(basePath string) (any, error) {
	basePath = strings.TrimSpace(basePath)
	first := basePath
	if dot := strings.IndexByte(basePath, '.'); dot > 0 {
		first = basePath[:dot]
	}
	if _, isStep := e.steps[first]; !isStep && isCustomVarRoot(e, first) {
		return e.EvaluateValue("$" + basePath)
	}
	return e.EvaluateStepReference(basePath)
}

// normalizeCollectionValue coerces a resolved base receiver into the value the
// in-memory collection evaluator expects. A plain slice is returned as-is; a
// flat-output engine envelope is peeled (#2271) and a query-result Bundle is
// unwrapped to its node slice (so `rows.where(...)` over a prior query step
// iterates the rows). Anything else is returned unchanged -- toCollection wraps
// a scalar / single map as a one-element collection.
func normalizeCollectionValue(val any) any {
	switch val.(type) {
	case nil, []any, []map[string]any:
		return val
	}
	unwrapped := UnwrapStepResult(val)
	switch unwrapped.(type) {
	case []any, []map[string]any:
		return unwrapped
	}
	if slice, err := ToSlice(unwrapped); err == nil && slice != nil {
		return slice
	}
	return unwrapped
}

// collectionLambdaArgs returns the outer-arg map a collection lambda body reads
// for `args.X` references that are NOT the bound element (#2318). It prefers the
// caller-arg map a logic body seeds (`args`), then the automation input, then
// the triggering-event envelope (so an automation forEach lambda can read
// `args.payload.X`). Returns nil when none is set -- the element-only lambdas
// (`m => m.active`) don't need it.
func (e *Evaluator) collectionLambdaArgs() map[string]any {
	if m, ok := e.custom["args"].(map[string]any); ok {
		return m
	}
	if m, ok := e.input.(map[string]any); ok {
		return m
	}
	if m, ok := e.custom["event"].(map[string]any); ok {
		return m
	}
	return nil
}

// tryEvaluateCollectionChainLocally routes a genuine collection-method / lambda
// chain through the shared in-memory evaluator (#2317 / #2318). The chain's base
// receiver is resolved + normalised through the local Evaluator, and the
// evaluator's outer args are threaded in so lambda bodies can read `args.X`.
//
// Returns (value, true, nil) when expr is a genuine chain that resolved,
// (nil, false, nil) when expr is not a chain (the caller falls through to its
// normal path), and (nil, false, err) on a hard evaluation error.
func tryEvaluateCollectionChainLocally(expr string, evaluator *Evaluator) (any, bool, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" || !isGenuineCollectionChain(expr) {
		return nil, false, nil
	}
	val, isChain, err := memql.EvaluateCollectionChainString(
		expr,
		evaluator.resolveCollectionBase,
		evaluator.collectionLambdaArgs(),
	)
	if !isChain {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return val, true, nil
}

// EvaluateForEachFilter evaluates a forEach item filter (#2318). When the filter
// is a genuine collection-method / lambda chain over the bound item
// (`item.tags.any(t => t == "vip")`) it is evaluated through the in-memory
// collection surface -- the chain's base receiver (`item.X` / `args.X`) resolves
// through this evaluator, lambda bodies can read the outer `args.X`, and the
// result's truthiness is the match decision. Otherwise the legacy
// string-condition path (EvaluateCondition) runs unchanged, so existing
// `item.status == "active"` filters are unaffected.
func (e *Evaluator) EvaluateForEachFilter(filter string) (bool, error) {
	if isGenuineCollectionChain(filter) {
		if val, handled, err := tryEvaluateCollectionChainLocally(filter, e); err != nil {
			return false, err
		} else if handled {
			return memql.IsTruthy(val), nil
		}
	}
	return e.EvaluateCondition(filter)
}
