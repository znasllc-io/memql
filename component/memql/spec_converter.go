package memql

// spec_converter.go bridges the langparser's *ast.SpecDecl AST node
// (introduced by memql#334 / sub-epic #329 / #310 Stage 1C) to the
// memql package's *Spec registry type. The hand-rolled spec parser it
// replaced was deleted with memql#5359, which made the annotation registry
// the one list of what a spec takes.
//
// Semantics mirror the retired hand-rolled parser one-for-one, except the
// annotation surface: which annotations a spec or trait takes is decided at parse time
// by the annotation registry (component/language/annotations, memql#5359),
// which also carries the migration hints for the retired @shape, @row,
// @actor and @use* forms. The converter reads what the legal ones mean:
//
//   * Annotations: @description (carries a value) and @enabled / @disabled
//     (author-surface lifecycle; @disabled skips registration).
//   * Body conversion: NewASTConverter().ConvertExpression on the
//     pre-parsed ast.ExpressionNode -> normalizeSpecCallsToReferences
//     -> ensureBooleanExpression -> classifySpecKind.
//   * Spec.ExprSource is set to canonicalExpression(engineExpr) --
//     the hand-rolled parser captured the raw author body; the canonical
//     form serves the only downstream consumer (Spec.clone) without
//     introducing a source-string capture pass on the langparser side.

import (
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// specDeclToSpec converts a langparser SpecDecl into the engine's
// *Spec registry type. Returns an error matching the retired parser's
// surface so the loader's diagnostic messages stay identical across
// the migration.
func specDeclToSpec(decl *languageParser.SpecDecl, origin string) (*Spec, error) {
	if decl == nil {
		return nil, fmt.Errorf("spec decl is nil")
	}
	if strings.TrimSpace(decl.Name) == "" {
		return nil, fmt.Errorf("%s: spec or trait name is required", origin)
	}

	kindLabel := "spec"
	if decl.IsTrait {
		kindLabel = "trait"
	}

	var description string
	var disabled bool
	for _, attr := range decl.Attributes {
		switch attr.Name {
		case "description":
			val, ok := attr.Value.(string)
			if !ok {
				return nil, fmt.Errorf("%s: @description expects a string value", origin)
			}
			description = val
		case ast.AttrEnabled:
			// Accepted no-op: enabled is the default (lifecycle ruling, #2607).
		case ast.AttrDisabled:
			disabled = true
		}
	}

	if err := validateSpecName(decl.Name); err != nil {
		return nil, err
	}

	// Edition 2026: `spec <bound> <name> = row => ...` / `trait <name> = row
	// => ...` (memql#5366). The body is the v1 lambda, and it is NOT converted
	// here: what its parameter reads depends on the binding (a concept's
	// declared fields, a shape's projected keys, nothing for a trait), which
	// is resolved after shapes load. The engine's Init pass lowers it into
	// Expr against that binding (lowerAllPushdownPositions); until then Expr
	// is nil, and resolveSpecBindings sets only the kind.
	if decl.Lambda != nil {
		if decl.Body != nil {
			return nil, fmt.Errorf("%s: %s %q has both a `{ return ... }` body and an `=` lambda; write one", origin, kindLabel, decl.Name)
		}
		if len(decl.Lambda.Params) != 1 {
			return nil, fmt.Errorf("%s: %s %q takes a lambda of one parameter, the row: = row => <predicate>", origin, kindLabel, decl.Name)
		}
		if disabled {
			return nil, nil
		}
		return &Spec{
			Name:        decl.Name,
			Description: languageParser.EffectiveDescription(decl.DocComment, description),
			ExprSource:  ast.FormatExpr(decl.Lambda),
			Origin:      origin,
			BoundName:   decl.BoundName,
			IsTrait:     decl.IsTrait,
			Lambda:      decl.Lambda,
		}, nil
	}

	if decl.Body == nil {
		return nil, fmt.Errorf("%s: %s %q: body is empty (expected `return <boolean expression>`)", origin, kindLabel, decl.Name)
	}

	converter := NewASTConverter()
	expr, err := converter.ConvertExpression(decl.Body)
	if err != nil {
		return nil, fmt.Errorf("%s: convert body of %s %q: %w", origin, kindLabel, decl.Name, err)
	}
	expr, err = normalizeSpecCallsToReferences(expr)
	if err != nil {
		return nil, fmt.Errorf("%s: normalize body of %s %q: %w", origin, kindLabel, decl.Name, err)
	}
	if err := ensureBooleanExpression(expr); err != nil {
		return nil, fmt.Errorf("%s: %s %q body must be boolean: %w", origin, kindLabel, decl.Name, err)
	}

	// Bare-only access (epic #2281): the body reads the bound surface by
	// bare field name. A `payload.` / `actor.` / `row.` / `meta.` prefix is
	// the old form -- reject it with a migration hint. The signature
	// already names the single bound context.
	if err := rejectPrefixedSpecFields(expr, origin, kindLabel, decl.Name); err != nil {
		return nil, err
	}

	// Classification + binding resolution are deferred to the post-load
	// resolveSpecBindings pass (engine bootstrap), which has the shape +
	// concept registries needed to resolve BoundName, rewrite the bare
	// field references to their underlying access form, and pick the eval
	// strategy. Kind is left empty here and finalized there.
	// A @disabled spec/trait is skipped at load (nil, nil is the
	// baseloader's intentional-skip contract): never registered, no
	// binding resolution, and the _reference sheets' long-standing claim
	// that @disabled skips trait registration becomes true (#2607). The
	// gate sits AFTER body validation deliberately -- a disabled spec must
	// still be semantically valid, or re-enabling it later bricks boot.
	if disabled {
		return nil, nil
	}

	return &Spec{
		Name:        decl.Name,
		Description: languageParser.EffectiveDescription(decl.DocComment, description),
		ExprSource:  canonicalExpression(expr),
		Expr:        expr,
		Kind:        "",
		UsesAI:      detectAIUsage(expr),
		Origin:      origin,
		BoundName:   decl.BoundName,
		IsTrait:     decl.IsTrait,
	}, nil
}

// rejectPrefixedSpecFields walks the spec/trait body and rejects any
// field reference that carries a `payload.` / `actor.` / `row.` / `meta.`
// prefix. Under the spec/shape binding redesign (epic #2281) the body
// reads bound fields by BARE name; the prefixed forms are retired.
func rejectPrefixedSpecFields(expr ExpressionNode, origin, kindLabel, name string) error {
	var walkErr error
	walkSpecRefs(expr, func(ref FieldReference) {
		if walkErr != nil || len(ref.Parts) == 0 {
			return
		}
		switch strings.ToLower(strings.TrimSpace(ref.Parts[0])) {
		case "payload", "meta":
			walkErr = fmt.Errorf("%s: %s %q body references %q -- read the bound field by BARE name (drop the `payload.` prefix); the signature already names the bound concept/shape", origin, kindLabel, name, joinParts(ref.Parts))
		case "actor":
			walkErr = fmt.Errorf("%s: %s %q body references %q -- a %s body may not read actor.* directly (epic #2281). Bind an @actor shape in the signature and read its projected key by bare name", origin, kindLabel, name, joinParts(ref.Parts), kindLabel)
		case "row":
			walkErr = fmt.Errorf("%s: %s %q body references %q -- a %s body may not read row.* directly (epic #2281). Bind a @row shape in the signature and read its projected key by bare name", origin, kindLabel, name, joinParts(ref.Parts), kindLabel)
		}
	})
	return walkErr
}

// joinParts renders a FieldReference's parts as a dotted path for
// diagnostics.
func joinParts(parts []string) string {
	return strings.Join(parts, ".")
}
