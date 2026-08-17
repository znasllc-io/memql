package memql

// Resolving flat-kind references AT LOAD TIME (memql#3897).
//
// # Why this pass has to exist at all
//
// A construct reference inside a compiled body -- a spec conjunct, a logic call,
// a builtin call, a named shape -- is looked up at EXECUTION time, from a
// context that has no idea which file it came from:
//
//	validator := newFunctionValidator(fns.LookupIndex(), nil)   // engine.go
//	fn, ok := v.functions[key]                                  // function_validator.go
//
// The only "origin" that validator carries is auth.CallOrigin (client vs
// internal). There is no moment during execution at which "which namespace is
// this reference in" can be asked, so namespacing the registry without moving
// resolution earlier would leave every reference to be guessed.
//
// This pass moves it earlier: after everything is registered, each construct's
// body is walked and every reference rewritten to its `<namespace>.<name>` key.
// The runtime lookup is then context-free -- exactly as baking canonical concept
// ids in at load already makes concept lookups context-free.
//
// # It runs after registration, not during, and that is not an implementation
// detail
//
// Load order across namespaces is not defined: a query in `cognition` may call a
// builtin in `common` that has not been loaded yet. Resolving during load would
// make the answer depend on file order, which is the class of defect
// memql#2360's uniqueness gate was written to kill. So it is a second pass over
// a complete registry, like resolveRelationshipTargets is for concepts.
//
// # What it deliberately does NOT do: fail
//
// An unresolvable reference is LEFT AS WRITTEN. Not because a silent failure is
// acceptable, but because this pass is not the gate for it: the registry's own
// bare-name resolution is the floor underneath (an unambiguous name resolves at
// execution exactly as it always did), the cross-namespace-import gate
// (memql#3803) is what refuses an undeclared dependency, and the validator
// reports a genuinely missing construct with a better message than this pass
// could. Rewriting only what resolves means the worst case of a reference shape
// this walker does not reach is TODAY'S BEHAVIOUR, not a construct that stops
// working -- which is the property that makes a change this broad safe to land
// over a 1097-construct corpus.

import (
	"strings"

	languageAst "github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/memql/baseloader"
	"github.com/znasllc-io/memql/core/dslfs"
)

// constructScopeIndex maps a file path to the scope its constructs resolve in.
type constructScopeIndex map[string]ConstructScope

// buildConstructScopeIndex parses every file's `use` block once.
//
// PER FILE, not per construct: the imports are a property of the file, and a
// construct sliced out of it inherits them. Parsing per construct would re-parse
// the same header once per declaration -- 537 times for the function registry
// alone.
func buildConstructScopeIndex(files []baseloader.RawFile) constructScopeIndex {
	idx := make(constructScopeIndex, len(files))
	for _, f := range files {
		uses, err := parsedUseDeclarations(f.Content)
		if err != nil {
			// A file whose import header does not parse still has a namespace,
			// and its constructs still resolve within it. The parse error is
			// reported by the loaders, which is where it belongs.
			uses = nil
		}
		idx[f.Path] = NewConstructScope(f.Path, uses)
	}
	return idx
}

// scopeFor returns the scope for a construct origin.
//
// Origins arrive DECORATED -- the unified loader stamps slice origins as
// `unified:<path>:<sliceName>` -- so the path is recovered before lookup. A file
// the index does not know still yields a usable scope from its namespace alone,
// which is the right degrade: same-namespace references keep resolving, and only
// imported ones fall through to the registry's bare-name floor.
func (idx constructScopeIndex) scopeFor(origin string) ConstructScope {
	path := undecorateOrigin(origin)
	if scope, ok := idx[path]; ok {
		return scope
	}
	return ConstructScope{Namespace: dslfs.NamespaceFromFilePath(origin)}
}

// undecorateOrigin strips a loader decoration down to the mounted file path.
//
//	"unified:agents/traits.memql:agentKindSystem" -> "agents/traits.memql"
//	"cognition/queries.memql"                     -> unchanged
func undecorateOrigin(origin string) string {
	s := strings.TrimSpace(origin)
	if i := strings.Index(s, ":"); i >= 0 && !strings.Contains(s[:i], "/") {
		s = s[i+1:]
	}
	if i := strings.LastIndex(s, ".memql:"); i >= 0 {
		s = s[:i+len(".memql")]
	}
	return s
}

// referenceRewriter rewrites one construct body's flat-kind references.
//
// Each kind consults its OWN registry for existence, because the twelve kinds
// live in eight of them and a name that resolves as a spec is not evidence that
// it resolves as a shape.
type referenceRewriter struct {
	scope    ConstructScope
	callable func(key string) bool // functions: query / mutation / logic / builtin
	spec     func(key string) bool
	shape    func(key string) bool
	prompt   func(key string) bool
	rewrites int
}

// resolve rewrites one reference, or returns it unchanged.
func (r *referenceRewriter) resolve(name string, exists func(string) bool) string {
	if exists == nil || strings.TrimSpace(name) == "" {
		return name
	}
	key, ok := r.scope.Resolve(name, exists)
	if !ok || key == name {
		return name
	}
	r.rewrites++
	return key
}

// rewriteExpression walks a compiled body IN PLACE.
//
// In place rather than returning a copy, because the registries hand out clones
// on egress and a copy would be discarded. The five node types below are the
// only ones carrying a flat-kind reference; the rest are traversed for their
// children. Modelled on cloneExpressionNode, which is the tree's authoritative
// node inventory -- so a new node type added there without a case here is a
// reference this pass silently does not reach, which is why the failure mode
// being "today's behaviour" rather than "broken" is load-bearing.
func (r *referenceRewriter) rewriteExpression(expr ExpressionNode) {
	switch node := expr.(type) {
	case nil:
		return
	case *LogicalExpression:
		r.rewriteExpression(node.Left)
		r.rewriteExpression(node.Right)
	case *ArithmeticExpression:
		r.rewriteExpression(node.Left)
		r.rewriteExpression(node.Right)
	case *BinaryComparisonExpression:
		r.rewriteExpression(node.Left)
		r.rewriteExpression(node.Right)
	case *RelationshipExpression:
		r.rewriteExpression(node.Target)
	case *ConditionalFilterExpression:
		r.rewriteExpression(node.Filter)

	// --- the reference-bearing nodes ---
	case *SpecReferenceExpression:
		node.Name = r.resolve(node.Name, r.spec)
	case *FunctionCallExpression:
		node.Name = r.resolve(node.Name, r.callable)
	case *BuiltinFunctionExpression:
		node.Name = r.resolve(node.Name, r.callable)
	case *ShapeExpression:
		node.TemplateName = r.resolve(node.TemplateName, r.shape)
		r.rewriteExpression(node.Target)
	case *AIExpression:
		if node.Invocation != nil {
			node.Invocation.TemplateId = r.resolve(node.Invocation.TemplateId, r.prompt)
		}

	// --- traversed for their target ---
	case *SortExpression:
		r.rewriteExpression(node.Target)
	case *PaginateExpression:
		r.rewriteExpression(node.Target)
	case *SelectExpression:
		r.rewriteExpression(node.Target)
	case *TimestampExpression:
		r.rewriteExpression(node.Target)
	case *DepthExpression:
		r.rewriteExpression(node.Target)
	}
}

// resolveConstructReferences is the pass. Returns how many references it
// rewrote, for the boot log.
//
// ZERO IS THE EXPECTED ANSWER ON A TREE WITH NO COLLISIONS, and that is worth
// knowing rather than alarming: every reference already resolves through the
// registry's bare-name floor, so a rewrite only changes anything once two
// namespaces declare the name -- which is the state memql#3897 exists to make
// possible and which no tree has until a product bundle arrives.
func resolveConstructReferences(
	functions *FunctionRegistry,
	specs *SpecRegistry,
	shapes *ShapeRegistry,
	prompts *PromptRegistry,
	files []baseloader.RawFile,
) int {
	if functions == nil {
		return 0
	}
	idx := buildConstructScopeIndex(files)

	callable := func(key string) bool { return functions.Has(key) }
	specExists := func(key string) bool { return specs != nil && specs.Has(key) }
	shapeExists := func(key string) bool {
		if shapes == nil {
			return false
		}
		_, ok := shapes.Get(key)
		return ok
	}
	promptExists := func(key string) bool {
		if prompts == nil {
			return false
		}
		_, ok := prompts.Get(key)
		return ok
	}

	total := 0
	for _, fn := range functions.Snapshot() {
		if fn == nil {
			continue
		}
		rw := &referenceRewriter{
			scope:    idx.scopeFor(fn.Origin),
			callable: callable,
			spec:     specExists,
			shape:    shapeExists,
			prompt:   promptExists,
		}
		rw.rewriteExpression(fn.Expr)
		if rw.rewrites > 0 {
			total += rw.rewrites
			// Written back through the KIND'S OWN wrapper, which derives the
			// key from the construct's Origin. Passing the map key instead
			// would work today and would silently write to the wrong place the
			// first time a construct is reached under its bare alias.
			_ = functions.Upsert(fn)
		}
	}
	if specs != nil {
		for _, spec := range specs.Snapshot() {
			if spec == nil {
				continue
			}
			rw := &referenceRewriter{
				scope:    idx.scopeFor(spec.Origin),
				callable: callable,
				spec:     specExists,
				shape:    shapeExists,
				prompt:   promptExists,
			}
			rw.rewriteExpression(spec.Expr)
			if rw.rewrites > 0 {
				total += rw.rewrites
				_ = specs.Upsert(QualifyConstruct(ConstructNamespaceForOrigin(spec.Origin), spec.Name), spec)
			}
		}
	}
	return total
}

// constructScopeForOrigin is the sandbox's entry point to the same rule.
//
// BOOT AND THE AUTHORING SANDBOX MUST AGREE, and memql#3800 is why that is
// stated rather than assumed: two paths implementing one resolution rule
// diverged, and 45 constructs compiled at boot while every editor refused them.
// The sandbox reaches the rule through this function rather than reimplementing
// it, so there is one implementation to be right.
func constructScopeForOrigin(origin string, uses []*languageAst.UseDeclaration) ConstructScope {
	return NewConstructScope(undecorateOrigin(origin), uses)
}
