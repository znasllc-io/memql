package memql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/config"
	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/tiers"
)

// mutation_values_v1.go -- a mutation's VALUES in edition 2026 (epic
// memql#5363, memql#5367).
//
// A mutation's insert/update block, and its id= / createdAt= / parent= /
// aliasOf= slots, hold parsed v1 expression nodes, and ONE evaluator renders
// them: EvalExpr, over a scope that binds `args` (the call's arguments),
// `actor` (the envelope, read one field at a time so that `actor.userId` can
// refuse, memql#3620) and `config` (the allow-listed configuration), with
// `now` the renderer's one clock. Nothing here reads expression TEXT. That is
// the point of the file: the string half of mutation_templates.go decided
// what a value meant by looking at its spelling, so a quoted "123" came back
// an int64, a quoted "args.x" came back the caller's argument, and a
// mistyped reference was stored as its own name; a parsed literal is a
// literal and a parsed reference is a reference.
//
// What carries over unchanged, because the v1 table keeps it: a missing
// argument omits its key or element and an explicit nil is kept (EvalExpr's
// container rule, memql#3627); `??` is blank-coalescing through the one
// selection rule, coalesceSelect; hash() of a missing value digests "" and so
// stays 64 characters wide (memql#3009); shortId, canonicalId, var and the
// secret readers reach the same engine resolvers the string half reached.
//
// TRANSITION. Every template the tree loads today is still the legacy kind,
// rendered by the string half. A template reaches this file only when
// newMutationTemplateV1 built it, which marks it ValuesV1; the parser option
// that makes the loader build one for every mutation lands separately, and
// the flip then deletes the string half and the dispatch in
// renderMutationTemplate.

// mutationValueCheck is the load-time check every mutation value passes
// (expr_inprocess_check.go): the position's node kinds and functions, and the
// three roots its scope binds.
var mutationValueCheck = &inProcessExprCheck{
	where:     "a mutation value",
	position:  tiers.PositionMutationValue,
	roots:     map[string]bool{"args": true, "actor": true, "config": true},
	fieldOnly: map[string]bool{"actor": true},
	fields: map[string]func(string) error{
		"actor": func(field string) error {
			if _, ok := auth.ActorEnvelopeCanonicalName(field); !ok {
				return fmt.Errorf("actor.%s is not a field of the actor envelope (valid: %s)", field, auth.ActorEnvelopeValidNames())
			}
			return nil
		},
		"config": func(field string) error {
			// Exact, not config.FieldByKey's case-insensitive match: the map
			// the scope binds is keyed by the exact spelling, so a key in the
			// wrong case would pass here and read as absent on every call.
			for _, f := range config.PolicyExposableConfig {
				if f.Key == field {
					return nil
				}
			}
			return fmt.Errorf("config.%s is not an allow-listed configuration key (component/config/policy_exposable.go)", field)
		},
	},
}

// mutationSlotsV1 carries a mutation's call-level slots -- its id=,
// createdAt=, parent= and aliasOf= values -- each nil when not written.
type mutationSlotsV1 struct {
	ID, CreatedAt, Parent, AliasOf ast.ExpressionNode
}

// newMutationTemplateV1 is the one builder of an edition-2026 mutation
// template: kind and concept as the statement names them, block the
// insert/update block as a map literal (nil for none), and slots the
// call-level values.
//
// It checks every value at LOAD (mutationValueCheck), so a value that cannot
// render is refused where the tree is read rather than on its first call. And
// it lays the block out exactly as the loader always has (function_loader.go),
// hoist included -- an `id` or `createdAt` key becomes that slot, a `payload`
// key becomes the whole-object splat with the remaining keys as its overlay
// (memql#401) -- so every gate that reads the layout rather than the text
// (validateMutationCallerArgs, destructiveNestedObjectFields,
// OwnerFieldProvenance) sees the shape it has always seen, with parsed nodes
// at the leaves. Two things the loader tolerated are refused instead: a
// value written twice (the block's `id` beside an id= slot, where the loader
// kept the slot and dropped the key with no signal), and a splat that is a
// literal, which can never render as an object.
//
// The annotation-driven fields (MergeFields, AppendFields, ...) are the
// caller's to set on the result, exactly as the loader sets them.
func newMutationTemplateV1(kind ast.MutationKind, concept string, block ast.ExpressionNode, slots mutationSlotsV1) (*FunctionMutationTemplate, error) {
	tmpl := &FunctionMutationTemplate{Kind: kind, Concept: strings.TrimSpace(concept), ValuesV1: true}

	for _, slot := range []struct {
		name string
		node ast.ExpressionNode
	}{{"id", slots.ID}, {"createdAt", slots.CreatedAt}, {"parent", slots.Parent}, {"aliasOf", slots.AliasOf}} {
		if slot.node == nil {
			continue
		}
		if err := mutationValueCheck.check(slot.node); err != nil {
			return nil, fmt.Errorf("%s=: %w", slot.name, err)
		}
	}
	tmpl.IDTemplate = mutationSlotTemplate(slots.ID)
	tmpl.CreatedAtTemplate = mutationSlotTemplate(slots.CreatedAt)
	tmpl.ParentTemplate = mutationSlotTemplate(slots.Parent)
	tmpl.AliasOfTemplate = mutationSlotTemplate(slots.AliasOf)

	if block == nil {
		tmpl.PayloadTemplate = map[string]any{}
		return tmpl, nil
	}
	m, ok := block.(*ast.MapExpr)
	if !ok || m == nil {
		return nil, fmt.Errorf("the insert/update block must be a map literal {field: value, ...}, got `%s`", ast.FormatExpr(block))
	}

	var (
		fields      []ast.MapEntry
		splat       ast.ExpressionNode
		hasSplatKey bool
		seen        = map[string]bool{}
	)
	for _, en := range m.Entries {
		if seen[en.Key] {
			return nil, fmt.Errorf("field %q is written twice in the block: the first value would be discarded with no signal", en.Key)
		}
		seen[en.Key] = true
		if err := mutationValueCheck.check(en.Value); err != nil {
			return nil, fmt.Errorf("field %q: %w", en.Key, err)
		}
		switch en.Key {
		case "id":
			if slots.ID != nil {
				return nil, fmt.Errorf("the id is written twice, as the id= slot and as the block's `id` key: write it once")
			}
			tmpl.IDTemplate = en.Value
		case "createdAt":
			if slots.CreatedAt != nil {
				return nil, fmt.Errorf("createdAt is written twice, as the createdAt= slot and as the block's `createdAt` key: write it once")
			}
			tmpl.CreatedAtTemplate = en.Value
		case "payload":
			hasSplatKey = true
			splat = en.Value
		default:
			fields = append(fields, en)
		}
	}

	layout := mutationLayoutV1(fields)
	if !hasSplatKey {
		tmpl.PayloadTemplate = layout
		return tmpl, nil
	}
	switch s := ast.Unparen(splat).(type) {
	case *ast.MapExpr:
		// `payload: {a: 1}` is the field map itself, as the loader read it.
		tmpl.PayloadTemplate = mutationLayoutV1(s.Entries)
	case *ast.LiteralExpr, *ast.NilExpr, *ast.ListExpr:
		return nil, fmt.Errorf("payload: `%s` can never be an object: the payload splat is an object, e.g. payload: args.payload", ast.FormatExpr(splat))
	default:
		tmpl.PayloadTemplate = splat
	}
	if len(layout) > 0 {
		// The block mixed the splat with explicit fields: they overlay it,
		// and win on a collision, so a server-side stamp cannot be displaced
		// by the caller's object (memql#401).
		tmpl.PayloadOverlayTemplate = layout
	}
	return tmpl, nil
}

// mutationSlotTemplate stores a slot node in the template's `any` field,
// keeping an absent slot a nil interface rather than a typed nil.
func mutationSlotTemplate(n ast.ExpressionNode) any {
	if n == nil {
		return nil
	}
	return n
}

// mutationLayoutV1 lays block entries out as the loader's layout: an object
// literal becomes a nested map, a list literal a []any, recursively, and
// every other value -- a reference, a call, an operator, a literal -- is the
// node itself. Only literal containers are laid out: `args.tags ?? []` is one
// node, whose list is an operand rather than a container of the block.
func mutationLayoutV1(entries []ast.MapEntry) map[string]any {
	out := make(map[string]any, len(entries))
	for _, en := range entries {
		out[en.Key] = mutationLayoutValueV1(en.Value)
	}
	return out
}

func mutationLayoutValueV1(n ast.ExpressionNode) any {
	switch e := n.(type) {
	case *ast.MapExpr:
		return mutationLayoutV1(e.Entries)
	case *ast.ListExpr:
		out := make([]any, len(e.Elems))
		for i, el := range e.Elems {
			out[i] = mutationLayoutValueV1(el)
		}
		return out
	}
	return n
}

// renderMutationTemplateV1 renders an edition-2026 template. The order of the
// steps, and every error's wording, follow renderMutationTemplate's, so a
// caller cannot tell which renderer refused a call except by what the
// refusal is about.
func (e *MemQLEngine) renderMutationTemplateV1(ctx context.Context, tmpl *FunctionMutationTemplate, concept string, args map[string]any) (MutationNode, error) {
	r := e.newMutationRendererV1(ctx, args)

	id, err := r.slotText(tmpl.IDTemplate)
	if err != nil {
		return MutationNode{}, fmt.Errorf("evaluate id: %w", err)
	}

	createdAtRaw, err := r.slotText(tmpl.CreatedAtTemplate)
	if err != nil {
		return MutationNode{}, fmt.Errorf("evaluate createdAt: %w", err)
	}
	var createdAtRef *time.Time
	if strings.TrimSpace(createdAtRaw) != "" {
		ts, err := parseRFC3339Timestamp(createdAtRaw)
		if err != nil {
			return MutationNode{}, fmt.Errorf("createdAt must be RFC3339/RFC3339Nano: %w", err)
		}
		createdAtRef = &ts
	}

	payloadMap, err := r.payload(tmpl.PayloadTemplate)
	if err != nil {
		return MutationNode{}, fmt.Errorf("evaluate payload: %w", err)
	}
	for _, k := range sortedAnyKeys(tmpl.PayloadOverlayTemplate) {
		v, absent, err := r.layoutValue(tmpl.PayloadOverlayTemplate[k])
		if err != nil {
			return MutationNode{}, fmt.Errorf("evaluate payload overlay field %q: %w", k, err)
		}
		if absent {
			// The container rule, applied to the overlay as to every other
			// container: a missing argument contributes nothing, so the
			// splat's own value for the key stands. The string half wrote
			// its missing sentinel here, which marshals as `{}` -- an object
			// nobody sent.
			continue
		}
		payloadMap[k] = v
	}

	// Insert-time canonicalisation of @relationship fields, exactly as the
	// string half does (see renderMutationTemplate for why it is insert-time).
	// It rewrites fields in place, a dotted relationship field inside a
	// nested object included, and a value read from args is the caller's own
	// map: canonicalise a deep copy, so a call never edits its arguments.
	payloadMap = cloneMapStringAny(payloadMap)
	if err := e.canonicalizeRelationshipFields(ctx, concept, payloadMap); err != nil {
		return MutationNode{}, fmt.Errorf("canonicalize relationship fields: %w", err)
	}

	payloadJSON, err := json.Marshal(payloadMap)
	if err != nil {
		return MutationNode{}, fmt.Errorf("marshal payload: %w", err)
	}

	parent, err := r.slotText(tmpl.ParentTemplate)
	if err != nil {
		return MutationNode{}, fmt.Errorf("evaluate parent: %w", err)
	}
	aliasOf, err := r.slotText(tmpl.AliasOfTemplate)
	if err != nil {
		return MutationNode{}, fmt.Errorf("evaluate aliasOf: %w", err)
	}

	return templateMutationNode(tmpl, concept, id, payloadJSON, createdAtRef, parent, aliasOf), nil
}

// mutationRendererV1 renders one call's values: one scope and one clock for
// every value of the call, so two fields reading `now` read one instant, as
// the string half's single e.now did.
type mutationRendererV1 struct {
	ctx   context.Context
	scope *mutationScopeV1
	opts  EvalOptions
}

func (e *MemQLEngine) newMutationRendererV1(ctx context.Context, args map[string]any) *mutationRendererV1 {
	if ctx == nil {
		ctx = context.Background()
	}
	return &mutationRendererV1{
		ctx:   ctx,
		scope: &mutationScopeV1{ctx: ctx, engine: e, args: args},
		opts: EvalOptions{
			Now:  time.Now().UTC(),
			Vars: e.resolveExprVariable,
			CanonicalID: func(ctx context.Context, value any, concept string) (string, error) {
				return e.canonicalizeIdValue(ctx, exprText(value), concept)
			},
		},
	}
}

// resolveExprVariable backs var / systemVar / secret / systemSecret: the same
// four engine resolvers the string half called, chosen by the function's name.
func (e *MemQLEngine) resolveExprVariable(ctx context.Context, kind, name string) (string, error) {
	switch kind {
	case "var":
		return e.ResolveVariable(ctx, name)
	case "systemVar":
		return e.ResolveSystemVariable(ctx, name)
	case "secret":
		return e.ResolveSecret(ctx, name)
	case "systemSecret":
		return e.ResolveSystemSecret(ctx, name)
	}
	return "", fmt.Errorf("%s(): not a variable reader", kind)
}

// eval evaluates one node of the template: a walk over the parsed tree
// through EvalExpr, over the call's scope. No source text is compiled or
// executed here or anywhere below it.
func (r *mutationRendererV1) eval(n ast.ExpressionNode) (any, error) {
	return EvalExpr(r.ctx, n, r.scope, r.opts)
}

// slotText renders an id / createdAt / parent / aliasOf slot as text: nil
// (the slot was not written), an absent value and nil are "", a string is
// itself, and a number or bool is its canonical text. A list or a map is
// refused -- a row id is text, and the string half's "%v" of one was Go's
// own map syntax, which no reader of an id could have meant.
func (r *mutationRendererV1) slotText(t any) (string, error) {
	if t == nil {
		return "", nil
	}
	n, ok := t.(ast.ExpressionNode)
	if !ok {
		return "", fmt.Errorf("a v1 template slot holds a %T, not an expression node", t)
	}
	v, err := r.eval(n)
	if err != nil {
		return "", err
	}
	nv, err := exprNormalize(v)
	if err != nil {
		return "", err
	}
	switch x := nv.(type) {
	case nil, absentValue:
		return "", nil
	case string:
		return x, nil
	case bool, int64, float64:
		return exprText(x), nil
	}
	return "", fmt.Errorf("`%s` is %s; this slot takes text", ast.FormatExpr(n), exprTypeName(nv))
}

// payload renders PayloadTemplate: the block's field layout, or a splat that
// must evaluate to an object. The object returned is always a fresh map, so
// the overlay's writes cannot land in a map the caller passed in its args.
func (r *mutationRendererV1) payload(t any) (map[string]any, error) {
	switch p := t.(type) {
	case map[string]any:
		return r.layoutMap(p)
	case ast.ExpressionNode:
		v, err := r.eval(p)
		if err != nil {
			return nil, err
		}
		nv, err := exprNormalize(v)
		if err != nil {
			return nil, err
		}
		obj, ok := nv.(map[string]any)
		if !ok || obj == nil {
			return nil, fmt.Errorf("payload must evaluate to an object; `%s` is %s", ast.FormatExpr(p), exprTypeName(nv))
		}
		return cloneMapStringAny(obj), nil
	}
	return nil, fmt.Errorf("payload must evaluate to an object")
}

// layoutMap renders a laid-out object under the container rule: a value that
// evaluates to Absent omits its key, an explicit nil is kept. Keys are
// rendered in sorted order, so when two fields would both refuse, the same
// one is reported on every call.
func (r *mutationRendererV1) layoutMap(m map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(m))
	for _, k := range sortedAnyKeys(m) {
		v, absent, err := r.layoutValue(m[k])
		if err != nil {
			return nil, fmt.Errorf("evaluate %q: %w", k, err)
		}
		if absent {
			continue
		}
		out[k] = v
	}
	return out, nil
}

// layoutValue renders one laid-out value and reports whether it is absent.
func (r *mutationRendererV1) layoutValue(t any) (any, bool, error) {
	switch v := t.(type) {
	case map[string]any:
		out, err := r.layoutMap(v)
		return out, false, err
	case []any:
		out := make([]any, 0, len(v))
		for i, el := range v {
			ev, absent, err := r.layoutValue(el)
			if err != nil {
				return nil, false, fmt.Errorf("evaluate [%d]: %w", i, err)
			}
			if absent {
				continue
			}
			out = append(out, ev)
		}
		return out, false, nil
	case ast.ExpressionNode:
		ev, err := r.eval(v)
		if err != nil {
			return nil, false, err
		}
		if _, absent := ev.(absentValue); absent {
			return nil, true, nil
		}
		return ev, false, nil
	}
	return nil, false, fmt.Errorf("a v1 template value holds a %T, not an expression node", t)
}

// mutationScopeV1 is the scope a mutation value reads. `now` is not bound
// here -- EvalExpr reads it from EvalOptions.Now, the renderer's one clock --
// and no other name is: an unknown name is unknown_name (and refused at load
// by mutationValueCheck), where the string half wrote it out as a literal.
type mutationScopeV1 struct {
	ctx    context.Context
	engine *MemQLEngine
	args   map[string]any

	config    map[string]any
	configSet bool
}

// Lookup binds the three roots.
func (s *mutationScopeV1) Lookup(name string) (any, bool) {
	switch name {
	case "args":
		return s.args, true
	case "actor":
		return actorEnvelopeV1{ctx: s.ctx}, true
	case "config":
		if !s.configSet {
			// The one config surface every DSL body reads (#2623): the
			// ambient envelope's allow-listed map, built on first read.
			s.config, _ = buildAmbientEnvelope(s.ctx, s.engine)["config"].(map[string]any)
			s.configSet = true
		}
		return s.config, true
	}
	return nil, false
}

// actorEnvelopeV1 is `actor` in a mutation value: the auth envelope, read
// one field at a time through resolveActorReference -- the ONE binding of an
// actor path into a write (memql#2840), which refuses `actor.userId` against
// an envelope that names nobody (memql#3620) and refuses a path the envelope
// does not have. Being an ExprMembers, it is asked only for the fields an
// evaluation actually reads.
type actorEnvelopeV1 struct {
	ctx context.Context
}

// ExprMember binds one envelope field.
func (a actorEnvelopeV1) ExprMember(field string) (any, error) {
	return resolveActorReference(a.ctx, "actor."+field)
}

// errActorEnvelopeWhole is the envelope's answer to being read whole.
var errActorEnvelopeWhole = errors.New("the actor envelope has no whole value: read one field at a time, as in actor.userId")

// MarshalJSON refuses: the envelope is not a value of the language, and a
// payload that captured it whole must fail to render rather than store `{}`.
func (actorEnvelopeV1) MarshalJSON() ([]byte, error) { return nil, errActorEnvelopeWhole }

// ---------------------------------------------------------------------------
// the layout gates' reading of a v1 leaf
// ---------------------------------------------------------------------------
//
// Three load-time gates read a template's LAYOUT rather than its source --
// validateMutationCallerArgs (C5, memql#2035), destructiveNestedObjectFields
// (memql#3617) and OwnerFieldProvenance (memql#2982). The layout is the same
// for a v1 template (newMutationTemplateV1 builds the loader's), but its
// leaves are parsed nodes where the string half's were text, and a gate that
// tests a leaf for the prefix "args." would find no text in a v1 leaf: the
// first two would pass every v1 mutation without looking, and the third would
// fail every one closed. Each gate asks the functions below first; a leaf
// they do not recognise as v1 (isV1 false) is read the way it always was.

// v1CallerArgPath reports whether n is exactly a read of a caller argument --
// `args.<path>`, through `.` or `.?` -- and returns the path. A value that
// merely CONTAINS one (`args.x ?? "d"`, `"p-" + args.x`) is not one: that is
// the leaf test the gates applied to text ("starts with args."), kept as it
// was rather than widened here.
func v1CallerArgPath(n ast.ExpressionNode) ([]string, bool) {
	var path []string
	for {
		switch e := n.(type) {
		case *ast.MemberExpr:
			if e == nil {
				return nil, false
			}
			path = append([]string{e.Field}, path...)
			n = e.Object
			continue
		case *ast.IdentExpr:
			if e != nil && e.Name == "args" && len(path) > 0 {
				return path, true
			}
		}
		return nil, false
	}
}

// v1LeafIsCallerArg is valueReferencesCallerArg's reading of a v1 leaf.
func v1LeafIsCallerArg(v any) (isArg, isV1 bool) {
	n, ok := v.(ast.ExpressionNode)
	if !ok || ast.KindOf(n) == ast.KindUnknown {
		return false, false
	}
	_, isArg = v1CallerArgPath(n)
	return isArg, true
}

// v1LeafBareArg is bareArgReference's reading of a v1 leaf: exactly
// `args.<name>`, one segment.
func v1LeafBareArg(v any) (name string, ok, isV1 bool) {
	n, isNode := v.(ast.ExpressionNode)
	if !isNode || ast.KindOf(n) == ast.KindUnknown {
		return "", false, false
	}
	path, isArg := v1CallerArgPath(n)
	if !isArg || len(path) != 1 {
		return "", false, true
	}
	return path[0], true, true
}

// classifyV1TemplateLeaf is OwnerFieldProvenance's reading of a v1 leaf,
// with the classifier's rules: a read of a caller argument anywhere in the
// value makes it caller-controllable (provAccept), `actor.userId` with no
// argument beside it is the stamp (provStamp), and a name it cannot place is
// provUnknown, which the gate treats as caller-controllable -- failing closed,
// as for the string half's unrecognised nodes.
func classifyV1TemplateLeaf(n ast.ExpressionNode) (valueProvenance, bool) {
	if ast.KindOf(n) == ast.KindUnknown {
		return provUnknown, false
	}
	return classifyV1Expr(n, nil), true
}

func classifyV1Expr(n ast.ExpressionNode, params []string) valueProvenance {
	fold := func(parts ...ast.ExpressionNode) valueProvenance {
		return foldProvenance(func(yield func(valueProvenance)) {
			for _, p := range parts {
				if p != nil {
					yield(classifyV1Expr(p, params))
				}
			}
		})
	}
	switch e := n.(type) {
	case *ast.LiteralExpr, *ast.NilExpr:
		return provNone
	case *ast.IdentExpr:
		switch {
		case exprCheckIsParam(e.Name, params):
			// A lambda's element: the collection it ranges over is
			// classified where the method's receiver is.
			return provNone
		case e.Name == "args":
			return provAccept
		case e.Name == "now" || e.Name == "config":
			return provNone
		}
		return provUnknown
	case *ast.MemberExpr:
		root, first := v1MemberRoot(e)
		if root != nil && !exprCheckIsParam(root.Name, params) && root.Name == "actor" {
			if first == "userId" {
				return provStamp
			}
			// Another envelope field: server-derived, not the owner.
			return provNone
		}
		return classifyV1Expr(e.Object, params)
	case *ast.CallExpr:
		return foldProvenance(func(yield func(valueProvenance)) {
			if e.Receiver != nil {
				yield(classifyV1Expr(e.Receiver, params))
			}
			for _, a := range e.Args {
				if lam, ok := ast.Unparen(a).(*ast.LambdaExpr); ok && lam != nil {
					yield(classifyV1Expr(lam.Body, append(append([]string(nil), params...), lam.Params...)))
					continue
				}
				yield(classifyV1Expr(a, params))
			}
			for _, a := range e.Named {
				yield(classifyV1Expr(a.Value, params))
			}
		})
	case *ast.UnaryExpr:
		return fold(e.Operand)
	case *ast.BinaryExpr:
		return fold(e.Left, e.Right)
	case *ast.TernaryExpr:
		return fold(e.Condition, e.Then, e.Else)
	case *ast.ListExpr:
		return fold(e.Elems...)
	case *ast.MapExpr:
		parts := make([]ast.ExpressionNode, 0, len(e.Entries))
		for _, en := range e.Entries {
			parts = append(parts, en.Value)
		}
		return fold(parts...)
	case *ast.ParenExpr:
		return fold(e.Inner)
	case *ast.LambdaExpr:
		return classifyV1Expr(e.Body, append(append([]string(nil), params...), e.Params...))
	}
	return provUnknown
}

// v1MemberRoot returns the root name of a member chain and the first field
// read from it (`actor` and "userId" for actor.userId.x), or nil when the
// chain does not start at a name.
func v1MemberRoot(e *ast.MemberExpr) (*ast.IdentExpr, string) {
	first := e.Field
	var n ast.ExpressionNode = e.Object
	for {
		switch o := n.(type) {
		case *ast.MemberExpr:
			first = o.Field
			n = o.Object
			continue
		case *ast.IdentExpr:
			return o, first
		}
		return nil, ""
	}
}

// sortedAnyKeys returns a map's keys in sorted order.
func sortedAnyKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
