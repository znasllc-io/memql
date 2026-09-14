package compiler

// automation_generator_v1.go -- compiling an automation (or logic) body
// (epic memql#5363, memql#5367).
//
// Every expression of a body is canonical edition-2026 source or a parsed v1
// node, and it is carried as written: the runtime evaluates it with EvalExpr
// (component/automations/expressions_v1.go), so nothing here rewrites
// authored text into another dialect. What it emits:
//
//   - every EXPRESSION FIELD -- a position that is always an expression: a
//     step `condition`, `forEach.source`, `forEach.filter`,
//     `switch.expression`, `shape.source`, `trigger.filter`, a `query.query`
//     and the logic `_return` -- carries canonical v1 source, unchanged;
//   - every VALUE LEAF is EncodeValueLeaf's encoding: plain JSON for a
//     literal, `{"$expr": "<canonical v1 source>"}` for an expression, a map
//     or list literal walked by the same rule. That is every entry of an
//     args / payload / body map, and every string-typed field that holds a
//     value -- `event.topic`, `webhook.url` and its header values,
//     `mutation.id` / `parent` / `aliasOf` -- which the runtime decodes into
//     its Go string fields through value_leaves.go;
//   - a call of a catalog function as a whole step (`x := lower(y)`) is an
//     expression the runtime evaluates in process, so it compiles to a query
//     step whose query is that call; every other function step is a construct
//     call for the engine.
//
// The step order is a topological sort fed by a reference walk: the free
// names of every expression in a step (lambda parameters bound, a forEach's
// loop variable bound inside its body) that are step ids, plus `steps.<id>`
// reads.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/functions"
	"github.com/znasllc-io/memql/component/language/parser"
)

// exprLeafKey is the one key of a compiled value leaf that is an expression.
const exprLeafKey = "$expr"

// compileStepV1 compiles one step.
func (c *Compiler) compileStepV1(step *parser.StepDef) (map[string]any, error) {
	output := map[string]any{
		"id":   step.ID,
		"type": string(step.Type),
	}
	if step.Name != "" {
		output["name"] = step.Name
	}
	if step.Condition != "" {
		// Canonical v1 source, as the parser printed it: no rewrite.
		output["condition"] = step.Condition
	}
	if step.OnError != "" {
		output["onError"] = step.OnError
	}
	if step.RetryCount > 0 {
		output["retryCount"] = step.RetryCount
	}
	at := func(pos string) string { return fmt.Sprintf("step %q %s", step.ID, pos) }

	switch step.Type {
	case parser.StepTypeQuery:
		cfg, ok := step.Config.(*parser.QueryStepConfig)
		if !ok {
			return nil, fmt.Errorf("%s: expected a query configuration, got %T", at("query"), step.Config)
		}
		src, err := v1Source(at("query"), cfg.Query)
		if err != nil {
			return nil, err
		}
		output["query"] = map[string]any{"query": src}

	case parser.StepTypeMutation:
		cfg, ok := step.Config.(*parser.MutationStepConfig)
		if !ok {
			return nil, fmt.Errorf("%s: expected a mutation configuration, got %T", at("mutation"), step.Config)
		}
		mutation, err := compileMutationConfigV1(at("mutation"), cfg.Mutation)
		if err != nil {
			return nil, err
		}
		output["mutation"] = mutation

	case parser.StepTypeFunction:
		cfg, ok := step.Config.(*parser.FunctionStepConfig)
		if !ok {
			return nil, fmt.Errorf("%s: expected a function configuration, got %T", at("call"), step.Config)
		}
		if err := c.compileFunctionStepV1(step, cfg, output); err != nil {
			return nil, err
		}

	case parser.StepTypeAction:
		cfg, ok := step.Config.(*parser.ActionStepConfig)
		if !ok {
			return nil, fmt.Errorf("%s: expected an action configuration, got %T", at("action"), step.Config)
		}
		action := map[string]any{"ref": cfg.Ref}
		if cfg.Surface != "" {
			action["surface"] = cfg.Surface
		}
		if len(cfg.Args) > 0 {
			args, err := compileValueMapV1(at("action args"), cfg.Args)
			if err != nil {
				return nil, err
			}
			action["args"] = args
		}
		output["action"] = action

	case parser.StepTypeForEach:
		cfg, ok := step.Config.(*parser.ForEachStepConfig)
		if !ok {
			return nil, fmt.Errorf("%s: expected a forEach configuration, got %T", at("forEach"), step.Config)
		}
		forEach := map[string]any{"source": cfg.Source}
		if cfg.Filter != "" {
			forEach["filter"] = cfg.Filter
		}
		if cfg.As != "" {
			forEach["as"] = cfg.As
		}
		if cfg.Concurrency > 0 {
			forEach["concurrency"] = cfg.Concurrency
		}
		doSteps := []map[string]any{}
		for i := range cfg.Do {
			compiled, err := c.compileStepV1(&cfg.Do[i])
			if err != nil {
				return nil, err
			}
			doSteps = append(doSteps, compiled)
		}
		forEach["do"] = doSteps
		output["forEach"] = forEach

	case parser.StepTypeParallel:
		cfg, ok := step.Config.(*parser.ParallelStepConfig)
		if !ok {
			return nil, fmt.Errorf("%s: expected a parallel configuration, got %T", at("parallel"), step.Config)
		}
		parallel := map[string]any{"failFast": cfg.FailFast}
		if cfg.Wait != "" {
			parallel["wait"] = cfg.Wait
		}
		branches := []map[string]any{}
		for i := range cfg.Branches {
			compiled, err := c.compileStepV1(&cfg.Branches[i])
			if err != nil {
				return nil, err
			}
			branches = append(branches, compiled)
		}
		parallel["branches"] = branches
		output["parallel"] = parallel

	case parser.StepTypeSwitch:
		cfg, ok := step.Config.(*parser.SwitchStepConfig)
		if !ok {
			return nil, fmt.Errorf("%s: expected a switch configuration, got %T", at("switch"), step.Config)
		}
		switchCfg := map[string]any{"expression": cfg.Expression}
		cases := make(map[string]any, len(cfg.Cases))
		labels := make([]string, 0, len(cfg.Cases))
		for label := range cfg.Cases {
			labels = append(labels, label)
		}
		sort.Strings(labels)
		for _, label := range labels {
			steps, err := c.compileStepListV1(cfg.Cases[label])
			if err != nil {
				return nil, err
			}
			cases[label] = map[string]any{"steps": steps}
		}
		switchCfg["cases"] = cases
		if cfg.Default != nil {
			steps, err := c.compileStepListV1(cfg.Default)
			if err != nil {
				return nil, err
			}
			switchCfg["default"] = map[string]any{"steps": steps}
		}
		output["switch"] = switchCfg

	default:
		return nil, fmt.Errorf("%s: step type %q has no edition-2026 form", at(""), step.Type)
	}
	return output, nil
}

func (c *Compiler) compileStepListV1(sc *parser.SwitchCase) ([]map[string]any, error) {
	out := []map[string]any{}
	if sc == nil {
		return out, nil
	}
	for i := range sc.Steps {
		compiled, err := c.compileStepV1(&sc.Steps[i])
		if err != nil {
			return nil, err
		}
		out = append(out, compiled)
	}
	return out, nil
}

// compileFunctionStepV1 compiles a call step: a helper call (event, webhook,
// shape, mutation), a sub-automation, a catalog function evaluated in
// process, or a construct call.
func (c *Compiler) compileFunctionStepV1(step *parser.StepDef, cfg *parser.FunctionStepConfig, output map[string]any) error {
	at := func(pos string) string { return fmt.Sprintf("step %q %s", step.ID, pos) }
	helperArgs := cfg.Args
	if len(helperArgs) == 1 {
		if obj, ok := helperArgs["0"].(map[string]any); ok {
			helperArgs = obj
		}
	}
	switch cfg.Name {
	case "query":
		if _, ok := helperArgs["query"]; ok {
			return fmt.Errorf("%s: the query() helper takes a filter string, which has no edition-2026 reading -- call a query construct (`query <name>(k: v)`)", at("query"))
		}
	case "event", "publishEvent":
		if topic, ok := helperArgs["topic"]; ok {
			eventCfg := map[string]any{}
			src, err := compileLeafFieldV1(at("event topic"), topic)
			if err != nil {
				return err
			}
			eventCfg["topic"] = src
			if kind, ok := helperArgs["kind"]; ok {
				lit, err := v1LiteralString(at("event kind"), kind)
				if err != nil {
					return err
				}
				eventCfg["kind"] = lit
			}
			if payload, ok := helperArgs["payload"]; ok {
				m, err := compileValueMapPositionV1(at("event payload"), payload)
				if err != nil {
					return err
				}
				eventCfg["payload"] = m
			}
			output["type"] = "event"
			output["event"] = eventCfg
			return nil
		}
	case "webhook":
		if url, ok := helperArgs["url"]; ok {
			src, err := compileLeafFieldV1(at("webhook url"), url)
			if err != nil {
				return err
			}
			whCfg := map[string]any{"url": src, "method": "POST"}
			if m, ok := helperArgs["method"]; ok {
				lit, err := v1LiteralString(at("webhook method"), m)
				if err != nil {
					return err
				}
				whCfg["method"] = lit
			}
			if h, ok := helperArgs["headers"]; ok {
				headers, err := compileHeadersV1(at("webhook headers"), h)
				if err != nil {
					return err
				}
				whCfg["headers"] = headers
			}
			if b, ok := helperArgs["body"]; ok {
				m, err := compileValueMapPositionV1(at("webhook body"), b)
				if err != nil {
					return err
				}
				whCfg["body"] = m
			}
			if t, ok := helperArgs["timeout"]; ok {
				lit, err := v1LiteralString(at("webhook timeout"), t)
				if err != nil {
					return err
				}
				whCfg["timeout"] = lit
			}
			output["type"] = "webhook"
			output["webhook"] = whCfg
			return nil
		}
	case "shape":
		if src, ok := helperArgs["source"]; ok {
			source, err := v1TextSource(at("shape source"), src)
			if err != nil {
				return err
			}
			shapeCfg := map[string]any{"source": source}
			if tpl, ok := helperArgs["template"]; ok {
				// A shape template is the shape helpers' own template
				// language, not an expression position: it must be literal.
				lit, err := v1LiteralValue(at("shape template"), tpl)
				if err != nil {
					return err
				}
				shapeCfg["template"] = lit
			}
			output["type"] = "shape"
			output["shape"] = shapeCfg
			return nil
		}
	case "mutation":
		if concept, ok := helperArgs["concept"]; ok {
			lit, err := v1LiteralString(at("mutation concept"), concept)
			if err != nil {
				return err
			}
			mutCfg := map[string]any{"concept": lit}
			for _, field := range []string{"id", "parent", "aliasOf"} {
				if v, ok := helperArgs[field]; ok {
					src, err := compileLeafFieldV1(at("mutation "+field), v)
					if err != nil {
						return err
					}
					mutCfg[field] = src
				}
			}
			if v, ok := helperArgs["payload"]; ok {
				m, err := compileValueMapPositionV1(at("mutation payload"), v)
				if err != nil {
					return err
				}
				mutCfg["payload"] = m
			}
			output["type"] = "mutation"
			output["mutation"] = mutCfg
			return nil
		}
	}

	// Sub-automation dispatch: the kindPrefix rewriter's
	// `automation<Name>` spelling (see compileStep).
	if strings.HasPrefix(cfg.Name, "automation") && len(cfg.Name) > len("automation") {
		subName := cfg.Name[len("automation"):]
		subName = strings.ToLower(subName[:1]) + subName[1:]
		autoCfg := map[string]any{"name": subName}
		if len(cfg.Args) > 0 {
			args, err := compileValueMapV1(at("sub-automation args"), cfg.Args)
			if err != nil {
				return err
			}
			autoCfg["args"] = args
		}
		output["type"] = "automation"
		output["automation"] = autoCfg
		return nil
	}

	// A catalog function as a whole step is an expression, evaluated in
	// process: the engine registers no function by that name. Only a
	// POSITIONAL call is one -- a construct call names its arguments, and a
	// zero-argument call keeps its construct reading, so a construct that
	// happens to share a catalog name is never swallowed.
	if positional, ok := positionalArgValues(cfg.Args); ok {
		if _, isCatalog := functions.Lookup(cfg.Name); isCatalog {
			call := &ast.CallExpr{Name: cfg.Name}
			for i, v := range positional {
				node, err := v1ValueNode(fmt.Sprintf("%s argument %d", at("call "+cfg.Name), i), v)
				if err != nil {
					return err
				}
				call.Args = append(call.Args, node)
			}
			output["type"] = "query"
			output["query"] = map[string]any{"query": ast.FormatExpr(call)}
			return nil
		}
	}
	if _, retired := functions.RetiredFunctions()[cfg.Name]; retired {
		return fmt.Errorf("%s: %s() is retired in edition 2026 -- write %s", at("call"), cfg.Name, functions.RetiredFunctions()[cfg.Name])
	}

	// A construct call for the engine: its arguments are values.
	function := map[string]any{"name": cfg.Name}
	if len(cfg.Args) > 0 {
		args, err := compileValueMapV1(at("args"), cfg.Args)
		if err != nil {
			return err
		}
		function["args"] = args
	}
	output["function"] = function
	return nil
}

// compileMutationConfigV1 compiles a mutation step's config, whose templates
// hold v1 nodes and whose payload is MutationStmt.PayloadExpr, a map literal.
// The id / parent / aliasOf templates are string-typed expression positions;
// the payload's values are value leaves. A payload of the form
// `{id: ..., payload: {...}}` names the id inside it.
func compileMutationConfigV1(where string, m *parser.MutationStmt) (map[string]any, error) {
	if m == nil {
		return nil, fmt.Errorf("%s: missing mutation", where)
	}
	config := map[string]any{"concept": m.Concept}
	texts := map[string]any{"id": m.IDTemplate, "parent": m.ParentTemplate, "aliasOf": m.AliasOfTemplate}
	if strings.TrimSpace(m.PayloadRaw) != "" && m.PayloadExpr == nil {
		return nil, fmt.Errorf("%s: the payload was not parsed as an edition-2026 map literal", where)
	}
	if m.PayloadExpr != nil {
		payload, ok := ast.Unparen(m.PayloadExpr).(*ast.MapExpr)
		if !ok {
			return nil, fmt.Errorf("%s: the payload must be an object literal, got %s", where, describeV1Value(m.PayloadExpr))
		}
		entries := map[string]ast.ExpressionNode{}
		for _, en := range payload.Entries {
			entries[en.Key] = en.Value
		}
		if id, has := entries["id"]; has {
			texts["id"] = id
			delete(entries, "id")
		}
		body := &ast.MapExpr{}
		if inner, has := entries["payload"]; has {
			nested, ok := ast.Unparen(inner).(*ast.MapExpr)
			if !ok {
				return nil, fmt.Errorf("%s: the nested payload must be an object literal, got %s", where, describeV1Value(inner))
			}
			body = nested
		} else {
			for _, en := range payload.Entries {
				if _, kept := entries[en.Key]; kept {
					body.Entries = append(body.Entries, en)
				}
			}
		}
		encoded, err := compileValueMapPositionV1(where+" payload", body)
		if err != nil {
			return nil, err
		}
		config["payload"] = encoded
	}
	for _, field := range []string{"id", "parent", "aliasOf"} {
		v := texts[field]
		if v == nil {
			continue
		}
		if s, isString := v.(string); isString && strings.TrimSpace(s) == "" {
			continue
		}
		src, err := compileLeafFieldV1(where+" "+field, v)
		if err != nil {
			return nil, err
		}
		config[field] = src
	}
	return config, nil
}

// compileHeadersV1 compiles webhook headers: each value is a string-typed
// expression position.
func compileHeadersV1(where string, v any) (map[string]any, error) {
	var entries map[string]any
	switch x := v.(type) {
	case map[string]any:
		entries = x
	case *ast.MapExpr:
		entries = make(map[string]any, len(x.Entries))
		for _, e := range x.Entries {
			entries[e.Key] = e.Value
		}
	default:
		return nil, fmt.Errorf("%s: headers must be an object literal, got %T", where, v)
	}
	out := make(map[string]any, len(entries))
	for k, hv := range entries {
		src, err := compileLeafFieldV1(where+"."+k, hv)
		if err != nil {
			return nil, err
		}
		out[k] = src
	}
	return out, nil
}

// EncodeValueLeaf encodes one edition-2026 value expression as it is stored
// in a compiled value map -- a call's args, a payload, a body. It is THE
// encoding: the automation compiler and the body compiler (epic memql#5370)
// both call it, so the two emit identical JSON, and the automations runtime
// decodes it (component/automations: PrepareExpressions parses each leaf once,
// ResolveV1Value evaluates it).
//
// The rule, applied after stripping any ParenExpr wrappers:
//
//   - a LiteralExpr is its plain JSON value (a string, an int64 or float64, a
//     bool);
//   - a NilExpr is JSON null;
//   - a MapExpr is a JSON object whose every value is EncodeValueLeaf of the
//     entry, recursively -- a map with one expression value is still an object,
//     with a `$expr` leaf inside, never a whole-map `$expr`;
//   - a ListExpr is a JSON array of EncodeValueLeaf of each element;
//   - anything else is `{"$expr": ast.FormatExpr(n)}`.
//
// The decoder walks the maps and arrays and evaluates each `$expr` leaf with
// EvalExpr, applying EvalExpr's own container rule: a leaf that evaluates to
// the Absent sentinel omits its key or element, and an explicit nil stays
// null. Decoding EncodeValueLeaf(e) therefore yields what EvalExpr(e) yields
// (TestEncodeValueLeafRoundTrip, component/automations).
//
// n must be an edition-2026 node: a node of the internal query form has no
// canonical source (compileValueV1 refuses one before it gets here). A nil
// node encodes as null.
func EncodeValueLeaf(n ast.ExpressionNode) any {
	if n == nil {
		return nil
	}
	switch e := ast.Unparen(n).(type) {
	case *ast.LiteralExpr:
		return e.Value
	case *ast.NilExpr:
		return nil
	case *ast.MapExpr:
		obj := make(map[string]any, len(e.Entries))
		for _, en := range e.Entries {
			obj[en.Key] = EncodeValueLeaf(en.Value)
		}
		return obj
	case *ast.ListExpr:
		arr := make([]any, len(e.Elems))
		for i, el := range e.Elems {
			arr[i] = EncodeValueLeaf(el)
		}
		return arr
	default:
		return map[string]any{exprLeafKey: ast.FormatExpr(e)}
	}
}

// compileValueMapV1 compiles every value of a call's argument map.
func compileValueMapV1(where string, m map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if k == exprLeafKey {
			return nil, fmt.Errorf("%s: %q is reserved for compiled expressions and cannot be a key", where, exprLeafKey)
		}
		cv, err := compileValueV1(where+"."+k, v)
		if err != nil {
			return nil, err
		}
		out[k] = cv
	}
	return out, nil
}

// compileValueV1 compiles one value of a v1 body: a node through
// EncodeValueLeaf, once it is known to be an edition-2026 node; a Go map or
// list (the parser's argument map) recursively; a Go scalar as the literal it
// is.
func compileValueV1(where string, v any) (any, error) {
	switch x := v.(type) {
	case nil, string, bool, float64, float32, int, int32, int64:
		return x, nil
	case map[string]any:
		return compileValueMapV1(where, x)
	case []any:
		out := make([]any, len(x))
		for i, el := range x {
			cv, err := compileValueV1(fmt.Sprintf("%s[%d]", where, i), el)
			if err != nil {
				return nil, err
			}
			out[i] = cv
		}
		return out, nil
	case ast.ExpressionNode:
		if err := requireV1Node(where, x); err != nil {
			return nil, err
		}
		return EncodeValueLeaf(x), nil
	}
	return nil, fmt.Errorf("%s: %T is not a value", where, v)
}

// compileValueMapPositionV1 compiles a map-typed position (a payload, a
// body): it must be an object literal, which encodes to a JSON object.
func compileValueMapPositionV1(where string, v any) (map[string]any, error) {
	if n, isNode := v.(ast.ExpressionNode); isNode {
		if _, isMap := ast.Unparen(n).(*ast.MapExpr); !isMap {
			return nil, fmt.Errorf("%s: must be an object literal (`{k: v}`), got %s", where, describeV1Value(v))
		}
	}
	cv, err := compileValueV1(where, v)
	if err != nil {
		return nil, err
	}
	obj, ok := cv.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: must be an object literal (`{k: v}`), got %s", where, describeV1Value(v))
	}
	return obj, nil
}

// v1LiteralValue requires a literal: a value whose encoding carries no
// expression leaf.
func v1LiteralValue(where string, v any) (any, error) {
	cv, err := compileValueV1(where, v)
	if err != nil {
		return nil, err
	}
	if hasExprLeaf(cv) {
		return nil, fmt.Errorf("%s: must be a literal, got %s", where, describeV1Value(v))
	}
	return cv, nil
}

// hasExprLeaf reports whether an encoded value carries an expression leaf.
func hasExprLeaf(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		if _, ok := x[exprLeafKey]; ok && len(x) == 1 {
			return true
		}
		for _, el := range x {
			if hasExprLeaf(el) {
				return true
			}
		}
	case []any:
		for _, el := range x {
			if hasExprLeaf(el) {
				return true
			}
		}
	}
	return false
}

// v1LiteralString requires a string literal and returns its value.
func v1LiteralString(where string, v any) (string, error) {
	lit, err := v1LiteralValue(where, v)
	if err != nil {
		return "", err
	}
	s, ok := lit.(string)
	if !ok {
		return "", fmt.Errorf("%s: must be a string literal, got %T", where, lit)
	}
	return s, nil
}

// compileLeafFieldV1 compiles a string-typed VALUE field -- an event topic,
// a webhook url or header value, a mutation id / parent / aliasOf -- as a
// value leaf (EncodeValueLeaf): a literal is plain JSON, an expression is
// `{"$expr": ...}`. The field holds one value, so a map or list literal is
// refused here rather than at load.
func compileLeafFieldV1(where string, v any) (any, error) {
	cv, err := compileValueV1(where, v)
	if err != nil {
		return nil, err
	}
	switch x := cv.(type) {
	case map[string]any:
		if _, isLeaf := x[exprLeafKey]; isLeaf && len(x) == 1 {
			return x, nil
		}
		return nil, fmt.Errorf("%s: must be a single value or an expression, got an object literal", where)
	case []any:
		return nil, fmt.Errorf("%s: must be a single value or an expression, got a list literal", where)
	}
	return cv, nil
}

// v1TextSource compiles a string-typed expression position to v1 source: a Go
// scalar is written as a v1 literal (quoted, for a string), a node as its
// canonical source.
func v1TextSource(where string, v any) (string, error) {
	switch x := v.(type) {
	case string:
		return ast.QuoteString(x), nil
	case bool, float64, int, int64:
		return ast.FormatLiteral(x), nil
	case ast.ExpressionNode:
		return v1Source(where, x)
	}
	return "", fmt.Errorf("%s: must be an expression, got %s", where, describeV1Value(v))
}

// v1ValueNode turns a positional argument into a node: a node as it is, a Go
// scalar as a literal node.
func v1ValueNode(where string, v any) (ast.ExpressionNode, error) {
	switch x := v.(type) {
	case nil:
		return &ast.NilExpr{}, nil
	case string, bool, float64, int64:
		return &ast.LiteralExpr{Value: x}, nil
	case int:
		return &ast.LiteralExpr{Value: int64(x)}, nil
	case ast.ExpressionNode:
		if err := requireV1Node(where, x); err != nil {
			return nil, err
		}
		return x, nil
	}
	return nil, fmt.Errorf("%s: %s cannot be an argument", where, describeV1Value(v))
}

// v1Source prints an edition-2026 node, refusing a node of the internal query
// form: that is a parser node a v1 body cannot contain, and printing it would
// write text the runtime cannot read back.
func v1Source(where string, n ast.ExpressionNode) (string, error) {
	if n == nil {
		return "", fmt.Errorf("%s: missing expression", where)
	}
	if err := requireV1Node(where, n); err != nil {
		return "", err
	}
	return ast.FormatExpr(n), nil
}

func requireV1Node(where string, n ast.ExpressionNode) error {
	var bad ast.ExpressionNode
	ast.WalkV1(n, func(x ast.ExpressionNode) bool {
		if bad != nil {
			return false
		}
		if ast.KindOf(x) == ast.KindUnknown {
			bad = x
			return false
		}
		return true
	})
	if bad != nil {
		return fmt.Errorf("%s: a %T node is not an edition-2026 expression (a v1 body holds v1 nodes only)", where, bad)
	}
	return nil
}

func describeV1Value(v any) string {
	if n, ok := v.(ast.ExpressionNode); ok && ast.KindOf(n) != ast.KindUnknown {
		return "`" + ast.FormatExpr(n) + "`"
	}
	return fmt.Sprintf("%T", v)
}

// ---------------------------------------------------------------------------
// step references, for the topological sort
// ---------------------------------------------------------------------------

// collectStepReferencesV1 returns the step ids a v1 step reads: the free
// names of its expressions that are step ids, and `steps.<id>` reads,
// including those of the steps it holds. Self-references are dropped.
func collectStepReferencesV1(step *parser.StepDef, allSteps map[string]struct{}) (map[string]struct{}, error) {
	refs := map[string]struct{}{}
	if err := stepRefsV1(step, nil, allSteps, refs); err != nil {
		return nil, err
	}
	delete(refs, step.ID)
	return refs, nil
}

func stepRefsV1(step *parser.StepDef, bound map[string]bool, allSteps map[string]struct{}, refs map[string]struct{}) error {
	// addText reads one always-expression position: the node the parser set
	// beside the text (StepDef.ConditionExpr and its siblings) when there is
	// one, else the text parsed here -- a StepDef built by hand carries only
	// the canonical source.
	addText := func(where, src string, node ast.ExpressionNode, b map[string]bool) error {
		if node == nil {
			if strings.TrimSpace(src) == "" {
				return nil
			}
			n, err := parser.ParseV1Expression(src)
			if err != nil {
				return fmt.Errorf("step %q %s: %w", step.ID, where, err)
			}
			node = n
		}
		v1StepRefs(node, b, allSteps, refs)
		return nil
	}
	if err := addText("condition", step.Condition, step.ConditionExpr, bound); err != nil {
		return err
	}
	switch cfg := step.Config.(type) {
	case *parser.QueryStepConfig:
		if cfg.Query != nil {
			v1StepRefs(cfg.Query, bound, allSteps, refs)
		}
	case *parser.FunctionStepConfig:
		for _, v := range cfg.Args {
			v1ValueRefs(v, bound, allSteps, refs)
		}
	case *parser.ActionStepConfig:
		for _, v := range cfg.Args {
			v1ValueRefs(v, bound, allSteps, refs)
		}
	case *parser.MutationStepConfig:
		if m := cfg.Mutation; m != nil {
			for _, v := range []any{m.IDTemplate, m.ParentTemplate, m.AliasOfTemplate} {
				v1ValueRefs(v, bound, allSteps, refs)
			}
			if m.PayloadExpr != nil {
				v1StepRefs(m.PayloadExpr, bound, allSteps, refs)
			}
		}
	case *parser.ForEachStepConfig:
		if err := addText("forEach source", cfg.Source, cfg.SourceExpr, bound); err != nil {
			return err
		}
		inner := make(map[string]bool, len(bound)+2)
		for k := range bound {
			inner[k] = true
		}
		as := cfg.As
		if as == "" {
			as = "item"
		}
		inner[as] = true
		inner["index"] = true
		if err := addText("forEach filter", cfg.Filter, cfg.FilterExpr, inner); err != nil {
			return err
		}
		for i := range cfg.Do {
			if err := stepRefsV1(&cfg.Do[i], inner, allSteps, refs); err != nil {
				return err
			}
		}
	case *parser.ParallelStepConfig:
		for i := range cfg.Branches {
			if err := stepRefsV1(&cfg.Branches[i], bound, allSteps, refs); err != nil {
				return err
			}
		}
	case *parser.SwitchStepConfig:
		if err := addText("switch expression", cfg.Expression, cfg.ExpressionExpr, bound); err != nil {
			return err
		}
		for _, cs := range cfg.Cases {
			if cs == nil {
				continue
			}
			for i := range cs.Steps {
				if err := stepRefsV1(&cs.Steps[i], bound, allSteps, refs); err != nil {
					return err
				}
			}
		}
		if cfg.Default != nil {
			for i := range cfg.Default.Steps {
				if err := stepRefsV1(&cfg.Default.Steps[i], bound, allSteps, refs); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// v1ValueRefs collects the step references of a value: nodes are walked,
// containers recursed, Go scalars are literals and reference nothing.
func v1ValueRefs(v any, bound map[string]bool, allSteps map[string]struct{}, refs map[string]struct{}) {
	switch x := v.(type) {
	case map[string]any:
		for _, el := range x {
			v1ValueRefs(el, bound, allSteps, refs)
		}
	case []any:
		for _, el := range x {
			v1ValueRefs(el, bound, allSteps, refs)
		}
	case ast.ExpressionNode:
		v1StepRefs(x, bound, allSteps, refs)
	}
}

// v1StepRefs adds the step ids n reads: a free IdentExpr naming a step, and
// the field of `steps.<id>`.
func v1StepRefs(n ast.ExpressionNode, bound map[string]bool, allSteps map[string]struct{}, refs map[string]struct{}) {
	switch e := n.(type) {
	case nil:
	case *ast.IdentExpr:
		if _, isStep := allSteps[e.Name]; isStep && !bound[e.Name] {
			refs[e.Name] = struct{}{}
		}
	case *ast.MemberExpr:
		if root, ok := e.Object.(*ast.IdentExpr); ok && root.Name == "steps" && !bound["steps"] {
			if _, isStep := allSteps[e.Field]; isStep {
				refs[e.Field] = struct{}{}
			}
			return
		}
		v1StepRefs(e.Object, bound, allSteps, refs)
	case *ast.LambdaExpr:
		inner := make(map[string]bool, len(bound)+len(e.Params))
		for k := range bound {
			inner[k] = true
		}
		for _, p := range e.Params {
			inner[p] = true
		}
		v1StepRefs(e.Body, inner, allSteps, refs)
	case *ast.CallExpr:
		v1StepRefs(e.Receiver, bound, allSteps, refs)
		for _, a := range e.Args {
			v1StepRefs(a, bound, allSteps, refs)
		}
		for _, a := range e.Named {
			v1StepRefs(a.Value, bound, allSteps, refs)
		}
	case *ast.UnaryExpr:
		v1StepRefs(e.Operand, bound, allSteps, refs)
	case *ast.BinaryExpr:
		v1StepRefs(e.Left, bound, allSteps, refs)
		v1StepRefs(e.Right, bound, allSteps, refs)
	case *ast.ListExpr:
		for _, el := range e.Elems {
			v1StepRefs(el, bound, allSteps, refs)
		}
	case *ast.MapExpr:
		for _, en := range e.Entries {
			v1StepRefs(en.Value, bound, allSteps, refs)
		}
	case *ast.ParenExpr:
		v1StepRefs(e.Inner, bound, allSteps, refs)
	case *ast.TernaryExpr:
		v1StepRefs(e.Condition, bound, allSteps, refs)
		v1StepRefs(e.Then, bound, allSteps, refs)
		v1StepRefs(e.Else, bound, allSteps, refs)
	}
}

// v1ReturnSource prints a v1 body's `return` expression.
func v1ReturnSource(step *parser.StepDef) (string, error) {
	cfg, ok := step.Config.(*parser.QueryStepConfig)
	if !ok {
		return "", fmt.Errorf("return: expected an expression, got %T", step.Config)
	}
	return v1Source("return", cfg.Query)
}
