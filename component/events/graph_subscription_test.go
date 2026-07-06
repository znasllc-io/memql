package events

import (
	"reflect"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// TestGraphSubscriptionPatterns pins the server-side topic composition for
// structured graph subscriptions (memql#2460): the client sends a concept
// type id + CDC verbs, the server composes the bus pattern(s).
func TestGraphSubscriptionPatterns(t *testing.T) {
	tests := []struct {
		name    string
		concept string
		actions []string
		want    []string
	}{
		{
			name:    "concept + single action",
			concept: "v1:cognition:utterance",
			actions: []string{"created"},
			want:    []string{"graph.node.created.v1:cognition:utterance"},
		},
		{
			name:    "concept + multiple actions (one pattern per verb)",
			concept: "v1:cognition:utterance",
			actions: []string{"created", "updated"},
			want: []string{
				"graph.node.created.v1:cognition:utterance",
				"graph.node.updated.v1:cognition:utterance",
			},
		},
		{
			name:    "concept + all actions (empty -> single action wildcard)",
			concept: "v1:cognition:utterance",
			actions: nil,
			want:    []string{"graph.node.*.v1:cognition:utterance"},
		},
		{
			name:    "all concepts + single action",
			concept: "",
			actions: []string{"created"},
			want:    []string{"graph.node.created.#"},
		},
		{
			name:    "all concepts + all actions",
			concept: "",
			actions: nil,
			want:    []string{"graph.node.*.#"},
		},
		{
			name:    "multi-segment concept type",
			concept: "v1:cognition:text:chunk",
			actions: []string{"created"},
			want:    []string{"graph.node.created.v1:cognition:text:chunk"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GraphSubscriptionPatterns(tt.concept, tt.actions)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("GraphSubscriptionPatterns(%q, %v) = %v, want %v", tt.concept, tt.actions, got, tt.want)
			}
			// The composition is only correct if the bus matcher agrees: a
			// concrete CDC topic must match the pattern set iff its verb was
			// selected (empty actions = all verbs).
			concept := tt.concept
			if concept == "" {
				concept = "v1:some:concept"
			}
			for _, verb := range []string{"created", "updated", "deleted"} {
				topic := "graph.node." + verb + "." + concept
				if want, have := verbSelected(tt.actions, verb), anyMatch(got, topic); want != have {
					t.Fatalf("topic %q: match=%v, want %v (patterns=%v)", topic, have, want, got)
				}
			}
		})
	}
}

func verbSelected(actions []string, verb string) bool {
	if len(actions) == 0 {
		return true
	}
	for _, a := range actions {
		if a == verb {
			return true
		}
	}
	return false
}

func anyMatch(patterns []string, topic string) bool {
	for _, p := range patterns {
		if Match(p, topic) {
			return true
		}
	}
	return false
}

// TestGraphSubscriptionPatterns_ActionIsolation proves a subset of actions
// does NOT match the excluded verb -- the reason we compose one pattern per
// verb instead of a single wildcard when actions is a proper subset.
func TestGraphSubscriptionPatterns_ActionIsolation(t *testing.T) {
	patterns := GraphSubscriptionPatterns("v1:cognition:utterance", []string{"created"})
	if anyMatch(patterns, "graph.node.deleted.v1:cognition:utterance") {
		t.Fatalf("created-only subscription must not match a deleted topic; patterns=%v", patterns)
	}
	if anyMatch(patterns, "graph.node.created.v1:cognition:space") {
		t.Fatalf("concept-scoped subscription must not match a different concept; patterns=%v", patterns)
	}
}

func TestGraphNodeActionVerb(t *testing.T) {
	tests := []struct {
		in       memqlv1.GraphNodeAction
		wantVerb string
		wantOK   bool
	}{
		{memqlv1.GraphNodeAction_GRAPH_NODE_ACTION_CREATED, "created", true},
		{memqlv1.GraphNodeAction_GRAPH_NODE_ACTION_UPDATED, "updated", true},
		{memqlv1.GraphNodeAction_GRAPH_NODE_ACTION_DELETED, "deleted", true},
		{memqlv1.GraphNodeAction_GRAPH_NODE_ACTION_UNSPECIFIED, "", false},
	}
	for _, tt := range tests {
		verb, ok := GraphNodeActionVerb(tt.in)
		if verb != tt.wantVerb || ok != tt.wantOK {
			t.Fatalf("GraphNodeActionVerb(%v) = (%q, %v), want (%q, %v)", tt.in, verb, ok, tt.wantVerb, tt.wantOK)
		}
	}
}
