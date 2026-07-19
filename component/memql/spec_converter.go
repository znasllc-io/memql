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
	"github.com/znasllc-io/memql/component/language/ast"
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
	// physical registry (component/language/annotations): description /
	// enabled / disabled. The @shape pin is removed (epic #2281, Story 5)
	// -- the binding moved to the signature -- and @row / @actor are
	// shape-only ambient gateways (Story 4); a spec/trait carrying them is
	// rejected with a migration hint.
	allowed := annotations.Set("Spec")

	var description string
	var disabled bool
	for _, attr := range decl.Attributes {
		switch attr.Name {
		case "useConcept", "useShape":
			// Retired @use* family (#301) keeps an explicit migration hint.
			return nil, fmt.Errorf("%s: @%s is retired (#301) -- bind via file-top `use <namespace>.{ %s }` imports", origin, attr.Name, decl.Name)
		case "shape":
			return nil, fmt.Errorf("%s: @shape is removed (epic #2281) -- a spec binds its shape/concept in the signature: `spec <boundName> %s { return <bool> }` (boundName resolves via the file-top `use` import)", origin, decl.Name)
		case "row", "actor":
			return nil, fmt.Errorf("%s: @%s is a shape-only marker (epic #2281) -- a %s may not carry it. To predicate on %s data, bind a @%s shape in the signature and read its projected key by bare name", origin, attr.Name, kindLabel, ambientWord(attr.Name), attr.Name)
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
		case ast.AttrEnabled:
			// Accepted no-op: enabled is the default (lifecycle ruling, #2607).
		case ast.AttrDisabled:
			disabled = true
		}
	}

	if err := validateSpecName(decl.Name); err != nil {
		return nil, err
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
		Description: description,
		ExprSource:  canonicalExpression(expr),
		Expr:        expr,
		Kind:        "",
		UsesAI:      detectAIUsage(expr),
		Origin:      origin,
		BoundName:   decl.BoundName,
		IsTrait:     decl.IsTrait,
	}, nil
}

// ambientWord maps the shape-kind marker to the ambient surface it
// unlocks, for the spec-carries-@row/@actor migration message.
func ambientWord(marker string) string {
	if marker == "actor" {
		return "caller / auth-envelope"
	}
	return "row-metadata"
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
