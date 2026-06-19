package memql

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// validateLogicEventFields enforces FIELD-LEVEL validation of an event-triggered
// logic's `event.payload.<field>` references against a declared per-logic event
// schema (memql#1743, follow-up to the binding-level check in
// validateLogicEventBinding / memql#1706).
//
// Why a declared schema (not the @trigger concept): several event-trigger logics
// consume synthetic, handler-assembled events that are NOT a concept payload
// (e.g. logicGenerateResponse reads event.payload.promptTemplateId / promptData
// / siParticipantId / toolNames / triggerText -- fields the Go cognition handler
// assembles into a cognition.response.requested event; no concept owns them). A
// concept-derived validator would false-reject those logics outright. So the
// schema is declared explicitly on the logic via the `@eventField(...)`
// annotation, which lists the allowed top-level payload field names.
//
// OPT-IN by design: a logic is field-validated ONLY when it declares
// `@eventField(...)`. A logic without the annotation is unaffected (the
// binding-level check still applies). This guarantees zero false-rejects across
// the live DSL tree -- adding the annotation to a logic is a deliberate, audited
// act, and the head-set is computed the same way the validator extracts refs.
//
// Example:
//
//	@enabled
//	@eventField("spaceId", "siParticipantId", "promptTemplateId", "promptData", "utteranceId", "agentId")
//	logic logicGenerateResponse {
//	  args { event object @required }
//	  body { ... args.event.payload.spaceId ... }   // every payload.<field> must be declared
//	}
//
// Scope: Logic functions only. Runs on the RAW source (pre-path-translation) so
// author-written `args.event.payload.*` / `event.payload.*` references pass
// through unchanged. Reads only the FIRST path segment after `payload.` (so
// `event.payload.node.type` validates `node`), matching how the runner threads
// the event object into nested step scope.
func validateLogicEventFields(rawSource string, funcDef *languageParser.FunctionDef) error {
	if funcDef == nil || funcDef.Type != languageParser.FunctionTypeLogic {
		return nil
	}
	declared, ok := declaredEventFields(funcDef)
	if !ok {
		return nil // opt-in: no @eventField annotation -> not field-validated
	}

	body := extractFunctionBody(rawSource)
	if body == "" {
		return nil
	}
	// Strip annotation lines + comments so neither the @eventField annotation's
	// own field strings nor a commented-out `event.payload.x` are mistaken for a
	// live body reference (which would false-reject).
	scan := stripLineComments(stripBlockComments(stripAttrLines(body)))

	var unknown []string
	seen := map[string]bool{}
	for _, m := range eventPayloadRefPattern.FindAllStringSubmatch(scan, -1) {
		head := m[1]
		if declared[head] || seen[head] {
			continue
		}
		seen[head] = true
		unknown = append(unknown, head)
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	allowed := make([]string, 0, len(declared))
	for f := range declared {
		allowed = append(allowed, f)
	}
	sort.Strings(allowed)
	return fmt.Errorf(
		"logic %q references event payload field(s) %s not declared in its @eventField schema (declared: %s); "+
			"either add the field to @eventField(...) if the triggering event carries it, or fix the typo "+
			"(memql#1743 -- per-logic event-field validation)",
		funcDef.Name, strings.Join(unknown, ", "), strings.Join(allowed, ", "),
	)
}

// eventPayloadRefPattern captures the FIRST path segment after `event.payload.`
// in a body reference (`args.event.payload.X` and the bare `event.payload.X`
// both contain `event.payload.X`). Only payload references are validated;
// envelope keys (event.topic / event.kind) and whole-object `args.event` passes
// are intentionally not field-checked.
var eventPayloadRefPattern = regexp.MustCompile(`\bevent\.payload\.([A-Za-z_][A-Za-z0-9_]*)`)

// declaredEventFields returns the set of allowed top-level payload field names
// declared via `@eventField(...)`, and whether the annotation is present at all.
// A declared entry may be a bare name (`spaceId`) or a `payload.`-prefixed path
// (`payload.spaceId`); both normalize to the head segment `spaceId`.
func declaredEventFields(funcDef *languageParser.FunctionDef) (map[string]bool, bool) {
	for _, attr := range funcDef.Attributes {
		if attr == nil || attr.Name != "eventField" {
			continue
		}
		set := map[string]bool{}
		for _, v := range attributeValueList(attr.Value) {
			head := normalizeEventFieldHead(v)
			if head != "" {
				set[head] = true
			}
		}
		return set, true
	}
	return nil, false
}

// normalizeEventFieldHead reduces a declared entry to its top-level payload
// field name: strips a leading `payload.` and keeps only the first segment.
func normalizeEventFieldHead(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "payload.")
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s = s[:i]
	}
	return s
}

// attributeValueList coerces an attribute value (which the parser yields as a
// []any/[]string for `@x("a", "b")`, or a bare string for `@x("a")`) to a
// string slice.
func attributeValueList(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case string:
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

var lineCommentPattern = regexp.MustCompile(`//[^\n]*`)
var blockCommentPattern = regexp.MustCompile(`(?s)/\*.*?\*/`)

func stripLineComments(s string) string  { return lineCommentPattern.ReplaceAllString(s, "") }
func stripBlockComments(s string) string { return blockCommentPattern.ReplaceAllString(s, "") }
