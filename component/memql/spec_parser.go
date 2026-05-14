package memql

import (
	"fmt"
	"strings"

	languageParser "github.com/visionarys-io/memql/component/language/parser"
	"github.com/visionarys-io/memql/component/memql/baseparser"
)

// parseSpecMemQL parses a single `.memql` source fragment containing
// exactly one `spec NAME { <bool-expr> }` or `trait NAME { <bool-expr> }`
// declaration and returns a *Spec ready for registration.
//
// This is the dedicated parser for spec + trait constructs. Both
// kinds share runtime contract (atomic boolean predicate registered
// in SpecRegistry) but differ in their author surface:
//
//   spec  -- requires exactly one of @useConcept(N) or @useShape(N).
//            Body fields under <N>.X are rewritten to payload.X at
//            load time (before the expression parser sees the body).
//   trait -- concept-agnostic. Forbids @useConcept + @useShape.
//
// Replaces the legacy pipeline (spec_rewrite -> trait_rewrite -> general
// parser -> ConvertSpecDef). The new path:
//
//   1. parseSpecMemQL identifies the keyword (`spec` or `trait`),
//      reads annotations into a typed decl, captures the body brace
//      contents verbatim.
//   2. The bare-name body translation pass applies the
//      `@useConcept(N)` / `@useShape(N)` `<N>.X -> payload.X` rewrite.
//   3. languageParser.ParseExpression turns the translated body into
//      the canonical ExpressionNode.
//   4. classifySpecKind walks the expression and labels the spec as
//      row-spec (SQL pushdown) or context-spec (in-process).
//
// Unknown annotations are hard-rejected. Mixed-reference bodies
// (payload + caller) are hard-rejected. Missing bindings on a spec
// + present bindings on a trait are hard-rejected.
func parseSpecMemQL(origin string, raw []byte) (*Spec, error) {
	p := &specMemQLParser{}
	p.Init(string(raw), origin)
	return p.parse(origin)
}

// specDecl is the intermediate the parser populates before building
// the final *Spec.
type specDecl struct {
	name                     string
	isTrait                  bool
	description              string
	useConceptName           string
	useShapeName             string
	bodySource               string
	seenLifecycleAnnotations map[string]bool
}

// specMemQLParser embeds baseparser.Base for shared scanning
// primitives (identifiers, quoted strings, balanced braces, inline +
// block comments, paren-string / paren-ident parsing). The expression
// body is delegated to the shared language-parser ParseExpression
// entry.
type specMemQLParser struct {
	baseparser.Base
}

// allowedSpecAnnotations + allowedTraitAnnotations enumerate the
// full author surfaces. The parser rejects anything else by name.
var allowedSpecAnnotations = map[string]bool{
	"description": true,
	"useConcept":  true,
	"useShape":    true,
}

var allowedTraitAnnotations = map[string]bool{
	"description": true,
	"enabled":     true,
	"disabled":    true,
}

func (p *specMemQLParser) parse(origin string) (*Spec, error) {
	decl := &specDecl{}

	for {
		p.SkipWhitespaceAndComments()
		if p.EOF() {
			break
		}

		// Annotations.
		if p.Peek() == '@' {
			p.Advance()
			name := p.ReadWord()
			if name == "" {
				return nil, fmt.Errorf("%s: expected annotation name after '@'", origin)
			}
			if err := p.parseAnnotation(decl, name); err != nil {
				return nil, fmt.Errorf("%s: %w", origin, err)
			}
			continue
		}

		// Keyword: spec | trait.
		if p.MatchWord("spec") {
			decl.isTrait = false
			if err := p.parseHeader(decl, origin); err != nil {
				return nil, err
			}
			break
		}
		if p.MatchWord("trait") {
			decl.isTrait = true
			if err := p.parseHeader(decl, origin); err != nil {
				return nil, err
			}
			break
		}
		return nil, fmt.Errorf("%s: expected `spec` or `trait` declaration, got %q", origin, p.Input[p.Pos:p.PosPlus(20)])
	}

	if decl.name == "" {
		return nil, fmt.Errorf("%s: no spec or trait declaration found", origin)
	}
	return p.buildSpec(decl, origin)
}

// parseAnnotation consumes the argument list (if any) and writes
// the value into the decl. Final allow-list check happens in
// buildSpec once the keyword is known (annotations come before the
// keyword, so we accept any annotation in EITHER allow-list here
// and reject mismatches downstream).
func (p *specMemQLParser) parseAnnotation(decl *specDecl, name string) error {
	switch name {
	case "description":
		val, err := p.ParseParenString()
		if err != nil {
			return fmt.Errorf("@description: %w", err)
		}
		decl.description = val
		return nil

	case "useConcept":
		val, err := p.ParseParenIdent()
		if err != nil {
			return fmt.Errorf("@useConcept: %w", err)
		}
		decl.useConceptName = val
		return nil

	case "useShape":
		val, err := p.ParseParenIdent()
		if err != nil {
			return fmt.Errorf("@useShape: %w", err)
		}
		decl.useShapeName = val
		return nil

	case "enabled":
		// Flag annotation -- valid on traits only. Drain optional
		// parens for parser robustness; the buildSpec allow-list
		// check rejects @enabled on specs.
		p.SkipOptionalParens()
		// Mark trait-only intent on decl via a private flag so
		// buildSpec can distinguish "saw @enabled" from "didn't".
		// We don't actually track @enabled state (registration is
		// unconditional today); we just need to confirm it was on
		// a trait, not a spec.
		return p.markAnnotationSeen(decl, "enabled")

	case "disabled":
		p.SkipOptionalParens()
		return p.markAnnotationSeen(decl, "disabled")

	default:
		// Drain any arguments before erroring so the message is
		// clean. Single-pass parser; can't recover otherwise.
		p.SkipOptionalParens()
		return fmt.Errorf("unknown annotation @%s -- supported: %s for specs / %s for traits",
			name,
			baseparser.FormatAnnotationAllowList(allowedSpecAnnotations),
			baseparser.FormatAnnotationAllowList(allowedTraitAnnotations))
	}
}

// markAnnotationSeen records that a lifecycle annotation was on the
// declaration so buildSpec can reject it on specs (the engine owns
// spec lifecycle). For traits, the annotation is a no-op marker.
func (p *specMemQLParser) markAnnotationSeen(decl *specDecl, name string) error {
	if decl.seenLifecycleAnnotations == nil {
		decl.seenLifecycleAnnotations = map[string]bool{}
	}
	decl.seenLifecycleAnnotations[name] = true
	return nil
}

// parseHeader reads `<name> { <body> }` after the keyword has been
// matched. The body's brace contents are captured verbatim.
func (p *specMemQLParser) parseHeader(decl *specDecl, origin string) error {
	p.SkipWhitespaceAndComments()
	decl.name = p.ReadWord()
	if decl.name == "" {
		return fmt.Errorf("%s: expected name after spec/trait keyword", origin)
	}

	p.SkipWhitespaceAndComments()
	if p.EOF() || p.Peek() != '{' {
		return fmt.Errorf("%s: expected '{' after declaration name", origin)
	}
	p.Advance() // consume '{'

	bodyStart := p.Pos
	bodyEnd, err := p.FindClosingBrace(bodyStart)
	if err != nil {
		return fmt.Errorf("%s: %w", origin, err)
	}
	decl.bodySource = strings.TrimSpace(p.Input[bodyStart:bodyEnd])
	p.Pos = bodyEnd + 1 // consume '}'
	return nil
}

// buildSpec applies the final allow-list + binding checks, runs the
// body through the bare-name translation pass + the expression
// parser, classifies the kind, and returns the Spec.
func (p *specMemQLParser) buildSpec(decl *specDecl, origin string) (*Spec, error) {
	kindLabel := "spec"
	if decl.isTrait {
		kindLabel = "trait"
	}

	// Lifecycle annotations: rejected on specs, accepted (no-op) on
	// traits.
	if !decl.isTrait {
		if decl.seenLifecycleAnnotations["enabled"] {
			return nil, fmt.Errorf("spec %q must not carry @enabled (the engine controls spec lifecycle; remove the annotation)", decl.name)
		}
		if decl.seenLifecycleAnnotations["disabled"] {
			return nil, fmt.Errorf("spec %q must not carry @disabled (the engine controls spec lifecycle; delete the spec instead)", decl.name)
		}
	}

	// Bindings rejected on traits.
	if decl.isTrait {
		if decl.useConceptName != "" {
			return nil, fmt.Errorf("trait %q must not carry @useConcept(...) (traits are concept-agnostic; bind only on specs)", decl.name)
		}
		if decl.useShapeName != "" {
			return nil, fmt.Errorf("trait %q must not carry @useShape(...) (traits are concept-agnostic; bind only on specs)", decl.name)
		}
	} else {
		// Specs require exactly one binding.
		hasUseConcept := decl.useConceptName != ""
		hasUseShape := decl.useShapeName != ""
		if !hasUseConcept && !hasUseShape {
			return nil, fmt.Errorf("spec %q is missing a binding -- declare exactly one of @useConcept(<bareName>) or @useShape(<bareName>)", decl.name)
		}
		if hasUseConcept && hasUseShape {
			return nil, fmt.Errorf("spec %q carries both @useConcept and @useShape -- declare exactly one", decl.name)
		}
	}

	// Bare-name body translation. `@useConcept(N)` / `@useShape(N)`
	// authors write payload references as `N.X`; rewrite to
	// `payload.X` before the expression parser tokenises.
	bodySource := decl.bodySource
	if decl.useConceptName != "" {
		bodySource = translateConceptPathsToPayload(bodySource, decl.useConceptName)
	}
	if decl.useShapeName != "" {
		bodySource = translateConceptPathsToPayload(bodySource, decl.useShapeName)
	}

	// Parse the body as a single boolean expression via the shared
	// expression parser.
	parsedExpr, err := languageParser.ParseExpression(bodySource)
	if err != nil {
		return nil, fmt.Errorf("%s: parse body of %s %q: %w", origin, kindLabel, decl.name, err)
	}

	converter := NewASTConverter()
	expr, err := converter.ConvertExpression(parsedExpr)
	if err != nil {
		return nil, fmt.Errorf("%s: convert body of %s %q: %w", origin, kindLabel, decl.name, err)
	}
	expr, err = normalizeSpecCallsToReferences(expr)
	if err != nil {
		return nil, fmt.Errorf("%s: normalize body of %s %q: %w", origin, kindLabel, decl.name, err)
	}
	if err := ensureBooleanExpression(expr); err != nil {
		return nil, fmt.Errorf("%s: %s %q body must be boolean: %w", origin, kindLabel, decl.name, err)
	}

	// Classify row-spec vs context-spec. Mixed bodies rejected.
	kind, classErr := classifySpecKind(expr)
	if classErr != nil {
		return nil, fmt.Errorf("classify %s %q: %w", kindLabel, decl.name, classErr)
	}

	if err := validateSpecName(decl.name); err != nil {
		return nil, err
	}

	return &Spec{
		Name:           decl.name,
		Description:    decl.description,
		ExprSource:     bodySource,
		Expr:           expr,
		Kind:           kind,
		UsesSI:         detectSIUsage(expr),
		Origin:         origin,
		UseConceptName: decl.useConceptName,
		UseShapeName:   decl.useShapeName,
		IsTrait:        decl.isTrait,
	}, nil
}

