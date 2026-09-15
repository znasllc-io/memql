package compiler

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/num"
)

// bareDottedIdentifier returns true when s looks like a dotted reference
// path (e.g. `agent.id`, `foo.bar.baz`). Condition-body expressions use
// this shape for step references (`stepName.payload.x`) and for forEach
// variables (`item.name`). A leading or trailing dot disqualifies the
// string — those fall out of the bare-reference category.
func bareDottedIdentifier(s string) bool {
	if s == "" {
		return false
	}
	if strings.HasPrefix(s, ".") || strings.HasSuffix(s, ".") {
		return false
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return false
	}
	for _, p := range parts {
		if !isSimpleIdentifier(p) {
			return false
		}
	}
	return true
}

// stepMethodAccessorPath reports whether s is a dotted reference whose only
// non-identifier segments are no-arg method accessors -- `first()`, `last()`,
// `empty()`, `count()`, `nodes()`, `Ran()` (the step-result accessors the
// runtime evaluator understands and normalizeStepMethodAccessors rewrites to
// dotted shorthand; Story 5 / #2303 retired the capitalized aliases except
// `Ran()`). Examples that match:
//
//	existing.first().payload.attachmentIds
//	rows.last().id
//
// This is the unquoted-runtime-reference companion to bareDottedIdentifier for
// the method-accessor case (which bareDottedIdentifier rejects because the
// `()` fail isSimpleIdentifier). It is intentionally strict: a segment with any
// argument inside the parens (a real function call) does NOT match, so genuine
// literal strings or other expressions are still quoted.
func stepMethodAccessorPath(s string) bool {
	if s == "" || strings.HasPrefix(s, ".") || strings.HasSuffix(s, ".") {
		return false
	}
	// Must actually carry a method accessor; otherwise bareDottedIdentifier
	// already covers the plain-dotted case.
	if !strings.Contains(s, "()") {
		return false
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return false
	}
	for _, p := range parts {
		if isSimpleIdentifier(p) {
			continue
		}
		// A no-arg method accessor: `Name()`.
		if strings.HasSuffix(p, "()") && isSimpleIdentifier(strings.TrimSuffix(p, "()")) {
			continue
		}
		return false
	}
	return true
}

// isSimpleIdentifier returns true for [a-zA-Z_][a-zA-Z0-9_]*.
func isSimpleIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if i == 0 {
			if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
				return false
			}
			continue
		}
		if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

// positionalArgValues reports whether args is a pure positional arg map
// (exactly the keys "0".."len-1") and, if so, returns the values in index
// order. A positional builtin call (`append(arr, item)`, `coalesce(a, b)`)
// stores its operands this way; rendering them positionally avoids the
// invalid `name(0=v0, 1=v1)` named form. Returns (nil, false) for any map
// that isn't a contiguous 0-based positional set (named args, gaps).
func positionalArgValues(args map[string]any) ([]any, bool) {
	if len(args) == 0 {
		return nil, false
	}
	out := make([]any, len(args))
	for i := range out {
		v, ok := args[strconv.Itoa(i)]
		if !ok {
			return nil, false
		}
		out[i] = v
	}
	return out, true
}

// isRuntimeReference checks if a string value is a runtime reference that should not be quoted.
// These are current expression paths such as event.xxx that the evaluator will resolve.
func isRuntimeReference(s string) bool {
	// Retired step roots are string data, never runtime references.
	candidate := strings.TrimSpace(s)
	if strings.HasPrefix(candidate, "$") || strings.HasPrefix(candidate, "item.") || strings.HasPrefix(candidate, "input.") {
		return false
	}

	// Also check for function calls that return runtime values
	funcPrefixes := []string{
		"concat(",
		"var(",
		"coalesce(",
		"first(",
		"last(",
		"timestamp(",
	}
	for _, prefix := range funcPrefixes {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}

	// Bare dotted identifiers (e.g., agent.id) should be preserved for runtime resolution.
	// If authors want a literal string containing dots, they should quote it explicitly.
	if bareDottedIdentifier(strings.TrimSpace(s)) {
		return true
	}

	// Step-method-accessor paths (e.g. `existing.first().payload.attachmentIds`)
	// are runtime references too. The parser captures these as raw source-text
	// strings when they appear as a positional-builtin operand (e.g. the array
	// arg of `append(existing.first().payload.attachmentIds, args.x)`). Without
	// this check valueToString quotes the whole thing into a STRING LITERAL --
	// so it never resolves against the step result and the literal source text
	// leaks into the write (the #1848 `["existing.first.payload.attachmentIds",
	// ...]` corruption). Recognise the `<ident>.first()/.last()/...` shape and
	// keep it unquoted so the evaluator's resolvePath navigates the step result.
	if stepMethodAccessorPath(strings.TrimSpace(s)) {
		return true
	}

	return false
}

// compileAutomation converts an automation FunctionDef to JSON output.
// attributeFlagPresent reports whether a flag-style `@name` attribute is present
// in the slice (used for @mcp promotion, Phase 4 #1534).
func attributeFlagPresent(attrs []*parser.Attribute, name string) bool {
	for _, a := range attrs {
		if a != nil && a.Name == name {
			return true
		}
	}
	return false
}

// compileArgsSchemaJSON lowers a parsed automation `args { }` block to the
// JSON shape the automations runtime (component/automations.ArgsSchema)
// unmarshals. Only the fields G1 (memql#2363) binds/validates against are
// emitted: name / type / optional / enum / maxLength / pattern / items /
// nested. `@default` never reaches here (the parser rejects it per #991) and
// `@description` carries no runtime slot, so neither is emitted.
func compileArgsSchemaJSON(schema *parser.ArgsSchema) map[string]any {
	if schema == nil {
		return nil
	}
	fields := make([]map[string]any, 0, len(schema.Fields))
	for _, f := range schema.Fields {
		if f == nil || strings.TrimSpace(f.Name) == "" {
			continue
		}
		fields = append(fields, compileArgsFieldJSON(f))
	}
	out := map[string]any{"fields": fields}
	if schema.AdditionalProperties != nil {
		out["additionalProperties"] = *schema.AdditionalProperties
	}
	return out
}

// compileArgsFieldJSON lowers a single args-block field (and any array-item /
// object-nested sub-schema) to its runtime JSON map.
func compileArgsFieldJSON(f *parser.ArgsField) map[string]any {
	fld := map[string]any{
		"name":     f.Name,
		"type":     f.Type,
		"optional": f.Optional,
	}
	if len(f.Enum) > 0 {
		fld["enum"] = append([]any(nil), f.Enum...)
	}
	if f.MaxLength > 0 {
		fld["maxLength"] = f.MaxLength
	}
	if strings.TrimSpace(f.Pattern) != "" {
		fld["pattern"] = f.Pattern
	}
	if f.Items != nil {
		fld["items"] = compileArgsFieldJSON(f.Items)
	}
	if len(f.Nested) > 0 {
		nested := make([]map[string]any, 0, len(f.Nested))
		for _, n := range f.Nested {
			if n == nil || strings.TrimSpace(n.Name) == "" {
				continue
			}
			nested = append(nested, compileArgsFieldJSON(n))
		}
		fld["nested"] = nested
	}
	return fld
}

func (c *Compiler) compileAutomation(def *parser.FunctionDef) (*AutomationOutput, error) {
	automation, ok := def.Body.(*parser.AutomationDef)
	if !ok || automation.Body == nil {
		return nil, fmt.Errorf("expected a statement body for automation %q", def.Name)
	}

	output := make(map[string]any)

	// Basic metadata
	output["name"] = def.Name
	if desc := parser.EffectiveDescription(automation.DocComment, automation.Description); desc != "" {
		output["description"] = desc
	}
	if automation.Schedule != "" {
		output["schedule"] = automation.Schedule
	}

	// Trigger (event-based)
	if automation.Trigger != nil && automation.Trigger.Before != "" {
		output["beforeWrite"] = map[string]any{"on": automation.Trigger.Before, "concept": automation.Trigger.Concept, "filter": automation.Trigger.Filter}
	}
	if automation.Trigger != nil && automation.Trigger.Before == "" {
		trigger := map[string]any{
			"event": automation.Trigger.Event,
		}
		if automation.Trigger.Filter != "" {
			trigger["filter"] = automation.Trigger.Filter
		}
		output["trigger"] = trigger
	}

	// @loop and @mode (epic memql#5380). The load judges both
	// (component/automations, loop_prepare.go), so each is carried as it
	// was written: until as canonical v1 source, like the trigger filter;
	// every mode flag, joined, so a second one is refused by name; and max
	// only when it was written, since a compiled 0 reads as the default.
	if loop := automation.Loop; loop != nil {
		out := map[string]any{}
		if loop.MaxDepthSet {
			out["maxDepth"] = loop.MaxDepth
		}
		if loop.Until != nil {
			out["until"] = ast.FormatExpr(loop.Until)
		}
		output["loop"] = out
	}
	if mode := automation.Mode; mode != nil {
		out := map[string]any{"kind": strings.Join(mode.Flags, ",")}
		if mode.MaxSet {
			out["max"] = mode.Max
		}
		output["mode"] = out
	}

	// Args contract (event-payload-binding ADR Decision 1, memql#2363): the
	// automation's typed input schema. When present, the scheduler/executor
	// bind event.payload -> args with fire-time validation (Decision 2) before
	// any statement runs.
	if def.ArgsSchema != nil && len(def.ArgsSchema.Fields) > 0 {
		output["args"] = compileArgsSchemaJSON(def.ArgsSchema)
	}

	// The statements compile in source order through CompileBody
	// (body_compile.go): each is a step, carried as canonical v1 source or a
	// `{"$expr": ...}` value leaf the runtime evaluates with EvalExpr.
	steps, err := compileStatementSteps(def, automation)
	if err != nil {
		return nil, err
	}
	output["steps"] = steps

	// Enabled state
	output["enabled"] = automation.Enabled

	// @mcp promotion (epic memql#1529 Phase 4 #1534): a flag attribute that
	// promotes the automation into its own first-class MCP tool. It may sit on
	// either the FunctionDef wrapper or the AutomationDef body, so check both.
	output["mcpPromoted"] = attributeFlagPresent(def.Attributes, "mcp") || attributeFlagPresent(automation.Attributes, "mcp")
	// @template (memql#5048): this automation is INVOKED BY A RUN that named
	// it, never fired by the graph. It is the third way an automation can be
	// reachable, alongside an event trigger and a schedule, and it exists
	// because the work spine's compiled templates are called rather than
	// triggered.
	output["template"] = attributeFlagPresent(def.Attributes, "template") || attributeFlagPresent(automation.Attributes, "template")

	return &AutomationOutput{
		Name:        def.Name,
		Description: parser.EffectiveDescription(automation.DocComment, automation.Description),
		JSON:        output,
	}, nil
}

// expressionToString converts an expression node back to MemQL string format.
//
// Every string literal it emits goes through parser.QuoteString rather than
// fmt.Sprintf("%q") -- and so do valueToString and mutationToString below, for
// the same reason (memql#3192). This is a serialize/RE-PARSE boundary: the
// text produced here is stored as step config and parsed again at runtime, by
// a lexer that implements the JSON escapes and only those. %q emits `\x00`,
// `\a` and `\v`, which that lexer rejects outright.
//
// Reachable from authored DSL, not only from runtime data: the lexer DECODES
// \u00XX, so an author writing "\u0007" in a .memql literal hands the compiler
// a Go string carrying a BEL, which came back out as `\a`. The value survives
// the first parse and dies on the second -- so load succeeds and the
// automation fails when it runs, which is the worst place to find it. Pinned
// by TestAuthoredUnicodeEscapeDecodesToAControlByte.
//
// Quoted faithfully, never substituted. These bytes are the author's source
// text round-tripping through an intermediate form; rewriting one would mean
// the automation that runs is not the automation that was written.
func (c *Compiler) expressionToString(expr parser.ExpressionNode) string {
	if expr == nil {
		return ""
	}

	switch e := expr.(type) {
	case *parser.LiteralExpr:
		switch v := e.Value.(type) {
		case string:
			return parser.QuoteString(v)
		case map[string]any, []any:
			// Object / array literals (e.g. an object-literal `return { k: <expr> }`).
			// Render proper MemQL via valueToString, which recurses into each value
			// (ExpressionNode -> its source form, nested map/array, scalar). Without
			// this they hit the default %v below and serialize as a Go map string
			// with AST pointers (`map[k:0x...]`), which the engine then rejects --
			// the #2274 object-literal-return bug.
			return c.valueToString(v)
		case float64:
			// Preserve float-ness across the re-parse boundary: a whole
			// float literal here (e.g. the `* 1.0` operand of a projection
			// fractional ratio) renders as `1` under %v, then re-parses as
			// int64 and collapses `* 1.0` to integer division (#2542). Emit
			// a decimal marker for the whole-number case; non-whole floats
			// already carry a `.` under %v.
			//
			// narrowing: GUARDED -- num.WholeInt64 IS the guard, and it
			// replaces `float64(int(v)) == v`, whose result is undefined for a
			// v outside int (memql#4779). The stakes here are the generated
			// source: a whole float that did not fit rendered `%d.0` of the
			// integer indefinite value, so `1e30` compiled to
			// `-9223372036854775808.0`. It now falls to %v, which prints an
			// exponent and therefore still re-parses as a float -- which is
			// the whole point of this branch.
			if whole, ok := num.WholeInt64(v); ok {
				return fmt.Sprintf("%d.0", whole)
			}
			return fmt.Sprintf("%v", v)
		default:
			return fmt.Sprintf("%v", v)
		}

	case *parser.ComparisonExpr:
		// Unary operators (== nil, != nil) have no right-hand value
		if e.Operator == parser.OpMissing || e.Operator == parser.OpNotMissing {
			if e.Operator == parser.OpMissing {
				return fmt.Sprintf("%s==nil", e.Field.Raw)
			}
			return fmt.Sprintf("%s!=nil", e.Field.Raw)
		}
		// Keyword operators need spacing
		opStr := string(e.Operator)
		switch e.Operator {
		case parser.OpIn, parser.OpOut, parser.OpHas, parser.OpStartsWith:
			return fmt.Sprintf("%s %s %v", e.Field.Raw, opStr, c.valueToString(e.Value))
		default:
			return fmt.Sprintf("%s%s%v", e.Field.Raw, opStr, c.valueToString(e.Value))
		}

	case *parser.LogicalExpr:
		left := c.logicalOperandString(e.Left)
		right := c.logicalOperandString(e.Right)
		// `&&`, not `;` (memql#5375). This function renders a parsed
		// expression BACK to source and the result is re-parsed --
		// compiler.CompileSource is called from component/automations/loader.go
		// -- so emitting the retired spelling would make every automation
		// filter refuse on its own round trip. Parse-identical: the two sat at
		// the same precedence level, so the grouping does not move.
		sep := "&&"
		if e.Op == parser.LogicalOr {
			sep = "||"
		}
		return fmt.Sprintf("%s%s%s", left, sep, right)

	case *parser.FunctionCallExpr:
		if len(e.Args) == 0 {
			return fmt.Sprintf("%s()", e.Name)
		}
		// A positional builtin (`append(arr, item)`, `coalesce(a, b)`) carries
		// its args under the index keys "0","1",.... Render those POSITIONALLY,
		// not as `0=v0, 1=v1`: the named form is invalid source for a
		// positional builtin and lands verbatim in the step value -- the #1840
		// forge attach corruption (`attachmentIds: [0="...", 1="..."]`). Mixed /
		// named args keep the `k=v` shape but are emitted in a STABLE key order
		// so the serialization is deterministic (Go map iteration is randomised,
		// which otherwise flips `0=..,1=..` between runs).
		if positional, ok := positionalArgValues(e.Args); ok {
			parts := make([]string, len(positional))
			for i, v := range positional {
				parts[i] = c.valueToString(v)
			}
			return fmt.Sprintf("%s(%s)", e.Name, strings.Join(parts, ", "))
		}
		keys := make([]string, 0, len(e.Args))
		for k := range e.Args {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		args := make([]string, len(keys))
		for i, k := range keys {
			args[i] = fmt.Sprintf("%s=%v", k, c.valueToString(e.Args[k]))
		}
		return fmt.Sprintf("%s(%s)", e.Name, strings.Join(args, ", "))

	case *parser.SortExpr:
		fields := []string{}
		for _, f := range e.Fields {
			prefix := ""
			if f.Direction == parser.SortDesc {
				prefix = "-"
			}
			fields = append(fields, prefix+f.Field)
		}
		return fmt.Sprintf("sort(%s)(%s)", strings.Join(fields, ","), c.expressionToString(e.Target))

	case *parser.PaginateExpr:
		args := []string{}
		if e.Limit != nil {
			args = append(args, fmt.Sprintf("limit=%d", *e.Limit))
		}
		return fmt.Sprintf("paginate(%s)(%s)", strings.Join(args, ","), c.expressionToString(e.Target))

	case *parser.SpecReferenceExpr:
		return e.Name

	case *parser.ConditionalFilterExpr:
		return fmt.Sprintf("?.%s", c.expressionToString(e.Filter))

	case *parser.ArgRefExpr:
		return "arg(" + parser.QuoteString(e.Path) + ")"

	// New accessor expressions
	case *parser.VarRefExpr:
		return "var(" + parser.QuoteString(e.Name) + ")"

	case *parser.EventRefExpr:
		return "event()"

	case *parser.ErrorExpr:
		return fmt.Sprintf("error(%s)", c.expressionToString(e.Message))

	case *parser.TimestampExprFunc:
		return "timestamp()"

	case *parser.FieldRefExpr:
		return fmt.Sprintf("field(%s, %s)", c.expressionToString(e.Object), parser.QuoteString(e.Key))

	case *parser.DotAccessExpr:
		// Serialize `DotAccess{DotAccess{call, "payload"}, "name"}` back
		// as `call.payload.name`. The runtime evaluator's resolvePath
		// parses the flat dotted string via its parts split — see
		// automations/evaluator.go:resolvePath.
		return fmt.Sprintf("%s.%s", c.expressionToString(e.Object), e.Field)

	case *parser.ConcatExpr:
		args := make([]string, len(e.Args))
		for i, arg := range e.Args {
			args[i] = c.expressionToString(arg)
		}
		return fmt.Sprintf("concat(%s)", strings.Join(args, ", "))

	case *parser.CoalesceExpr:
		args := make([]string, len(e.Args))
		for i, arg := range e.Args {
			args[i] = c.expressionToString(arg)
		}
		return fmt.Sprintf("coalesce(%s)", strings.Join(args, ", "))

	case *parser.CondExpr:
		return fmt.Sprintf("cond(%s, %s, %s)",
			c.expressionToString(e.Condition),
			c.expressionToString(e.Then),
			c.expressionToString(e.Else))

	case *parser.TernaryExpr:
		return fmt.Sprintf("%s ? %s : %s",
			c.expressionToString(e.Condition),
			c.expressionToString(e.Then),
			c.expressionToString(e.Else))

	case *parser.FirstExpr:
		return fmt.Sprintf("first(%s)", c.expressionToString(e.Target))

	case *parser.LastExpr:
		return fmt.Sprintf("last(%s)", c.expressionToString(e.Target))

	case *parser.LowerExpr:
		return fmt.Sprintf("lower(%s)", c.expressionToString(e.Target))

	case *parser.UpperExpr:
		return fmt.Sprintf("upper(%s)", c.expressionToString(e.Target))

	case *parser.TrimExpr:
		return fmt.Sprintf("trim(%s)", c.expressionToString(e.Target))

	case *parser.HashExpr:
		return fmt.Sprintf("hash(%s)", c.expressionToString(e.Target))

	case *parser.ShortIdExpr:
		return fmt.Sprintf("shortId(%s)", c.expressionToString(e.Target))

	case *parser.CanonicalIdExpr:
		// Round-trips canonicalId(value, "<conceptType>") back to its
		// source form. Without this case, the default branch below
		// would emit `*ast.CanonicalIdExpr` (the Go %T type name)
		// into the generated code, which the engine then takes as
		// the literal arg value -- exactly the leak that produced
		// `default:v1:agents:agent:*ast.CanonicalIdExpr` rows in
		// the participant table.
		return fmt.Sprintf("canonicalId(%s, %s)",
			c.expressionToString(e.Value),
			parser.QuoteString(e.Concept))

	case *parser.ContainsExpr:
		return fmt.Sprintf("contains(%s, %s)",
			c.expressionToString(e.Target),
			c.expressionToString(e.Substring))

	case *parser.ArithmeticExpr:
		// Binary arithmetic (#2316/#2542): re-emit the operator form,
		// fully parenthesized so the re-parse rebuilds exactly this tree
		// regardless of operator precedence. Without this case a logic
		// terminal return like `return revenue / orders` serialized to
		// `<<unsupported expression *ast.ArithmeticExpr>>` and the
		// automation failed at runtime even though memqllint passed.
		return fmt.Sprintf("(%s %s %s)",
			c.expressionToString(e.Left), e.Op, c.expressionToString(e.Right))

	case *parser.BinaryComparisonExpr:
		// Expression-led comparison (#2542 item 5 residual): re-emit the
		// operator form, fully parenthesized so the re-parse rebuilds exactly
		// this tree regardless of precedence. Like ArithmeticExpr this is an
		// in-memory-only node; the runtime evaluates it via evalCollScalar (a
		// lambda body / logic terminal return), never in SQL. Without this
		// case a logic return of a bare expression-led comparison serialized
		// to `<<unsupported expression *ast.BinaryComparisonExpr>>`.
		return fmt.Sprintf("(%s %s %s)",
			c.expressionToString(e.Left), string(e.Operator), c.expressionToString(e.Right))

	// Date/duration builtins (#2541): canonical re-parseable call forms.
	// The logic runner's local evaluator and the automations condition
	// evaluator dispatch these by name (memql.EvaluateDateBuiltin), so the
	// emitted source must keep the builtin's spelling exactly.
	case *parser.AddDurationExpr:
		return fmt.Sprintf("addDuration(%s, %s)",
			c.expressionToString(e.Timestamp), c.expressionToString(e.Duration))

	case *parser.DaysBetweenExpr:
		return fmt.Sprintf("daysBetween(%s, %s)",
			c.expressionToString(e.Date1), c.expressionToString(e.Date2))

	case *parser.NotExpr:
		// The bang `!` shape. (The #2612 NotExpr{EqExpr} != case is gone:
		// since memql#2654 the arg grammar emits BinaryComparisonExpr for
		// equality, serialized by its own case in the operator form the
		// runtime re-parse requires -- not(<comparison>) is not a working
		// cond predicate.)
		return fmt.Sprintf("not(%s)", c.expressionToString(e.Target))

	case *parser.AndExpr:
		args := make([]string, len(e.Args))
		for i, arg := range e.Args {
			args[i] = c.expressionToString(arg)
		}
		return fmt.Sprintf("and(%s)", strings.Join(args, ", "))

	case *parser.OrExpr:
		args := make([]string, len(e.Args))
		for i, arg := range e.Args {
			args[i] = c.expressionToString(arg)
		}
		return fmt.Sprintf("or(%s)", strings.Join(args, ", "))

	case *parser.MethodCallExpr:
		// A collection chain captured as a multi-statement logic step RHS
		// (#2317) carries its verbatim source text on Raw -- emit it as-is so
		// the chain reaches the runtime exactly as the author wrote it (the
		// per-node reconstruction below emits engine-IR for an arg receiver,
		// e.g. `arg("members")`, which the collection evaluator can't resolve).
		if e.Raw != "" {
			return e.Raw
		}
		// A `<receiver>.<method>(<args>)` chain. Story 5 (ADR §2.2 / #2303)
		// unified the step-result accessors on the lowercase spelling
		// (`existing.first()`, `rows.count()`, `found.empty()`), which the
		// parser now lexes into a MethodCallExpr because those names overlap
		// the Story 4 (#2302) collection-method set. Round-trip the chain back
		// to its source text so a logic-body `return existing.first()` reaches
		// the runtime as a navigable step reference (the local Evaluator's
		// EvaluateStepReference resolves it via resolvePath's step-accessor
		// cases) instead of leaking the Go `%T` type name as a literal.
		args := make([]string, len(e.Args))
		for i, a := range e.Args {
			args[i] = c.expressionToString(a)
		}
		return fmt.Sprintf("%s.%s(%s)", c.expressionToString(e.Receiver), e.Method, strings.Join(args, ", "))

	case *parser.LambdaExpr:
		// Arrow lambda carried by a collection-method chain (#2302), e.g. the
		// `m => m.active` in `return rows.where(m => m.active).count()` or the
		// `g => {worker: g.key, acc: ...}` projection body (#2542 item 3). A
		// lambda-carrying chain in TERMINAL RETURN position reaches this
		// reconstruction path (the compiler serializes the return expression
		// per node), where the absent case emitted `<<unsupported expression
		// *ast.LambdaExpr>>` and the automation failed at runtime even though
		// memqllint passed. Emit the re-parseable arrow form so the runtime
		// re-parse (EvaluateCollectionChainString) rebuilds the same chain: a
		// single param stays bare (`m => ...`), multiple params take the
		// parenthesised list (`(acc, x) => ...`). The body round-trips through
		// expressionToString / valueToString (object-literal projection bodies
		// included).
		body := c.expressionToString(e.Body)
		if len(e.Params) == 1 {
			return fmt.Sprintf("%s => %s", e.Params[0], body)
		}
		return fmt.Sprintf("(%s) => %s", strings.Join(e.Params, ", "), body)

	case *ast.IdentExpr, *ast.MemberExpr, *ast.CallExpr, *ast.UnaryExpr, *ast.BinaryExpr, *ast.ListExpr, *ast.MapExpr, *ast.ParenExpr:
		// An edition-2026 node: a query's filter lambda body (the struct-form
		// rewriter joins it as `concept==<id> && (row => ...)`), or a
		// mutation value. Its canonical source is its serialisation -- the
		// internal form reads a v1 lambda where it meets an operand
		// (parser.tryParseV1LambdaOperand), and a mutation value parses
		// with the v1 grammar.
		return ast.FormatExpr(e)

	default:
		// SAFETY: emitting `%T` here puts the Go type name (e.g.
		// `*parser.CanonicalIdExpr`) into the generated code as a
		// literal -- silently breaking downstream evaluation. If you
		// added a new AST node, add an explicit case above. Until
		// then, surround the type name with `<<>>` so the breakage
		// is loud at runtime instead of producing plausible-looking
		// id strings.
		return fmt.Sprintf("<<unsupported expression %T>>", expr)
	}
}

// logicalOperandString is expressionToString for an operand of `;` / `,`. A
// lambda operand is parenthesised, as the struct-form rewriter writes a v1
// filter's join (`concept==<id> && (row => ...)`): a lambda's body extends as
// far right as it can, so a bare one would take in whatever follows it.
func (c *Compiler) logicalOperandString(n parser.ExpressionNode) string {
	if _, ok := n.(*ast.LambdaExpr); ok {
		return "(" + c.expressionToString(n) + ")"
	}
	return c.expressionToString(n)
}

// mutationToString converts a mutation statement to MemQL string format.
func (c *Compiler) mutationToString(m *parser.MutationStmt) string {
	if m == nil {
		return ""
	}

	parts := []string{parser.QuoteString(m.Concept)}

	if m.IDTemplate != nil {
		switch id := m.IDTemplate.(type) {
		case string:
			if strings.TrimSpace(id) != "" {
				parts = append(parts, "id="+parser.QuoteString(id))
			}
		case parser.ExpressionNode:
			parts = append(parts, fmt.Sprintf("id=%s", c.expressionToString(id)))
		default:
			// Best-effort: treat as literal string
			parts = append(parts, "id="+parser.QuoteString(fmt.Sprintf("%v", id)))
		}
	}

	if m.PayloadRaw != "" {
		trimmed := strings.TrimSpace(m.PayloadRaw)
		// Check if PayloadRaw is the full object literal syntax (contains both id and payload)
		// In that case, output directly without "payload=" prefix
		if strings.HasPrefix(trimmed, "{") && (strings.Contains(trimmed, "id:") || strings.Contains(trimmed, "\"id\":")) && m.IDTemplate == nil {
			// Object literal syntax: insert("concept", { id: ..., payload: {...} })
			parts = append(parts, trimmed)
		} else {
			// Named argument syntax: insert("concept", payload={...})
			parts = append(parts, fmt.Sprintf("payload=%s", m.PayloadRaw))
		}
	}

	if m.ParentTemplate != nil {
		switch v := m.ParentTemplate.(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				parts = append(parts, "parent="+parser.QuoteString(v))
			}
		case parser.ExpressionNode:
			parts = append(parts, fmt.Sprintf("parent=%s", c.expressionToString(v)))
		default:
			parts = append(parts, "parent="+parser.QuoteString(fmt.Sprintf("%v", v)))
		}
	}

	if m.AliasOfTemplate != nil {
		switch v := m.AliasOfTemplate.(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				parts = append(parts, "aliasOf="+parser.QuoteString(v))
			}
		case parser.ExpressionNode:
			parts = append(parts, fmt.Sprintf("aliasOf=%s", c.expressionToString(v)))
		default:
			parts = append(parts, "aliasOf="+parser.QuoteString(fmt.Sprintf("%v", v)))
		}
	}

	return fmt.Sprintf("insert(%s)", strings.Join(parts, ", "))
}

// valueToString converts a value to its string representation.
func (c *Compiler) valueToString(v any) string {
	switch val := v.(type) {
	case string:
		// Check if the string is a reference that should not be quoted
		// These are runtime references that the evaluator will resolve
		if isRuntimeReference(val) {
			return val
		}
		return parser.QuoteString(val)
	case float64:
		// narrowing: GUARDED -- num.WholeInt64 IS the guard, replacing
		// `float64(int(val)) == val`, whose result is undefined for a val
		// outside int (memql#4779).
		if whole, ok := num.WholeInt64(val); ok {
			// Keep an explicit decimal marker so a whole-valued float
			// (1.0, 100.0) round-trips as a FLOAT literal across the
			// serialize/re-parse boundary. Rendering it as `1` makes
			// ParseNumericLiteral decode int64 on re-parse, silently
			// switching arithmetic to integer division -- the #2542
			// `... * 1.0` fractional-ratio idiom collapsing to `... * 1`.
			return fmt.Sprintf("%d.0", whole)
		}
		return fmt.Sprintf("%f", val)
	case bool:
		if val {
			return "true"
		}
		return "false"
	case []any:
		items := []string{}
		for _, item := range val {
			items = append(items, c.valueToString(item))
		}
		return fmt.Sprintf("(%s)", strings.Join(items, ","))
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s: %s", k, c.valueToString(val[k])))
		}
		return fmt.Sprintf("{%s}", strings.Join(parts, ", "))
	case parser.ExpressionNode:
		return c.expressionToString(val)
	default:
		return fmt.Sprintf("%v", v)
	}
}
