package memql

import (
	"fmt"

	"github.com/znasllc-io/memql/component/language/annotations"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/baseparser"
)

// function_annotation_allow_lists.go exposes the four per-construct
// annotation allow-lists the unified function loader's pre-parse
// validator consults. They are a load-time gate that surfaces typo'd
// or stale annotations as hard parse errors instead of letting them
// slip through to FunctionDef's generic attribute map.
//
// The lists are no longer hand-maintained here: they derive from the
// single physical registry in component/language/annotations (#991),
// the same source the editor/sense surface derives from, so the two
// can never drift. `constructAnnotationAllowLists` in
// unified_functions_loader.go is the sole consumer.
//
// The retired `@use*` family (`useConcept` / `useShape` / `useTrait` /
// `useSpec` / `useQuery` / `useMutation` / `useLogic` / `useBuiltin` /
// `usePrompt` / `useTool` / `useAutomation`) is deliberately absent:
// `ValidateConstructAnnotations` (baseparser/iface.go) hard-rejects
// anything matching the `@use*` prefix with a migration hint BEFORE the
// allow-list is consulted, so listing them was a dead no-op. The audit
// in #964 confirmed zero remaining occurrences across the tree; the
// no-op entries were pruned in #966.
//
// `@public` (on Query / Mutation) is a parse-only per-row-authz marker
// from issue #54 — no runtime semantics, just the author's explicit
// acknowledgement that the construct is intentionally callable without
// a caller-scope filter (concept catalogs, pre-auth login paths). See
// docs/public/operate/auth/per-row-authz-audit.md.

var (
	allowedQueryAnnotations      = annotations.Set("Query")
	allowedMutationAnnotations   = annotations.Set("Mutation")
	allowedLogicAnnotations      = annotations.Set("Logic")
	allowedAutomationAnnotations = annotations.Set("Automation")
)

// ValidateAutomationAnnotations closes the silent-tolerance gap (#2712):
// automations load through component/automations (the dedicated runtime
// loader), never reaching dispatchPerConstructParser's gate, and
// processAutomationAttributes has no default case -- so an unknown, dead, or
// retired annotation on an automation was silently dropped instead of
// load-rejected. This applies the SAME allow-list + retired-name gate the
// function kinds use. Terse single-step sources are lowered first (idempotent
// for the struct form) so leading annotations are visible to the header scan.
func ValidateAutomationAnnotations(source string) error {
	if lowered, err := languageParser.NormaliseTerseAutomationSource(source); err == nil {
		source = lowered
	}
	return baseparser.ValidateConstructAnnotations(source, "automation", allowedAutomationAnnotations)
}

// fieldAnnotations is the ONE allow-list for a FIELD annotation, wherever the
// field appears: an args block, a prompt body, a builtin body or a tool body.
//
// Before memql#5375 there were three behaviours for one surface. An args
// field REFUSED an unknown name (#991); a prompt field tolerated it SILENTLY
// (prompt_converter's default arm was a comment saying so); a builtin field
// dropped every annotation but @required (BuiltinField.Attributes was
// documented as "tolerated, not yet acted on"). So `@requred` -- one r -- was
// a load error in one body, a silently-absent constraint in the second and a
// silently-absent description in the third, and only the first told the
// author. D16's last sentence closes that: prompt fields and builtin fields
// get the allow-list args fields have.
//
// @default IS here and is RETIRED on a concept field, which is not a
// contradiction. A prompt / builtin / tool body IS the JSON schema handed to
// the model, so `default` is a value the model reads; a concept field's was
// published as a schema keyword nothing applies.
var fieldAnnotations = map[string]bool{
	"required":    true,
	"description": true,
	"default":     true,
	"enum":        true,
	"maxLength":   true,
	"pattern":     true,
	"minimum":     true,
	"maximum":     true,
}

// validateFieldAnnotation returns a refusal for a field annotation that is
// retired or unknown, and nil for one the schema surface accepts.
//
// The retirement ledger is consulted FIRST so a retired name gets its
// migration hint rather than the generic list of supported names -- an author
// who wrote a name that was correct last release is not making a typo, and
// telling them the list of valid names does not say which one replaced theirs.
func validateFieldAnnotation(origin, construct, field, name string) error {
	if fieldAnnotations[name] {
		return nil
	}
	if hint, retired := baseparser.RetiredFieldAnnotation(name); retired {
		return fmt.Errorf("%s: %s field %q: @%s is retired -- %s", origin, construct, field, name, hint)
	}
	if hint, retired := baseparser.RetiredConstructAnnotation(name); retired {
		return fmt.Errorf("%s: %s field %q: @%s is retired -- %s", origin, construct, field, name, hint)
	}
	return fmt.Errorf("%s: %s field %q: unknown field annotation @%s -- supported: %s",
		origin, construct, field, name, baseparser.FormatAnnotationAllowList(fieldAnnotations))
}
