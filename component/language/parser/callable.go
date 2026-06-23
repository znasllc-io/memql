package parser

import (
	"sort"
	"sync"

	"github.com/znasllc-io/memql/component/memql/baseparser"
)

// callable.go is the introspectable, table-driven source of truth for every
// bare-function name the expression grammar recognises in parseFunctionCall:
// the editor-callable expression builtins (concat / coalesce / hash / ...),
// the accessor functions (item / event / step / input / field / index / var /
// actor / timestamp / now / error), the keyword-functions (case / default),
// the query directives (paginate / sort / select / asOf / withDepth / shape /
// count), and the relationship wrapper functions (parentOf / childOf / ...).
//
// Before this table the recognised set lived only inside the parseFunctionCall
// switch (and the wrapperFunctions map), so nothing outside the parser could
// enumerate "what builtins exist" -- the one DSL editor surface
// (component/memql/sense) carried a hand-maintained copy that silently drifted
// from the grammar. This mirrors the A3 (#2124) pattern that made the
// top-level construct dispatch introspectable via topLevelDeclParsers /
// TopLevelDeclKeywords: the switch keys off callableParsers, and the exported
// CallableBuiltins is derived from it so it cannot drift. dslspec models the
// builtin metadata and a drift test (component/language/dslspec/drift_test.go)
// pins dslspec.Builtins to CallableBuiltins.
//
// Adding a recognised bare-function name means adding exactly one entry here
// and (if it is editor-callable) a matching dslspec.Builtins entry.

// CallableKind buckets a recognised bare-function name by its role in the
// grammar, so the dslspec drift test can keep the editor-callable expression
// builtins and route the accessors / keyword-functions / directives / wrappers
// to a documented allow-list instead of duplicating them.
type CallableKind int

const (
	// CallableBuiltin is a true editor-callable expression builtin
	// (concat / coalesce / hash / lower / year / contains / ...). These are
	// the names dslspec.Builtins models and Sense offers in completion.
	CallableBuiltin CallableKind = iota
	// CallableAccessor is a context accessor whose name is a reserved engine
	// identifier or a loop/step/automation accessor (item / event / step /
	// input / field / index / var / actor / timestamp / now / error). The
	// reserved ones (now / actor / ...) are modelled by dslspec's keywords();
	// the rest are loop/automation accessors. Either way they are NOT
	// expression builtins.
	CallableAccessor
	// CallableKeywordFunc is a keyword-function: case() / default(). These read
	// like control-flow inside an expression, not as a regular builtin call.
	CallableKeywordFunc
	// CallableDirective is a query directive that wraps an inner expression
	// (paginate / sort / select / asOf / withDepth / shape / count). They have
	// their own AST node types and argument shapes; not regular builtins.
	CallableDirective
	// CallableWrapper is a relationship wrapper function (parentOf / childOf /
	// aliasOf / equals / interactsWith / owns / createdBy / ids). They take a
	// single inner expression and produce a RelationshipExpr.
	CallableWrapper
	// CallableRetired is a name the parser still recognises only to emit a
	// migration-hint error (caller, retired in #221 in favour of actor).
	CallableRetired
)

// callableEntry pairs a recognised name's grammar role with the parse function
// parseFunctionCall dispatches to. The parse functions all share the
// (p *Parser) () (ExpressionNode, error) shape so the switch is a clean map
// lookup; the few names with extra dispatch nuance (index's no-arg special
// case, shape's object-form fork) keep that nuance inline in parseFunctionCall
// and the table entry documents it.
type callableEntry struct {
	kind  CallableKind
	parse func(p *Parser) (ExpressionNode, error)
}

// callableParsers is the authoritative dispatch table for parseFunctionCall,
// accessed via the package-level callableParsers map below. The switch over
// strings.ToLower(name) keys off it. Keys are the LOWERCASED function name (the
// dispatch lower-cases before lookup); the DSL author still spells e.g. shortId
// / canonicalId / asOf / parentOf in source.
//
// It is built lazily inside a function (rather than as a package-level var
// literal) to break the Go initialization cycle: the table holds method values
// that transitively reach parseFunctionCall, which reads the table -- a
// static var-init cycle Go rejects. A sync.Once-guarded builder sidesteps it
// while keeping a single authoritative definition.
//
// Names handled entirely outside this table by parseFunctionCall on purpose:
//   - shape's two-form fork (object vs expr first arg) is decided before the
//     map lookup, so "shape" maps here to parseShapeFunction for the
//     expression-first form and the object form is taken inline.
func buildCallableParsers() map[string]callableEntry {
	return map[string]callableEntry{
		// --- Accessors (loop / step / automation / reserved-identifier) ---
		"var":       {CallableAccessor, (*Parser).parseVarAccessor},
		"step":      {CallableAccessor, (*Parser).parseStepAccessor},
		"field":     {CallableAccessor, (*Parser).parseFieldAccessor},
		"input":     {CallableAccessor, (*Parser).parseInputAccessor},
		"item":      {CallableAccessor, (*Parser).parseItemAccessor},
		"event":     {CallableAccessor, (*Parser).parseEventAccessor},
		"actor":     {CallableAccessor, (*Parser).parseActorAccessor},
		"error":     {CallableAccessor, (*Parser).parseErrorAccessor},
		"timestamp": {CallableAccessor, (*Parser).parseTimestampAccessor},
		"now":       {CallableAccessor, (*Parser).parseTimestampAccessor},

		// --- Retired (recognised only to emit a migration hint) ---
		"caller": {CallableRetired, (*Parser).parseCallerRetired},

		// --- Editor-callable expression builtins ---
		"memqlversion":        {CallableBuiltin, (*Parser).parseMemqlVersionFunction},
		"concat":              {CallableBuiltin, (*Parser).parseConcatFunction},
		"coalesce":            {CallableBuiltin, (*Parser).parseCoalesceFunction},
		"cond":                {CallableBuiltin, (*Parser).parseCondFunction},
		"first":               {CallableBuiltin, (*Parser).parseFirstFunction},
		"last":                {CallableBuiltin, (*Parser).parseLastFunction},
		"lower":               {CallableBuiltin, (*Parser).parseLowerFunction},
		"upper":               {CallableBuiltin, (*Parser).parseUpperFunction},
		"trim":                {CallableBuiltin, (*Parser).parseTrimFunction},
		"hash":                {CallableBuiltin, (*Parser).parseHashFunction},
		"shortid":             {CallableBuiltin, (*Parser).parseShortIdFunction},
		"canonicalid":         {CallableBuiltin, (*Parser).parseCanonicalIdFunction},
		"tostring":            {CallableBuiltin, (*Parser).parseToStringFunction},
		"addduration":         {CallableBuiltin, (*Parser).parseAddDurationFunction},
		"daysbetween":         {CallableBuiltin, (*Parser).parseDaysBetweenFunction},
		"subtracttimestamps":  {CallableBuiltin, (*Parser).parseSubtractTimestampsFunction},
		"year":                {CallableBuiltin, (*Parser).parseYearFunction},
		"quarter":             {CallableBuiltin, (*Parser).parseQuarterFunction},
		"month":               {CallableBuiltin, (*Parser).parseMonthFunction},
		"dayofmonth":          {CallableBuiltin, (*Parser).parseDayOfMonthFunction},
		"isanniversary":       {CallableBuiltin, (*Parser).parseIsAnniversaryFunction},
		"isfirstdayofquarter": {CallableBuiltin, (*Parser).parseIsFirstDayOfQuarterFunction},
		"contains":            {CallableBuiltin, (*Parser).parseContainsFunction},

		// --- Keyword-functions (control-flow shaped expression functions) ---
		"case":    {CallableKeywordFunc, (*Parser).parseCaseFunction},
		"default": {CallableKeywordFunc, (*Parser).parseDefaultFunction},

		// --- Query directives (wrap an inner expression / produce specialised AST) ---
		"paginate":  {CallableDirective, (*Parser).parsePaginateFunction},
		"sort":      {CallableDirective, (*Parser).parseSortFunction},
		"select":    {CallableDirective, (*Parser).parseSelectFunction},
		"asof":      {CallableDirective, (*Parser).parseAsOfFunction},
		"withdepth": {CallableDirective, (*Parser).parseWithDepthFunction},
		"count":     {CallableDirective, (*Parser).parseCountFunction},
		"shape":     {CallableDirective, (*Parser).parseShapeFunction},
	}
}

var (
	callableParsersOnce  sync.Once
	callableParsersTable map[string]callableEntry
)

// callableParsers returns the lazily-built dispatch table (see
// buildCallableParsers for why it is not a package-level var literal). The
// sync.Once guard makes it safe under the concurrent parsing the engine does.
func callableParsers() map[string]callableEntry {
	callableParsersOnce.Do(func() {
		callableParsersTable = buildCallableParsers()
	})
	return callableParsersTable
}

// relationshipWrapperNames is the lowercased set of relationship wrapper
// functions (parentOf / childOf / ...). parseFunctionCall handles them via a
// shared inner-expression parse + createWrapper dispatch rather than a
// dedicated parse function, so they are catalogued here for introspection but
// are NOT in callableParsers. contains() is intentionally absent -- the modern
// single-paren contains(filter) relationship form is discriminated inside
// parseContainsFunction by arg count, so contains lives in callableParsers as a
// builtin.
var relationshipWrapperNames = []string{
	"parentof", "childof", "aliasof",
	"equals", "interactswith",
	"owns", "createdby", "ids",
}

// parseCallerRetired is the dispatch target for the retired caller() accessor.
// It keeps the switch a uniform map lookup; the produced error is the single
// migration-hint string baseparser.ErrCallerRetired (asserted equal across the
// two parsers by the #244 cross-parser equivalence test).
func (p *Parser) parseCallerRetired() (ExpressionNode, error) {
	return nil, newParseErrorf(&p.current, "%s", baseparser.ErrCallerRetired.Error())
}

// CallableBuiltins is the sorted set of editor-callable expression builtin
// names (CallableBuiltin kind) derived from callableParsers. It is the
// authoritative, introspectable list dslspec.Builtins is pinned against by the
// #2155 drift test, so the editor's builtin surface cannot drift from the
// grammar. It deliberately EXCLUDES accessors (item / event / now / actor /
// ...), keyword-functions (case / default), query directives (sort / shape /
// count / ...), relationship wrappers (parentOf / ...), and the retired
// caller() -- those are routed to the drift test's documented allow-list.
//
// Names are the LOWERCASED dispatch keys (shortid / canonicalid / asof). The
// DSL author spelling (shortId / canonicalId) is carried by dslspec.Builtins'
// Name field; the drift test lower-cases dslspec names before comparing.
var CallableBuiltins = callableNamesOfKind(CallableBuiltin)

// CallableAccessors is the sorted set of accessor names (CallableAccessor
// kind). Exposed for the drift test's allow-list so it can assert the
// accessors are exactly the names dslspec routes to keywords() / loop
// accessors rather than to Builtins.
var CallableAccessors = callableNamesOfKind(CallableAccessor)

// CallableKeywordFuncs is the sorted set of keyword-function names
// (case / default).
var CallableKeywordFuncs = callableNamesOfKind(CallableKeywordFunc)

// CallableDirectives is the sorted set of query-directive names
// (paginate / sort / select / asOf / withDepth / count / shape).
var CallableDirectives = callableNamesOfKind(CallableDirective)

// CallableRelationshipWrappers is the sorted set of relationship wrapper
// function names (parentOf / childOf / ...). Sourced from
// relationshipWrapperNames (they are not in callableParsers; see its doc).
var CallableRelationshipWrappers = func() []string {
	out := append([]string(nil), relationshipWrapperNames...)
	sort.Strings(out)
	return out
}()

// CallableRetiredNames is the sorted set of recognised-but-retired callable
// names (caller).
var CallableRetiredNames = callableNamesOfKind(CallableRetired)

func callableNamesOfKind(kind CallableKind) []string {
	out := make([]string, 0)
	for name, entry := range callableParsers() {
		if entry.kind == kind {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
