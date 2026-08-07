package automations

// args_secret.go carries memql#3183 (epic memql#3111): @secret redaction for
// the automation args binder.
//
// # Why this surface needs it at all
//
// A `graph.node.created.<concept>` event does not carry the concept row by
// reference -- component/memql/executor_mutation.go:805-819 FLATTENS the
// stored payload into the event payload (`maps.Copy(eventPayload, payloadMap)`).
// So every field of the row, including one the concept annotates `@secret`,
// arrives in `event.Payload` under its own name. bindEventArgs then reads it
// by declared field name and validateAutomationArg quotes it into the refusal
// reason -- which refuseFireForArgs writes to a WARN log (args_binding.go:285).
// That is the one path by which a concept row's secret value reaches a
// STRUCTURED LOG, which memql#3036's original scoping ruling assumed did not
// exist.
//
// # How the Secret flag is plumbed -- and why THIS way
//
// The constraint the issue calls out: the automations package must not reach
// for component/memql's function loader to get the concept binding. It does
// not need to. Both halves are already in this package:
//
//  1. THE CONCEPT BINDING. An automation's trigger topic IS its concept
//     binding. loader.go normalizes every structured/`on=` trigger to the
//     canonical `graph.node.<action>.<conceptId>` form (loader.go:557 and
//     normalizeStructuredTriggers, via ast.BuildTriggerTopic) BEFORE the
//     Automation struct is built, so Automation.Trigger.Event carries the
//     fully-qualified concept id as its trailing segment. No new field on the
//     automation contract, and nothing to thread through the fire path.
//
//  2. THE CONCEPT REGISTRY. Loader already holds one
//     (loader.go:24, `registry memoryNodes.Registry`) and already queries it
//     for `use`-import resolution. component/database/memory-nodes is a LEAF
//     relative to both memql and automations -- automations/loader.go,
//     checkpoint.go and sandbox_bridge.go import it today -- so reading
//     Concept.SecretFields() from it introduces no edge that did not already
//     exist, let alone a cycle. (The cycle the package comment warns about is
//     the other direction: memql importing automations.)
//
// Stamping therefore happens at LOAD time in compileMemQL, exactly mirroring
// component/memql/function_loader.go's markSecretArgsFields, and for the same
// two reasons that one gives: the registry is a loader-side dependency the
// validator deliberately does not have, and doing it once at load keeps the
// per-fire path a plain field read.
//
// The alternative -- resolving the concept from the LIVE event topic at fire
// time -- was rejected: it would put a registry lookup on every refusal path,
// require handing bindEventArgs a registry it has no other use for, and buy
// nothing, since a concept-scoped trigger is the only shape that can deliver a
// flattened row payload in the first place.
//
// FAILS OPEN, deliberately and visibly, on every input it cannot resolve: no
// registry, a scheduled or glob trigger with no concept segment, or a concept
// the registry cannot resolve leaves every field unstamped and every message
// byte-identical to before. Refusing to load over a diagnostic detail would
// let an unrelated registry gap take down boot.

import (
	"strconv"
	"strings"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// redactedArgValue replaces a rejected value in an args-contract refusal
// message (and therefore in the WARN log that message lands in) when the
// field is @secret on the automation's trigger concept.
//
// The message keeps everything that makes it actionable -- which automation,
// which topic, which field, and which declared constraint was violated -- and
// drops only the value. A declared constraint (an enum's members, a pattern
// source) comes from the automation's own schema, never from event data, so it
// is never redacted.
//
// Defined here rather than imported from component/memql's function validator
// (which uses the identical literal) so this package keeps no dependency on it
// for a string constant. The two MUST stay in sync: an operator grepping logs
// for the placeholder has to find both surfaces.
const redactedArgValue = "<redacted>"

// argValueForMessage returns what to interpolate for a rejected value: the
// value itself, or the redaction placeholder when the field is @secret.
func argValueForMessage(field *ArgsField, value any) any {
	if field != nil && field.Secret {
		return redactedArgValue
	}
	return value
}

// quotedArgValueForMessage is argValueForMessage for the site that rendered
// the value with %q. It returns an ALREADY-QUOTED string so the caller can
// interpolate with %v: strconv.Quote reproduces %q byte for byte, so a
// non-secret message is unchanged, while the placeholder stays unquoted and is
// therefore unmistakable for a real value that happens to read "<redacted>".
func quotedArgValueForMessage(field *ArgsField, value string) string {
	if field != nil && field.Secret {
		return redactedArgValue
	}
	return strconv.Quote(value)
}

// markSecretArgsFields stamps ArgsField.Secret on every declared args field
// whose name matches a field the automation's TRIGGER CONCEPT annotates
// `@secret`.
//
// Matching is BY DECLARED ARGS-FIELD NAME, which for this surface is also the
// payload key: bindEventArgs looks the value up as `payload[field.Name]`, and
// executor_mutation.go flattens the row under its own field names. So unlike
// the function-loader twin -- where an arg may be renamed between the args
// block and the insert target -- name matching here is exact rather than a
// heuristic: if the names differ, the binder never reads that payload key at
// all and there is nothing to leak.
//
// It never suppresses a validation FAILURE; only the value inside the message.
func markSecretArgsFields(automation *Automation, registry memoryNodes.Registry) {
	if automation == nil || automation.Args == nil || len(automation.Args.Fields) == 0 {
		return
	}
	if registry == nil || automation.Trigger == nil {
		return
	}
	conceptId := conceptIdFromTriggerTopic(automation.Trigger.Event)
	if conceptId == "" {
		return
	}
	concept, err := registry.Get(conceptId)
	if err != nil || concept == nil {
		return
	}
	secret := concept.SecretFields()
	if len(secret) == 0 {
		return
	}
	names := make(map[string]struct{}, len(secret))
	for _, name := range secret {
		names[name] = struct{}{}
	}
	for _, field := range automation.Args.Fields {
		markSecretArgsField(field, names)
	}
}

// markSecretArgsField stamps one field and its nested / item children.
func markSecretArgsField(field *ArgsField, secretNames map[string]struct{}) {
	if field == nil {
		return
	}
	if _, ok := secretNames[field.Name]; ok {
		field.Secret = true
	}
	for _, nested := range field.Nested {
		markSecretArgsField(nested, secretNames)
	}
	// An array's Items carries the ELEMENT schema and usually has no name of
	// its own, so it inherits the array field's classification: a `[]string`
	// of secrets is a secret in every element.
	if field.Items != nil {
		if field.Secret {
			field.Items.Secret = true
		}
		markSecretArgsField(field.Items, secretNames)
	}
}

// conceptIdFromTriggerTopic extracts the concept id from a normalized
// graph-event trigger topic.
//
// By the time an Automation exists, loader.go has rewritten both authored
// trigger forms (`on=<concept>.<action>` and the structured
// `event="node.created", concept="..."`) into the canonical
// `graph.node.<action>.<conceptId>` topic, and a concept id is
// colon-delimited (`v1:identity:authCode`) so it is always exactly the
// trailing dot-segment.
//
// Returns "" -- meaning "no concept binding, stamp nothing" -- for a
// non-graph topic (schedule / system.startup / cognition.*), a concept-less
// graph topic (`graph.node.deleted`, which matches any concept), or a glob
// pattern. Those cannot name one concept, so there is no @secret set to read.
func conceptIdFromTriggerTopic(topic string) string {
	topic = strings.TrimSpace(topic)
	if !strings.HasPrefix(topic, "graph.") {
		return ""
	}
	idx := strings.LastIndex(topic, ".")
	if idx < 0 {
		return ""
	}
	conceptId := topic[idx+1:]
	if !strings.Contains(conceptId, ":") {
		return ""
	}
	if strings.ContainsAny(conceptId, "*#") {
		return ""
	}
	return conceptId
}
