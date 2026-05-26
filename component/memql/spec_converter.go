package memql

// spec_converter.go bridges the langparser's *ast.SpecDecl AST node
// (introduced by memql#334 / sub-epic #329 / #310 Stage 1C) to the
// memql package's *Spec registry type. The hand-rolled parseSpecMemQL
// (spec_parser.go) is unreferenced from production after this child
// lands but kept for its tests until sub-epic #329's cleanup PR.
//
// Semantics mirror parseSpecMemQL one-for-one:
//
//   * Annotation surface: specs allow @description only; traits also
//     allow @enabled / @disabled (no-op flags -- the engine controls
//     spec lifecycle, not the author surface). The retired
//     @useConcept / @useShape annotations are rejected as unknown
//     (sub-epic #301 retired them; specs + traits now bind their
//     concept context via the file-top `use ...` import).
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

	var description string
	var sawEnabled, sawDisabled bool

	for _, attr := range decl.Attributes {
		switch attr.Name {
		case "description":
			val, ok := attr.Value.(string)
			if !ok {
				return nil, fmt.Errorf("%s: @description expects a string value", origin)
			}
			description = val
		case "enabled":
			sawEnabled = true
		case "disabled":
			sawDisabled = true
		case "useConcept", "useShape":
			// Retired in #301 -- specs + traits bind via file-top
			// `use ...` imports + the signature.
			return nil, fmt.Errorf("%s: @%s is retired (#301) -- bind via file-top `use <namespace>.{ %s }` imports", origin, attr.Name, decl.Name)
		default:
			return nil, fmt.Errorf("%s: unknown %s annotation @%s (supported: @description for specs; @description / @enabled / @disabled for traits)", origin, kindLabel, attr.Name)
		}
	}

	// Lifecycle annotations: rejected on specs, accepted (no-op) on
	// traits. Mirrors the parseSpecMemQL contract.
	if !decl.IsTrait {
		if sawEnabled {
			return nil, fmt.Errorf("%s: spec %q must not carry @enabled (the engine controls spec lifecycle; remove the annotation)", origin, decl.Name)
		}
		if sawDisabled {
			return nil, fmt.Errorf("%s: spec %q must not carry @disabled (the engine controls spec lifecycle; delete the spec instead)", origin, decl.Name)
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

	return &Spec{
		Name:        decl.Name,
		Description: description,
		ExprSource:  canonicalExpression(expr),
		Expr:        expr,
		Kind:        kind,
		UsesSI:      detectSIUsage(expr),
		Origin:      origin,
		IsTrait:     decl.IsTrait,
	}, nil
}
