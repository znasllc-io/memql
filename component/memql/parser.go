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

	"github.com/znasllc-io/memql/component/memql/baseparser"
)

// Parse converts a MemQL query string into a QueryPlan.
func (e *MemQLEngine) Parse(query string) (*QueryPlan, error) {
	return e.parseWithFunctions(query, e.functions)
}

// parseWithFunctions is Parse with the function registry made explicit, so an
// authored-overlay call (ExecuteAuthored, MCP Tier-2 session constructs) can
// classify + inline session-authored functions that core does not define. The
// public Parse delegates with e.functions, so default behaviour is unchanged.
// Spec resolution still reads e.specs (the authored overlay is function-only in
// Phase 3; authored specs referenced from authored query filters are a tracked
// follow-up).
func (e *MemQLEngine) parseWithFunctions(query string, fns *FunctionRegistry) (*QueryPlan, error) {
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
	// returns a typed ErrUnsupportedQueryShape for the 8 shapes the
	// langparser doesn't cover (shape / select / in / concept==memql:
	// version / trailing-comma / @timestamp / :=); other errors are
	// genuine parse failures and propagate raw. The legacy memql
	// parser fallback (the post-#249 soak posture) is retired now
	// that sub-epic #286 migrated cognition's runtime callsites.
	//
	// inlineSpecs / timestamp / useLatest stay nil/zero -- the
	// detector rejects the shapes that would have populated them.
	root, err := parseViaLangparser(trimmed)
	if err != nil {
		return nil, err
	}
	if root == nil {
		return nil, ErrEmptyQuery
	}

	plan := &QueryPlan{
		Root:          root,
		Mutations:     nil,
		Filters:       nil,
		Relationships: nil,
		Timestamp:     nil,
		UseLatest:     false,
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
	// #250: inline-spec definitions are rejected at parseViaLangparser
	// (unsupportedQueryShape returns the typed error for `:=`), so
	// inlineSpecs is always nil here.
	if err := e.resolvePlanSpecs(plan, nil); err != nil {
		return nil, err
	}
	// Resolve function calls after spec resolution
	if err := resolvePlanFunctions(plan, fns, e.specs); err != nil {
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
	populateCacheHints(plan)
	populateConceptFields(plan)
	if err := validateSIContext(plan.Root); err != nil {
		return nil, err
	}

	return plan, nil
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
			if plan.Limit != nil || plan.Offset != nil {
				return nil, fmt.Errorf("multiple paginate() directives are not supported")
			}
			plan.Limit = node.Limit
			plan.Offset = node.Offset
			expr = node.Target
		case *TimestampExpression:
			if plan.Timestamp != nil || plan.UseLatest {
				return nil, fmt.Errorf("multiple asOf() directives are not supported")
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
	case *SortExpression, *SelectExpression, *PaginateExpression, *TimestampExpression, *DepthExpression, *ShapeExpression:
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
