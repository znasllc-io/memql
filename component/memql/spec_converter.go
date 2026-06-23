package memql

// spec_converter.go bridges the langparser's *ast.SpecDecl AST node
// (introduced by memql#334 / sub-epic #329 / #310 Stage 1C) to the
// memql package's *Spec registry type. The hand-rolled parseSpecMemQL
// (spec_parser.go) is unreferenced from production after this child
// lands but kept for its tests until sub-epic #329's cleanup PR.
//
// Semantics mirror parseSpecMemQL one-for-one, except the annotation
// surface now derives from the single source of truth in
// component/language/annotations (the same registry the function
// load-gate + editor derive from) so the converter, the load gate, and
// the editor can never drift (#1031):
//
//   * Annotation surface: specs + traits accept @description (carries a
//     value), @shape (optionally pins the shape the predicate reads -- a
//     no-op, since the eval strategy is derived from the body's field
//     references, not the pin), and @enabled / @disabled (author-surface
//     lifecycle no-ops -- the engine controls spec lifecycle). Anything
//     else is a hard error so a typo'd or stale annotation surfaces
//     instead of the construct being silently dropped at load. The
//     retired @useConcept / @useShape annotations keep an explicit
//     migration hint (sub-epic #301 retired them; specs + traits now
//     bind their concept context via the file-top `use ...` import).
//   * Body conversion: NewASTConverter().ConvertExpression on the
//     pre-parsed ast.ExpressionNode -> normalizeSpecCallsToReferences
//     -> ensureBooleanExpression -> classifySpecKind.
//   * Spec.ExprSource is set to canonicalExpression(engineExpr) --
//     parseSpecMemQL captured the raw author body; the canonical
//     form serves the only downstream consumer (Spec.clone) without
//     introducing a source-string capture pass on the langparser side.

import (
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/language/annotations"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// specDeclToSpec converts a langparser SpecDecl into the engine's
// *Spec registry type. Returns an error matching parseSpecMemQL's
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

	// The accepted annotation surface for specs + traits is the single
	// physical registry (component/language/annotations), the same source
	// the function load-gate + editor derive from. Spec and trait share the
	// "Spec" receiver set: description / enabled / disabled / shape.
	allowed := annotations.Set("Spec")

	var description string
	for _, attr := range decl.Attributes {
		// The retired @use* family keeps an explicit migration hint (it is
		// absent from the registry, so it would otherwise read as a generic
		// unknown annotation).
		if attr.Name == "useConcept" || attr.Name == "useShape" {
			return nil, fmt.Errorf("%s: @%s is retired (#301) -- bind via file-top `use <namespace>.{ %s }` imports", origin, attr.Name, decl.Name)
		}
		if !allowed[attr.Name] {
			return nil, fmt.Errorf("%s: unknown %s annotation @%s (supported: %s)", origin, kindLabel, attr.Name, strings.Join(annotations.ByReceiver["Spec"], " / "))
		}
		switch attr.Name {
		case "description":
			val, ok := attr.Value.(string)
			if !ok {
				return nil, fmt.Errorf("%s: @description expects a string value", origin)
			}
			description = val
		case "shape":
			if _, ok := attr.Value.(string); !ok {
				return nil, fmt.Errorf("%s: @shape expects a string value (the pinned shape name)", origin)
			}
			// No-op pin: documents the shape the predicate reads; the eval
			// strategy is derived from the body, not this annotation.
		case "enabled", "disabled":
			// Author-surface lifecycle no-ops; the engine controls spec
			// lifecycle.
		}
	}

	if err := validateSpecName(decl.Name); err != nil {
		return nil, err
	}

	if decl.Body == nil {
		return nil, fmt.Errorf("%s: %s %q: body is empty (expected a boolean expression)", origin, kindLabel, decl.Name)
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

	kind, classErr := classifySpecKind(expr)
	if classErr != nil {
		return nil, fmt.Errorf("%s: classify %s %q: %w", origin, kindLabel, decl.Name, classErr)
	}

	if err := validateSpecRole(decl.IsTrait, kind, decl.Name); err != nil {
		return nil, fmt.Errorf("%s: %w", origin, err)
	}

	return &Spec{
		Name:        decl.Name,
		Description: description,
		ExprSource:  canonicalExpression(expr),
		Expr:        expr,
		Kind:        kind,
		UsesAI:      detectAIUsage(expr),
		Origin:      origin,
		IsTrait:     decl.IsTrait,
	}, nil
}
