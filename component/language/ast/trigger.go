package ast

import (
	"fmt"
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
