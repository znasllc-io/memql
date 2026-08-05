package memql

// parser.go carries the runtime-entry surface for the MemQL engine:
// the `MemQLEngine.Parse` method that `engine.Execute` calls on every
// query, the `applyDirectiveWrappers` post-parse normaliser, the
// `parseInsertFunction` shortcut for the `insert(...)` literal form,
// and a small set of pure-function helpers (splitSelectFields /
// populate{Cache,Concept}*, etc.) consumed by Parse + its callers.
//
// The legacy in-package recursive-descent parser (the `parser` struct
// + `newParser` + ~100 `(p *parser).*` methods + the `tokenize` lexer
// + the `token` / `tokenType` types) was retired in #328 (Stage 1B of
// #310). Runtime queries are now parsed exclusively through the
// language parser (`parseViaLangparser` in parser_langpath.go); the
// short-circuit branches above stay because they handle shapes that
// don't fit the expression grammar (insert() literal, meta-command
// dispatch).

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql/baseparser"
)

// Parse converts a MemQL query string into a QueryPlan.
func (e *MemQLEngine) Parse(query string) (*QueryPlan, error) {
	return e.parseWithFunctions(query, e.functions, nil, false)
}

// parseWithFunctions is Parse with the function registry made explicit, so an
// authored-overlay call (ExecuteAuthored, MCP Tier-2 session constructs) can
// classify + inline session-authored functions that core does not define. The
// public Parse delegates with e.functions + a nil specOverlay, so default
// behaviour is unchanged.
//
// specOverlay (when non-nil) carries the owner's session-authored specs layered
// CORE-FIRST over e.specs (#1559). It is consulted ONLY on the authored path:
// after function calls are inlined (so an authored query's filter is now part of
// plan.Root) we fully expand the remaining SpecReferenceExpression nodes against
// the overlay, so a session-defined spec referenced from an authored query's
// filter resolves -- without ever shadowing a core spec, and without the public
// Parse/Execute path seeing the session spec. A nil overlay leaves spec
// resolution exactly as before (the core query-filter spec refs survive parse
// and resolve at runtime via e.specs).
//
// allowInline (MCP Tier-3 #1535) relaxes the runtime-shape pre-rejection for the
// inline-definition (`name := expr`) + trailing `@timestamp`/`@latest` shapes,
// gated server-side to the inline tier + owner/developer. Default false keeps the
// sealed behaviour for every other caller.
func (e *MemQLEngine) parseWithFunctions(query string, fns *FunctionRegistry, specOverlay map[string]*Spec, allowInline bool) (*QueryPlan, error) {
	// No origin supplied: treat as an untrusted client call (memql#2800).
	return e.parseWithFunctionsOrigin(query, fns, specOverlay, allowInline, auth.OriginClient)
}

// parseWithFunctionsOrigin is parseWithFunctions with the call origin made
// explicit. The parse path is otherwise ctx-free, so the origin rides as a
// parameter rather than dragging a context through the parser.
func (e *MemQLEngine) parseWithFunctionsOrigin(query string, fns *FunctionRegistry, specOverlay map[string]*Spec, allowInline bool, origin auth.CallOrigin) (*QueryPlan, error) {
	return e.parseWithFunctionsAmbient(query, fns, specOverlay, allowInline, origin, nil)
}

// parseWithFunctionsAmbient is parseWithFunctionsOrigin carrying the ambient
// envelope (memql#3024). It rides as a parameter for the same reason the origin
// does: the parse path is ctx-free by design, so executeWith reads the context
// once and hands the resolved values down. The no-envelope wrapper passes nil.
func (e *MemQLEngine) parseWithFunctionsAmbient(query string, fns *FunctionRegistry, specOverlay map[string]*Spec, allowInline bool, origin auth.CallOrigin, ambient map[string]any) (*QueryPlan, error) {
	if !e.canResolve() {
		return nil, ErrEngineNotInitialized
	}

	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return nil, ErrEmptyQuery
	}

	if isInsertFunction(trimmed) {
		mutation, err := parseInsertFunction(trimmed)
		if err != nil {
			return nil, err
		}
		return &QueryPlan{
			Root:          nil,
			Mutations:     []MutationNode{*mutation},
			Filters:       nil,
			Relationships: nil,
			Timestamp:     nil,
			UseLatest:     false,
		}, nil
	}

	// Introspection meta-command short-circuit (#256). Runs BEFORE
	// either runtime parser so dispatch for help / docs / concepts /
	// validate / functions / tools / serviceVersion / memqlVersion /
	// shapeTemplates / shapeHelp / contentId / previewInsert is owned
	// by the meta_commands.go shim. The two parsers no longer carry
	// per-name special cases for these (parser.go's lookupBuiltin
	// branch never fires for them now; the langparser never had
	// equivalent code). On a matched name with bad args this returns
	// a real parse error -- we do NOT fall through to the parsers,
	// matching the insert(...) short-circuit's all-or-nothing
	// commitment above.
	if metaExpr, matched, err := e.tryParseMetaCommand(trimmed); matched {
		if err != nil {
			return nil, err
		}
		return &QueryPlan{
			Root:          metaExpr,
			Mutations:     nil,
			Filters:       nil,
			Relationships: nil,
			Timestamp:     nil,
			UseLatest:     false,
		}, nil
	}

	// #250: langparser is the sole runtime parser. parseViaLangparser
	// returns a typed ErrUnsupportedQueryShape for the shapes the
	// langparser doesn't cover (shape / select / in / concept==memql:
	// version / trailing-comma); other errors are genuine parse failures
	// and propagate raw. The legacy memql parser fallback (the post-#249
	// soak posture) is retired now that sub-epic #286 migrated cognition's
	// runtime callsites.
	//
	// #1558: under allowInline (the gated MCP Tier-3 path) the two inline
	// shapes -- a trailing `@timestamp`/`@latest` asOf modifier and a
	// leading `name := expr` inline-spec definition -- are PARSED here
	// (rather than rejected) and surfaced on the result, so they populate
	// timestamp / useLatest / inlineSpecs below. For every other caller
	// (allowInline=false) those fields stay zero/nil exactly as before.
	parsed, err := parseViaLangparser(trimmed, allowInline)
	if err != nil {
		return nil, err
	}
	if parsed.Root == nil {
		return nil, ErrEmptyQuery
	}

	plan := &QueryPlan{
		Root:          parsed.Root,
		Mutations:     nil,
		Filters:       nil,
		Relationships: nil,
		Timestamp:     parsed.Timestamp,
		UseLatest:     parsed.UseLatest,
		CacheHints:    nil,
		ConceptFields: make(map[string][]FieldReference),
		Metadata: metadataSelection{
			IncludeAll: true,
			Fields:     make(map[string]struct{}),
		},
		InlineSpecs: nil,
	}

	normalizedRoot, err := applyDirectiveWrappers(plan)
	if err != nil {
		return nil, err
	}
	plan.Root = normalizedRoot
	// #1558: inline-spec definitions parsed under allowInline land in
	// parsed.InlineSpecs; resolvePlanSpecs expands the SpecReference root
	// against them. For non-inline callers this is nil (unchanged).
	if err := e.resolvePlanSpecs(plan, parsed.InlineSpecs); err != nil {
		return nil, err
	}
	// Resolve function calls after spec resolution
	if err := resolvePlanFunctionsWithAmbient(plan, fns, e.specs, origin, ambient); err != nil {
		return nil, err
	}
	// Apply directive wrappers again after function resolution
	// Functions may expand to expressions containing directives (shape, sort, paginate, etc.)
	// that need to be extracted into the plan
	normalizedRoot, err = applyDirectiveWrappers(plan)
	if err != nil {
		return nil, err
	}
	plan.Root = normalizedRoot
	// #1559: authored-path spec overlay. A session-authored query's filter is
	// only part of plan.Root AFTER function inlining above, so we expand its
	// remaining spec references here, consulting the owner's session specs
	// layered core-first over e.specs. The public path passes a nil overlay and
	// is untouched -- its spec refs resolve unchanged at runtime via e.specs.
	if specOverlay != nil {
		resolvedRoot, err := e.resolveAuthoredSpecOverlay(plan.Root, specOverlay)
		if err != nil {
			return nil, err
		}
		plan.Root = resolvedRoot
	}
	// Bare payload access (epic #2292): a filter predicates on payload
	// properties by BARE name (the concept is bound by the query
	// signature). Rewrite each bare, non-reserved field reference to its
	// underlying payload.<prop> form so both the SQL pushdown and the
	// in-memory evaluator consume the canonical access. Runs AFTER spec +
	// function inlining so inlined query-author fields are present; inlined
	// specs/traits already carry payload.* (first part "payload") and are
	// skipped. The legacy explicit payload.X form passes through unchanged.
	if err := rewriteFilterFieldRefs(plan.Root); err != nil {
		return nil, err
	}
	populateCacheHints(plan)
	populateConceptFields(plan)
	if err := validateAIContext(plan.Root); err != nil {
		return nil, err
	}

	return plan, nil
}

// reservedFilterHead reports whether a filter field's first path segment
// is a reserved engine name or a row intrinsic -- such a reference is NOT
// a payload property and must not be payload-prefixed by the bare-access
// rewrite (epic #2292).
func reservedFilterHead(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "row", "payload", "actor", "args", "now", "config", "trace", "meta",
		"schema", "partition", "provenance":
		return true
	}
	_, ok := resolveIntrinsicField(name)
	return ok
}

// rowIntrinsicNamespace is the head that addresses the row envelope in a
// predicate: `row.id`, `row.createdAt`, `row.provenance.kind`. It mirrors
// the prefix shape bodies already project through, so one spelling names a
// row intrinsic everywhere an author can write one (memql#2779).
const rowIntrinsicNamespace = "row"

// rowIntrinsicFieldRef strips the `row.` namespace from a filter field
// reference and returns the intrinsic it addresses, canonically cased.
// `row.provenance.<leaf>` keeps its leaf path -- provenance is the one
// object-valued intrinsic.
//
// A leaf that is not an intrinsic is an ERROR rather than a fall-through to
// the payload surface. That fall-through is exactly the defect this
// namespace closes: before #2779 `row` was not a reserved head, so
// `row.id` was payload-prefixed into `payload.row.id` -- a JSON path no
// stored row carries, which parsed, loaded, and linted clean while matching
// zero rows forever.
func rowIntrinsicFieldRef(ref FieldReference) (FieldReference, error) {
	if len(ref.Parts) < 2 {
		return FieldReference{}, fmt.Errorf(
			"%w: `row` is the row-intrinsic namespace and needs a field -- write row.id, row.concept, row.type, row.createdAt, row.createdBy, or row.provenance.<leaf>",
			ErrInvalidArgument)
	}

	leaf := strings.TrimSpace(ref.Parts[1])

	// `partition` and `schema` ARE row intrinsics, but the filter compiler
	// has no branch for either -- both end at "field %q is not supported".
	// Say that, rather than routing them into the payload advice below: an
	// author told to "write it bare" would hit the same wall one edit later.
	if isUnfilterableRowIntrinsic(leaf) {
		return FieldReference{}, fmt.Errorf(
			"%w: `row.%s` is a row intrinsic but is not filter-comparable -- there is no SQL push-down for it. Filterable intrinsics: id, concept, type, createdAt, createdBy, provenance.<leaf>",
			ErrInvalidArgument, leaf)
	}

	info, ok := resolveIntrinsicField(leaf)
	if !ok {
		// "Write it bare" is only sound advice when the bare form would read
		// the payload. A leaf that is itself a reserved head (`row.actor`,
		// `row.payload`, `row.row`, ...) would NOT -- suggesting it sends the
		// author somewhere else wrong, and `row.row` would loop.
		if leaf == "" {
			return FieldReference{}, fmt.Errorf(
				"%w: `row.` needs a field after the dot -- write row.id, row.concept, row.type, row.createdAt, row.createdBy, or row.provenance.<leaf>",
				ErrInvalidArgument)
		}
		if reservedFilterHead(leaf) {
			return FieldReference{}, fmt.Errorf(
				"%w: `row.%s` is not a row intrinsic -- `%s` is a reserved engine namespace of its own, not a field under `row`. Row intrinsics: id, concept, type, createdAt, createdBy, provenance.<leaf>",
				ErrInvalidArgument, leaf, leaf)
		}
		return FieldReference{}, fmt.Errorf(
			"%w: `row.%s` is not a row intrinsic (valid: id, concept, type, createdAt, createdBy, provenance) -- a concept's payload property is named BARE in a filter, so write `%s`",
			ErrInvalidArgument, leaf, leaf)
	}

	// Depth validation, mirroring validateFieldReference (the strict literal
	// path) so the namespace is not WEAKER than the bare spelling it
	// replaces. Without this, `row.id.foo` slipped past parse -- this rewrite
	// runs after validateFieldReference -- and surfaced later as a SQL-compile
	// error whose text had lost the `row.` the author actually typed.
	rest := ref.Parts[2:]
	if info.kind == intrinsicFieldProvenance {
		switch {
		case len(rest) == 0:
			return FieldReference{}, fmt.Errorf(
				"%w: `row.provenance` addresses an object -- compare a leaf instead (row.provenance.kind / .name / .trigger / .via)",
				ErrInvalidArgument)
		case len(rest) > 1:
			// The author DID supply a leaf; the problem is depth. Saying
			// "compare a leaf instead" here would describe the wrong mistake.
			return FieldReference{}, fmt.Errorf(
				"%w: `row.provenance.%s` is too deep -- provenance leaves are scalars, so compare row.provenance.%s directly",
				ErrInvalidArgument, strings.Join(rest, "."), rest[0])
		}
		if !isProvenanceLeaf(rest[0]) {
			return FieldReference{}, fmt.Errorf(
				"%w: `row.provenance.%s` is not a provenance field (valid: kind, name, trigger, via)",
				ErrInvalidArgument, rest[0])
		}
	} else if len(rest) > 0 {
		return FieldReference{}, fmt.Errorf(
			"%w: row intrinsic `row.%s` is a scalar and does not support nested paths (got `row.%s.%s`)",
			ErrInvalidArgument, info.canonical, info.canonical, strings.Join(rest, "."))
	}

	parts := append([]string{info.canonical}, rest...)
	return FieldReference{
		Raw:      strings.Join(parts, "."),
		Parts:    parts,
		Wildcard: ref.Wildcard,
	}, nil
}

// isUnfilterableRowIntrinsic reports whether name is a row intrinsic that
// carries no filter push-down. `partition` and `schema` are stamped on every
// row and are reserved filter heads (so they are never payload-prefixed), but
// compileComparisonExpressionWithContext has no case for either.
func isUnfilterableRowIntrinsic(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "partition", "schema":
		return true
	}
	return false
}

// isProvenanceLeaf reports whether name is one of the engine-stamped
// provenance fields. Mirrors the allow-list the provenance compiler enforces
// (executor_filter.go) so the error arrives at parse time with the authored
// `row.` spelling intact.
func isProvenanceLeaf(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "kind", "name", "trigger", "via":
		return true
	}
	return false
}

// rewriteFilterFieldRefs walks a resolved query plan expression and
// normalises every field reference through filterFieldRef: the `row.`
// namespace collapses to the intrinsic it names, and a bare, non-reserved
// property gains its `payload.` prefix. Spec references and comparison
// values are left untouched. Mirrors collectConceptFields' wrapper
// traversal so it reaches comparisons nested under directive wrappers.
//
// It returns an error (rather than rewriting in place silently) because a
// malformed `row.<leaf>` must fail the parse loudly -- see
// rowIntrinsicFieldRef.
func rewriteFilterFieldRefs(expr ExpressionNode) error {
	switch node := expr.(type) {
	case *ComparisonExpression:
		field, err := filterFieldRef(node.Field)
		if err != nil {
			return err
		}
		node.Field = field
		for i := range node.FieldSelections {
			selection, err := filterFieldRef(node.FieldSelections[i])
			if err != nil {
				return err
			}
			node.FieldSelections[i] = selection
		}
	case *LogicalExpression:
		if err := rewriteFilterFieldRefs(node.Left); err != nil {
			return err
		}
		return rewriteFilterFieldRefs(node.Right)
	case *RelationshipExpression:
		return rewriteFilterFieldRefs(node.Target)
	case *SortExpression:
		return rewriteFilterFieldRefs(node.Target)
	case *PaginateExpression:
		return rewriteFilterFieldRefs(node.Target)
	case *SelectExpression:
		return rewriteFilterFieldRefs(node.Target)
	case *TimestampExpression:
		return rewriteFilterFieldRefs(node.Target)
	case *DepthExpression:
		return rewriteFilterFieldRefs(node.Target)
	case *CountExpression:
		return rewriteFilterFieldRefs(node.Target)
	case *ShapeExpression:
		return rewriteFilterFieldRefs(node.Target)
	}
	return nil
}

// filterFieldRef normalises one filter field reference to the form the SQL
// pushdown and the in-memory evaluator both consume:
//
//	row.<intrinsic>  -> <intrinsic>          (the row envelope, #2779)
//	<reserved head>  -> unchanged            (actor / args / payload / ...)
//	<bare property>  -> payload.<property>   (epic #2292)
func filterFieldRef(ref FieldReference) (FieldReference, error) {
	if len(ref.Parts) == 0 {
		return ref, nil
	}
	if strings.EqualFold(strings.TrimSpace(ref.Parts[0]), rowIntrinsicNamespace) {
		return rowIntrinsicFieldRef(ref)
	}
	if reservedFilterHead(ref.Parts[0]) {
		return ref, nil
	}
	parts := append([]string{"payload"}, ref.Parts...)
	return FieldReference{Raw: strings.Join(parts, "."), Parts: parts, Wildcard: ref.Wildcard}, nil
}

func applyDirectiveWrappers(plan *QueryPlan) (ExpressionNode, error) {
	expr := plan.Root
	for {
		switch node := expr.(type) {
		case *SortExpression:
			if len(node.Fields) == 0 {
				return nil, fmt.Errorf("sort() requires at least one field")
			}
			if len(plan.Sort) > 0 {
				return nil, fmt.Errorf("multiple sort() directives are not supported")
			}
			fields := make([]SortField, len(node.Fields))
			copy(fields, node.Fields)
			plan.Sort = fields
			expr = node.Target
		case *SelectExpression:
			if len(node.Fields) == 0 {
				return nil, fmt.Errorf("select() requires at least one field")
			}
			if len(plan.Fields) > 0 {
				return nil, fmt.Errorf("multiple select() directives are not supported")
			}
			payloadRefs, metadataSel, err := splitSelectFields(node.Fields)
			if err != nil {
				return nil, err
			}
			plan.Fields = payloadRefs
			plan.Metadata = metadataSel
			plan.PayloadSelect = true
			expr = node.Target
		case *PaginateExpression:
			if plan.Limit != nil {
				return nil, fmt.Errorf("multiple paginate() directives are not supported")
			}
			plan.Limit = node.Limit
			expr = node.Target
		case *TimestampExpression:
			if plan.Timestamp != nil || plan.UseLatest {
				return nil, fmt.Errorf("multiple asOf() directives are not supported")
			}
			// An UNRESOLVED caller-arg asOf must not reach here. This copies
			// Timestamp and UseLatest only, so an ArgPath node would be
			// silently DISCARDED -- the plan would carry no asOf at all, the
			// caller would get a live-tip read having asked for a point in
			// time, and nothing would say so.
			//
			// A declared query cannot reach this state: resolveAsOfArg runs at
			// argument expansion and replaces the node. A RAW RUNTIME QUERY
			// STRING can, because it parses through the same entry point with
			// no args to expand against -- so `asOf(concept==..., args.at)`
			// parsed clean and lost its directive. asof_arg_resolve.go's design
			// note says "the unresolved form never escapes"; this is the path
			// where it did (memql#2992 landing review).
			if node.ArgPath != "" {
				return nil, fmt.Errorf(
					"asOf(args.%s) is not available in a raw query string: there are no declared "+
						"arguments to resolve it against. Use a literal RFC3339 instant or `latest` "+
						"here, or call a declared query that offers the caller-chosen form (memql#2992)",
					node.ArgPath)
			}
			plan.Timestamp = node.Timestamp
			plan.UseLatest = node.UseLatest
			expr = node.Target
		case *DepthExpression:
			if plan.Depth != nil {
				return nil, fmt.Errorf("multiple withDepth() directives are not supported")
			}
			depth := node.Depth
			plan.Depth = &depth
			expr = node.Target
		case *CountExpression:
			if plan.Count {
				return nil, fmt.Errorf("multiple count() directives are not supported")
			}
			plan.Count = true
			expr = node.Target
		case *ShapeExpression:
			if plan.ShapeTemplate != nil || plan.ShapeTemplateName != "" {
				return nil, fmt.Errorf("multiple shape() directives are not supported")
			}
			plan.ShapeTemplate = node.Template
			plan.ShapeTemplateName = node.TemplateName
			plan.IncludeBundle = node.IncludeBundle
			expr = node.Target
		default:
			if err := ensureNoDirectiveNodes(expr); err != nil {
				return nil, err
			}
			return expr, nil
		}
	}
}

func ensureNoDirectiveNodes(expr ExpressionNode) error {
	if expr == nil {
		return nil
	}
	switch node := expr.(type) {
	case *SortExpression, *SelectExpression, *PaginateExpression, *TimestampExpression, *DepthExpression, *CountExpression, *ShapeExpression:
		return fmt.Errorf("directive functions (e.g., paginate()) must be the outermost wrapper around the query expression")
	case *LogicalExpression:
		if err := ensureNoDirectiveNodes(node.Left); err != nil {
			return err
		}
		return ensureNoDirectiveNodes(node.Right)
	case *RelationshipExpression:
		return ensureNoDirectiveNodes(node.Target)
	default:
		return nil
	}
}

func isInsertFunction(query string) bool {
	trimmed := strings.TrimSpace(strings.ToLower(query))
	return strings.HasPrefix(trimmed, "insert(")
}

func validateBuiltinCallArgs(callName string, args map[string]any, contract *BuiltinArgContract) error {
	if contract == nil {
		return nil
	}
	for _, required := range contract.Required {
		if _, ok := args[required]; !ok {
			return fmt.Errorf("%w: %s() requires '%s' field in argument", ErrInvalidArgument, callName, required)
		}
	}
	if contract.Properties != nil {
		if contract.AdditionalProperties != nil && !*contract.AdditionalProperties {
			for key := range args {
				if _, ok := contract.Properties[key]; !ok {
					return fmt.Errorf("%w: %s() does not accept '%s' field in argument", ErrInvalidArgument, callName, key)
				}
			}
		}
		for key, rawVal := range args {
			expected, ok := contract.Properties[key]
			if !ok || expected == "" {
				continue
			}
			if !builtinArgTypeMatches(rawVal, expected) {
				return fmt.Errorf("%w: %s() '%s' field must be %s", ErrInvalidArgument, callName, key, expected)
			}
			if strings.EqualFold(expected, "string") {
				if s, ok := rawVal.(string); ok && strings.TrimSpace(s) == "" {
					return fmt.Errorf("%w: %s() %s cannot be empty", ErrInvalidArgument, callName, key)
				}
			}
		}
	}
	return nil
}

func builtinArgTypeMatches(value any, expected string) bool {
	switch strings.ToLower(strings.TrimSpace(expected)) {
	case "string":
		_, ok := value.(string)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "number", "int":
		switch value.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
			return true
		}
		return false
	case "boolean", "bool":
		_, ok := value.(bool)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "[]string":
		arr, ok := value.([]any)
		if !ok {
			return false
		}
		for _, v := range arr {
			if _, ok := v.(string); !ok {
				return false
			}
		}
		return true
	case "[]int", "[]number":
		arr, ok := value.([]any)
		if !ok {
			return false
		}
		for _, v := range arr {
			switch v.(type) {
			case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
				// ok
			default:
				return false
			}
		}
		return true
	default:
		return true
	}
}

func parseInsertFunction(query string) (*MutationNode, error) {
	parser := &insertFunctionParser{
		input: []rune(query),
	}
	parser.skipWhitespace()

	if !parser.consumeKeyword("insert") {
		return nil, parser.errorf(parser.pos, "expected insert(")
	}

	parser.skipWhitespace()
	if !parser.consumeChar('(') {
		return nil, parser.errorf(parser.pos, "expected '(' after insert")
	}

	parser.skipWhitespace()
	concept, err := parser.parseStringLiteral()
	if err != nil {
		return nil, err
	}
	concept = strings.TrimSpace(concept)
	if concept == "" {
		return nil, parser.errorf(parser.pos, "concept name is required")
	}

	parser.skipWhitespace()
	if !parser.consumeChar(',') {
		return nil, parser.errorf(parser.pos, "expected ',' after concept argument")
	}

	var (
		idValue    string
		payloadRaw string
		parentRef  *string
		aliasOfRef *string
		seen       = make(map[string]struct{})
	)

	for {
		parser.skipWhitespace()
		if parser.peek() == ')' {
			break
		}

		argPos := parser.pos
		name, err := parser.parseIdentifier()
		if err != nil {
			return nil, err
		}
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			return nil, parser.errorf(argPos, "argument name is required")
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, parser.errorf(argPos, "duplicate argument %q", name)
		}

		parser.skipWhitespace()
		if !parser.consumeChar('=') {
			return nil, parser.errorf(parser.pos, "expected '=' after argument %q", name)
		}
		parser.skipWhitespace()

		switch name {
		case "id":
			value, err := parser.parseStringLiteral()
			if err != nil {
				return nil, err
			}
			idValue = strings.TrimSpace(value)
		case "payload":
			raw, err := parser.parseJSONObject()
			if err != nil {
				return nil, err
			}
			payloadRaw = raw
		case "parent":
			value, err := parser.parseStringLiteral()
			if err != nil {
				return nil, err
			}
			copy := strings.TrimSpace(value)
			parentRef = &copy
		case "aliasof":
			value, err := parser.parseStringLiteral()
			if err != nil {
				return nil, err
			}
			copy := strings.TrimSpace(value)
			aliasOfRef = &copy
		default:
			return nil, parser.errorf(argPos, "unknown argument %q", name)
		}

		seen[name] = struct{}{}

		parser.skipWhitespace()
		switch parser.peek() {
		case ',':
			parser.pos++
		case ')':
			// handled by loop exit
		default:
			return nil, parser.errorf(parser.pos, "expected ',' or ')' after argument")
		}

		if parser.peek() == ')' {
			break
		}
	}

	if !parser.consumeChar(')') {
		return nil, parser.errorf(parser.pos, "expected ')' to close insert()")
	}

	parser.skipWhitespace()
	if !parser.eof() {
		return nil, parser.errorf(parser.pos, "unexpected content after insert()")
	}

	if payloadRaw == "" {
		return nil, parser.errorf(parser.pos, "payload argument is required")
	}

	return &MutationNode{
		Concept:    concept,
		ID:         idValue,
		PayloadRaw: payloadRaw,
		ParentRef:  parentRef,
		AliasOfRef: aliasOfRef,
	}, nil
}

type insertFunctionParser struct {
	input []rune
	pos   int
}

func (p *insertFunctionParser) eof() bool {
	return p.pos >= len(p.input)
}

func (p *insertFunctionParser) peek() rune {
	if p.eof() {
		return 0
	}
	return p.input[p.pos]
}

func (p *insertFunctionParser) skipWhitespace() {
	for !p.eof() && unicode.IsSpace(p.peek()) {
		p.pos++
	}
}

func (p *insertFunctionParser) consumeKeyword(keyword string) bool {
	if p.pos+len(keyword) > len(p.input) {
		return false
	}
	candidate := string(p.input[p.pos : p.pos+len(keyword)])
	if !strings.EqualFold(candidate, keyword) {
		return false
	}
	p.pos += len(keyword)
	return true
}

func (p *insertFunctionParser) consumeChar(expected rune) bool {
	if p.eof() || p.peek() != expected {
		return false
	}
	p.pos++
	return true
}

func (p *insertFunctionParser) parseIdentifier() (string, error) {
	if p.eof() {
		return "", p.errorf(p.pos, "expected identifier")
	}

	start := p.pos
	if !isIdentifierStart(p.peek()) {
		return "", p.errorf(p.pos, "expected identifier")
	}

	p.pos++
	for !p.eof() && isIdentifierChar(p.peek()) {
		p.pos++
	}

	return string(p.input[start:p.pos]), nil
}

func (p *insertFunctionParser) parseStringLiteral() (string, error) {
	if p.eof() || p.peek() != '"' {
		return "", p.errorf(p.pos, "expected '\"' to start string literal")
	}

	start := p.pos
	p.pos++

	escaped := false
	for !p.eof() {
		r := p.peek()
		p.pos++

		if escaped {
			escaped = false
			continue
		}

		if r == '\\' {
			escaped = true
			continue
		}

		if r == '"' {
			literal := string(p.input[start:p.pos])
			value, err := strconv.Unquote(literal)
			if err != nil {
				return "", p.errorf(start, "invalid string literal: %v", err)
			}
			return value, nil
		}
	}

	return "", p.errorf(start, "unterminated string literal")
}

func (p *insertFunctionParser) parseJSONObject() (string, error) {
	if p.eof() || p.peek() != '{' {
		return "", p.errorf(p.pos, "expected '{' to start payload")
	}

	start := p.pos
	depth := 0
	inString := false
	escaped := false

	for !p.eof() {
		r := p.peek()
		p.pos++

		if inString {
			if escaped {
				escaped = false
				continue
			}
			if r == '\\' {
				escaped = true
				continue
			}
			if r == '"' {
				inString = false
			}
			continue
		}

		switch r {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return string(p.input[start:p.pos]), nil
			}
		}
	}

	return "", p.errorf(start, "unterminated JSON payload")
}

func (p *insertFunctionParser) errorf(pos int, format string, args ...any) error {
	message := fmt.Sprintf(format, args...)
	return fmt.Errorf("%w: %s at position %d", ErrInvalidQuerySyntax, message, pos)
}

func populateCacheHints(plan *QueryPlan) {
	if plan == nil || plan.Root == nil {
		return
	}
	hints := make(map[string]int64)
	collectCacheHints(plan.Root, hints)
	if len(hints) > 0 {
		plan.CacheHints = hints
	}
}

func populateConceptFields(plan *QueryPlan) {
	if plan == nil || plan.Root == nil {
		return
	}
	fields := make(map[string][]FieldReference)
	collectConceptFields(plan.Root, fields)
	if len(fields) > 0 {
		plan.ConceptFields = fields
	}
}

func collectConceptFields(expr ExpressionNode, acc map[string][]FieldReference) {
	if expr == nil || acc == nil {
		return
	}

	switch node := expr.(type) {
	case *ComparisonExpression:
		if len(node.FieldSelections) > 0 &&
			len(node.Field.Parts) > 0 &&
			strings.EqualFold(node.Field.Parts[0], "concept") {
			conceptName, ok := node.Value.(string)
			if !ok {
				break
			}
			conceptKey := strings.TrimSpace(conceptName)
			if conceptKey == "" {
				break
			}
			acc[conceptKey] = appendUniqueFieldReferences(acc[conceptKey], node.FieldSelections)
		}
	case *LogicalExpression:
		collectConceptFields(node.Left, acc)
		collectConceptFields(node.Right, acc)
	case *RelationshipExpression:
		collectConceptFields(node.Target, acc)
	case *SortExpression:
		collectConceptFields(node.Target, acc)
	case *PaginateExpression:
		collectConceptFields(node.Target, acc)
	case *SelectExpression:
		collectConceptFields(node.Target, acc)
	case *TimestampExpression:
		collectConceptFields(node.Target, acc)
	case *DepthExpression:
		collectConceptFields(node.Target, acc)
	case *CountExpression:
		collectConceptFields(node.Target, acc)
	case *ShapeExpression:
		collectConceptFields(node.Target, acc)
	case *BuiltinFunctionExpression:
		// Builtin functions don't have nested expressions to collect from
	}
}

func appendUniqueFieldReferences(existing []FieldReference, refs []FieldReference) []FieldReference {
	if len(refs) == 0 {
		return existing
	}
	seen := make(map[string]struct{}, len(existing))
	for _, ref := range existing {
		key := canonicalField(ref)
		if key == "" {
			continue
		}
		seen[key] = struct{}{}
	}
	for _, ref := range refs {
		key := canonicalField(ref)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		existing = append(existing, ref)
		seen[key] = struct{}{}
	}
	return existing
}

func collectCacheHints(expr ExpressionNode, acc map[string]int64) {
	if expr == nil || acc == nil {
		return
	}

	switch node := expr.(type) {
	case *ComparisonExpression:
		if node.CacheHintSeconds != nil &&
			len(node.Field.Parts) > 0 &&
			strings.EqualFold(node.Field.Parts[0], "concept") {
			if conceptName, ok := node.Value.(string); ok {
				key := strings.ToLower(strings.TrimSpace(conceptName))
				if key != "" {
					hint := int64(*node.CacheHintSeconds)
					if existing, ok := acc[key]; !ok || hint < existing {
						acc[key] = hint
					}
				}
			}
		}
	case *LogicalExpression:
		collectCacheHints(node.Left, acc)
		collectCacheHints(node.Right, acc)
	case *RelationshipExpression:
		collectCacheHints(node.Target, acc)
	case *SortExpression:
		collectCacheHints(node.Target, acc)
	case *PaginateExpression:
		collectCacheHints(node.Target, acc)
	case *SelectExpression:
		collectCacheHints(node.Target, acc)
	case *TimestampExpression:
		collectCacheHints(node.Target, acc)
	case *DepthExpression:
		collectCacheHints(node.Target, acc)
	case *CountExpression:
		collectCacheHints(node.Target, acc)
	case *ShapeExpression:
		collectCacheHints(node.Target, acc)
	case *BuiltinFunctionExpression:
		// Builtin functions don't have nested expressions to collect from
	}
}

func splitFieldParts(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ".")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func splitSelectFields(refs []FieldReference) ([]FieldReference, metadataSelection, error) {
	payload := make([]FieldReference, 0, len(refs))
	selection := metadataSelection{
		Fields: make(map[string]struct{}),
	}
	for _, ref := range refs {
		if name, wildcard, ok := metadataReferenceInfo(ref); ok {
			if wildcard {
				selection.IncludeAll = true
				selection.Fields = make(map[string]struct{})
			} else if !selection.IncludeAll {
				selection.Fields[name] = struct{}{}
			}
			continue
		}
		payload = append(payload, ref)
	}
	if selection.IncludeAll {
		return copyFieldReferences(payload), selection, nil
	}
	if selection.Fields == nil {
		selection.Fields = make(map[string]struct{})
	}
	if _, ok := selection.Fields["id"]; !ok {
		selection.Fields["id"] = struct{}{}
	}
	return copyFieldReferences(payload), selection, nil
}

func parseFieldReferenceLiteral(value string) (FieldReference, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return FieldReference{}, fmt.Errorf("field reference must not be empty")
	}
	ref := FieldReference{
		Raw:   trimmed,
		Parts: splitFieldParts(trimmed),
	}
	if len(ref.Parts) == 0 {
		return FieldReference{}, fmt.Errorf("field reference must not be empty")
	}
	if err := validateFieldReference(&ref); err != nil {
		return FieldReference{}, err
	}
	return ref, nil
}

func validateFieldReference(ref *FieldReference) error {
	if ref == nil {
		return fmt.Errorf("field reference is required")
	}
	if len(ref.Parts) == 0 {
		return fmt.Errorf("field reference must not be empty")
	}

	first := strings.TrimSpace(ref.Parts[0])
	// ctx.X in field-reference position (e.g. spec body
	// `ctx.participantType == "human"`) is the unified replacement for
	// `payload.X`. Normalize to payload so the rest of the validator +
	// executor pipeline keeps the single payload-rooted assumption.
	if strings.EqualFold(first, "ctx") {
		ref.Parts[0] = "payload"
		first = "payload"
		if ref.Raw != "" {
			if strings.HasPrefix(ref.Raw, "ctx.") {
				ref.Raw = "payload." + strings.TrimPrefix(ref.Raw, "ctx.")
			} else if strings.HasPrefix(ref.Raw, "ctx") {
				ref.Raw = "payload" + strings.TrimPrefix(ref.Raw, "ctx")
			}
		}
	}
	switch strings.ToLower(first) {
	case "payload":
		// allowed
	case "meta":
		if len(ref.Parts) != 2 {
			return fmt.Errorf("meta fields must use meta.<field> or meta.*")
		}
		if ref.Parts[1] == "*" {
			ref.Wildcard = true
			return nil
		}
		if _, ok := canonicalMetadataFieldName(ref.Parts[1]); !ok {
			return fmt.Errorf("%w: meta field %q is not supported", ErrInvalidArgument, ref.Parts[1])
		}
		return nil
	case "id", "concept", "type", "createdat", "createdby", "schema":
		if len(ref.Parts) > 1 {
			return fmt.Errorf("intrinsic field %q does not support nested paths", first)
		}
		return nil
	case "provenance":
		// provenance is a JSON-object intrinsic; nested paths address
		// the engine-stamped fields (kind, name, trigger, via). Bare
		// `provenance` returns the whole object.
		if len(ref.Parts) > 2 {
			return fmt.Errorf("provenance paths support one level (e.g. provenance.kind); got %q", ref.Raw)
		}
		if len(ref.Parts) == 2 {
			sub := strings.ToLower(strings.TrimSpace(ref.Parts[1]))
			switch sub {
			case "kind", "name", "trigger", "via":
				// allowed
			default:
				return fmt.Errorf("provenance field %q is not supported (kind|name|trigger|via)", ref.Parts[1])
			}
		}
		return nil
	case "actor":
		// actor.X (auth-context reference) on the LHS of a comparison
		// is valid inside context-spec bodies — `actor.role == "admin"`,
		// `actor.userId == "..."`, etc. The engine evaluates these
		// in-process at call time via the same actor-resolver used by
		// the policy evaluator; nothing is pushed into SQL.
		//
		// The spec registration path inspects whether a spec body
		// touches `payload.*` / intrinsics (row-spec, SQL pushdown) OR
		// only `actor.*` (context-spec, in-process eval) — mixed bodies
		// are flagged separately. Here we just admit the field-ref
		// shape; the row-vs-context classification happens in the spec
		// validator.
		if len(ref.Parts) < 2 {
			return fmt.Errorf("actor field references must include a sub-field (e.g. actor.role, actor.userId)")
		}
		return nil
	case "caller":
		// caller.X retired by #221. The migration-hint string is
		// sourced from baseparser.ErrCallerRetired so every
		// rejection site across both parsers emits identical text
		// (#244 / epic #218).
		return baseparser.ErrCallerRetired
	default:
		return fmt.Errorf("field %q must start with payload., meta., actor., or an intrinsic like id/concept/type/createdAt/createdBy/provenance", ref.Raw)
	}

	if len(ref.Parts) == 1 {
		return nil
	}

	for i := 1; i < len(ref.Parts); i++ {
		part := strings.TrimSpace(ref.Parts[i])
		if part == "" {
			return fmt.Errorf("field %q contains an empty segment", ref.Raw)
		}
	}

	last := ref.Parts[len(ref.Parts)-1]
	if last == "*" {
		if len(ref.Parts) < 3 {
			return fmt.Errorf("%w: payload wildcards must follow at least one property", ErrInvalidArgument)
		}
		ref.Wildcard = true
	} else {
		ref.Wildcard = false
	}

	for i := 1; i < len(ref.Parts)-1; i++ {
		if ref.Parts[i] == "*" {
			return fmt.Errorf("'*' may only appear as the final payload path segment")
		}
	}

	if ref.Wildcard && !strings.EqualFold(first, "payload") {
		return fmt.Errorf("wildcards are only supported for payload paths")
	}

	return nil
}

func isIdentifierStart(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-'
}

func isIdentifierChar(r rune) bool {
	switch {
	case unicode.IsLetter(r), unicode.IsDigit(r):
		return true
	case r == '_', r == '-', r == '.', r == '/', r == '*':
		return true
	default:
		return false
	}
}
