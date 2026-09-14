package memql

// spec_converter.go bridges the langparser's *ast.SpecDecl AST node
// (introduced by memql#334 / sub-epic #329 / #310 Stage 1C) to the
// memql package's *Spec registry type. The hand-rolled spec parser it
// replaced was deleted with memql#5359, which made the annotation registry
// the one list of what a spec takes.
//
// Which annotations a spec or trait takes is decided at parse time by the
// annotation registry (component/language/annotations, memql#5359), which
// also carries the migration hints for the retired @shape, @row, @actor and
// @use* forms. The converter reads what the legal ones mean:
//
//   * Annotations: @description (carries a value) and @enabled / @disabled
//     (author-surface lifecycle; @disabled skips registration).
//   * Body: the edition-2026 lambda, kept whole on Spec.Lambda with its
//     canonical source in Spec.ExprSource. What its parameter reads depends
//     on the binding, which resolves after shapes load, so the engine's Init
//     pass lowers it (lowerAllPushdownPositions).

import (
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// specDeclToSpec converts a langparser SpecDecl into the engine's
// *Spec registry type. Returns an error matching the retired parser's
// surface so the loader's diagnostic messages stay identical across
// the migration. A @disabled declaration converts to nil, nil -- the
// baseloader's intentional-skip contract -- once its body has been
// validated as far as this conversion can.
func specDeclToSpec(decl *languageParser.SpecDecl, origin string) (*Spec, error) {
	spec, disabled, err := convertSpecDecl(decl, origin)
	if err != nil || disabled {
		return nil, err
	}
	return spec, nil
}

// convertSpecDecl is specDeclToSpec reporting @disabled rather than dropping
// it: for a @disabled declaration it returns the spec it would have
// registered, because its body is validated by Lower at Init, not here, and
// the loader keeps it for that (SpecRegistry.disabledBodies).
func convertSpecDecl(decl *languageParser.SpecDecl, origin string) (*Spec, bool, error) {
	spec, err := convertSpecDeclBody(decl, origin)
	if err != nil {
		return nil, false, err
	}
	disabled := false
	for _, attr := range decl.Attributes {
		if attr.Name == ast.AttrDisabled {
			disabled = true
		}
	}
	return spec, disabled, nil
}

func convertSpecDeclBody(decl *languageParser.SpecDecl, origin string) (*Spec, error) {
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
	for _, attr := range decl.Attributes {
		if attr.Name != "description" {
			continue
		}
		val, ok := attr.Value.(string)
		if !ok {
			return nil, fmt.Errorf("%s: @description expects a string value", origin)
		}
		description = val
	}

	if err := validateSpecName(decl.Name); err != nil {
		return nil, err
	}

	// The body is the edition-2026 lambda (memql#5366), and it is NOT
	// converted here: what its parameter reads depends on the binding (a
	// concept's declared fields, a shape's projected keys, nothing for a
	// trait), which is resolved after shapes load. The engine's Init pass
	// lowers it into Expr against that binding (lowerAllPushdownPositions);
	// until then Expr is nil, and resolveSpecBindings sets only the kind.
	if decl.Lambda == nil {
		return nil, fmt.Errorf("%s: %s %q has no body (expected `= row => <predicate>`)", origin, kindLabel, decl.Name)
	}
	if len(decl.Lambda.Params) != 1 {
		return nil, fmt.Errorf("%s: %s %q takes a lambda of one parameter, the row: = row => <predicate>", origin, kindLabel, decl.Name)
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

// joinParts renders a FieldReference's parts as a dotted path for
// diagnostics.
func joinParts(parts []string) string {
	return strings.Join(parts, ".")
}
