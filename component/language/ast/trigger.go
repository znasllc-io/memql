package ast

import (
	"fmt"
	"sort"
	"strings"
)

// Trigger-topic assembly for the structured @trigger form.
//
// Today authors write:
//
//	@trigger(event="graph.node.created.v1:cognition:participant")
//
// The structured form:
//
//	@trigger(event="node.created", concept=cog.participant)
//
// The concept reference (`cog.participant`) is a cross-file symbol;
// the loader resolves it to the canonical concept ID
// `v1:cognition:participant` using the importing file's alias table,
// then this package's BuildTriggerTopic assembles the actual
// subscription topic string the engine uses.

// allowedEventKinds is the closed set of `event=` values the
// structured trigger accepts. Mirrors the action segment of the
// 4-segment topic format the engine emits
// (graph.node.{action}.{concept}).
var allowedEventKinds = map[string]bool{
	"node.created": true,
	"node.updated": true,
	"node.deleted": true,
}

// EventKindAllowed reports whether the supplied event-kind string is
// one of the documented structured-trigger values.
func EventKindAllowed(kind string) bool {
	return allowedEventKinds[kind]
}

// AllowedEventKinds returns the closed set, sorted, for error messages
// that have to tell an author what they may write instead. Exported so
// callers name the same set the gate enforces rather than restating it
// (memql#3614).
func AllowedEventKinds() []string {
	out := make([]string, 0, len(allowedEventKinds))
	for k := range allowedEventKinds {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// BuildTriggerTopic assembles the subscription topic from the
// structured-trigger fields. Returns an error when the event-kind is
// malformed or empty. concept may be empty when the trigger is not
// scoped to a single concept (matches any concept).
//
// Format: graph.{event-action-segments}[.{concept}]
//
//	event="node.created", concept="v1:cognition:participant"
//	  ==> "graph.node.created.v1:cognition:participant"
//
//	event="node.deleted", concept=""
//	  ==> "graph.node.deleted" (concept-less, matches any concept)
func BuildTriggerTopic(eventKind, conceptId string) (string, error) {
	if eventKind == "" {
		return "", fmt.Errorf("event-kind cannot be empty")
	}
	if !EventKindAllowed(eventKind) {
		allowed := []string{}
		for k := range allowedEventKinds {
			allowed = append(allowed, k)
		}
		return "", fmt.Errorf("event %q is not one of the allowed kinds: %s", eventKind, strings.Join(allowed, ", "))
	}

	topic := "graph." + eventKind
	if conceptId != "" {
		topic += "." + conceptId
	}
	return topic, nil
}

// SplitTriggerTopic is the inverse of BuildTriggerTopic: it recovers the
// structured (event-kind, concept) pair from a composed subscription topic.
//
// WHY AN INVERSE IS NEEDED AT ALL. The loader folds the concept INTO the topic
// at load time -- normalizeStructuredTriggers calls BuildTriggerTopic, writes
// the result back over `event=`, and DELETES `concept=` -- so a running node
// holds ONE string where the author wrote two fields. Everything that FIRES the
// automation wants that one string, which is why it is stored that way. But
// anything that has to DESCRIBE the automation wants the pair back: the
// construct catalog reports a trigger in the language server's own shape
// (memql#3805), and the run form it feeds decides which payload modes to offer
// from the event and which concept's rows to browse from the concept.
//
// WHY IT CAN BE DONE WITHOUT AMBIGUITY. A concept id is colon-separated and an
// event kind is dot-separated, but the topic joins the two with a dot, so a
// naive split on "." cannot tell where the kind ends. It does not have to: the
// kinds are a CLOSED SET (allowedEventKinds), so the remainder after "graph."
// is MATCHED against that set rather than parsed. A topic this function does
// not recognise -- a raw application topic, a legacy topic whose action is not
// one of the three, a glob -- returns ok=false, and the caller keeps the whole
// string as the event, which is what the author wrote.
//
//	"graph.node.created.v1:cognition:participant" -> ("node.created", "v1:cognition:participant", true)
//	"graph.node.deleted"                          -> ("node.deleted", "",                         true)
//	"system.startup"                              -> ("",             "",                         false)
func SplitTriggerTopic(topic string) (eventKind, conceptId string, ok bool) {
	const prefix = "graph."
	if !strings.HasPrefix(topic, prefix) {
		return "", "", false
	}
	rest := topic[len(prefix):]
	for kind := range allowedEventKinds {
		if rest == kind {
			return kind, "", true
		}
		if strings.HasPrefix(rest, kind+".") {
			return kind, rest[len(kind)+1:], true
		}
	}
	return "", "", false
}

// ExtractStructuredTriggerArgs pulls the structured fields
// from an attribute's Args map. Reports which fields were present
// so the caller can distinguish "missing field" from "field set to
// the empty string."
//
// The concept field's value can be a string (a literal already-
// resolved ID like "v1:cognition:participant") OR a symbol ref
// (a dotted-identifier like "cog.participant"). The caller is
// responsible for resolving the symbol ref via the per-file alias
// table before calling BuildTriggerTopic.
type StructuredTriggerArgs struct {
	EventKind  string
	Concept    string
	HasEvent   bool
	HasConcept bool
}

// ParseStructuredTriggerArgs reads `event` and `concept` from the
// trigger attribute's Args map. Tolerates extra unknown args.
func ParseStructuredTriggerArgs(args map[string]any) (*StructuredTriggerArgs, error) {
	out := &StructuredTriggerArgs{}

	if v, ok := args["event"]; ok {
		s, isStr := v.(string)
		if !isStr {
			return nil, fmt.Errorf("@trigger event= must be a string, got %T", v)
		}
		out.EventKind = s
		out.HasEvent = true
	}

	if v, ok := args["concept"]; ok {
		out.HasConcept = true
		switch s := v.(type) {
		case string:
			out.Concept = s
		default:
			// Concept can also arrive as an AST node when the parser
			// recognises dotted-identifier syntax as a symbol ref.
			// Stringify via fmt.Sprintf so the caller's symbol-
			// resolution pass picks it up.
			out.Concept = fmt.Sprintf("%v", v)
		}
	}

	return out, nil
}
