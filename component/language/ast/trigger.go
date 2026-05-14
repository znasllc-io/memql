package ast

import (
	"fmt"
	"strings"
)

// Trigger-topic assembly for the structured @trigger form
// (docs/dsl-import-model-refactor.md decision #6).
//
// Today authors write:
//
//	@trigger(event="graph.node.created.*.v1:cognition:participant")
//
// The new structured form:
//
//	@trigger(event="node.created", concept=cog.participant, partition="*")
//
// The concept reference (`cog.participant`) is a cross-file symbol;
// the loader resolves it to the canonical concept ID
// `v1:cognition:participant` using the importing file's alias table,
// then this package's BuildTriggerTopic assembles the actual
// subscription topic string the engine uses.

// allowedEventKinds is the closed set of `event=` values the new
// structured trigger accepts. Mirrors the action segment of the
// 5-segment topic format the engine emits today
// (graph.node.{action}.{partition}.{concept}).
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

// BuildTriggerTopic assembles the subscription topic from the three
// structured-trigger fields. Returns an error when any field is
// malformed or empty (concept may be empty when the trigger is not
// scoped to a single concept; partition may be "*" for the
// "any partition" wildcard).
//
// Format: graph.{event-action-segments}.{partition}.{concept}
//
//	event="node.created", concept="v1:cognition:participant", partition="*"
//	  ==> "graph.node.created.*.v1:cognition:participant"
//
//	event="node.deleted", concept="", partition="*"
//	  ==> "graph.node.deleted.*" (concept-less, matches any concept)
//
//	event="node.updated", concept="v1:foo:bar", partition="acme"
//	  ==> "graph.node.updated.acme.v1:foo:bar"
//
// Callers that want lenient matching across partitions use "*" as
// the partition; callers that want a specific partition pass that
// partition name verbatim.
func BuildTriggerTopic(eventKind, conceptId, partition string) (string, error) {
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
	if partition == "" {
		return "", fmt.Errorf("partition cannot be empty (use \"*\" for any partition)")
	}

	topic := "graph." + eventKind + "." + partition
	if conceptId != "" {
		topic += "." + conceptId
	}
	return topic, nil
}

// ExtractStructuredTriggerArgs pulls the three structured fields
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
	EventKind   string
	Concept     string
	Partition   string
	HasEvent    bool
	HasConcept  bool
	HasPartition bool
}

// ParseStructuredTriggerArgs reads `event`, `concept`, and
// `partition` from the trigger attribute's Args map. Tolerates
// extra unknown args (e.g. legacy `on=`); the caller decides
// whether to reject them.
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

	if v, ok := args["partition"]; ok {
		s, isStr := v.(string)
		if !isStr {
			return nil, fmt.Errorf("@trigger partition= must be a string, got %T", v)
		}
		out.Partition = s
		out.HasPartition = true
	}

	return out, nil
}
