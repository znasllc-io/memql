package memql

import (
	"testing"

	"github.com/visionarys-io/memql/component/auth"
	"github.com/visionarys-io/memql/component/events"
)

func TestShouldDropPrivateUtteranceForCaller(t *testing.T) {
	t.Parallel()

	const (
		userAlice = "user:alice"
		userBob   = "user:bob"
	)

	privateTopic := "graph.node.created.acme.v1:cognition:privateUtterance"
	otherTopic := "graph.node.created.acme.v1:cognition:utterance"

	cases := []struct {
		name     string
		event    events.Event
		caller   string
		access   *auth.AccessContext
		wantDrop bool
	}{
		{
			name: "drops privateUtterance event for different user",
			event: events.Event{
				Topic:   privateTopic,
				Payload: map[string]any{"forUserId": userAlice},
			},
			caller:   userBob,
			wantDrop: true,
		},
		{
			name: "delivers privateUtterance event for matching user",
			event: events.Event{
				Topic:   privateTopic,
				Payload: map[string]any{"forUserId": userAlice},
			},
			caller:   userAlice,
			wantDrop: false,
		},
		{
			name: "drops privateUtterance event with empty forUserId",
			event: events.Event{
				Topic:   privateTopic,
				Payload: map[string]any{},
			},
			caller:   userAlice,
			wantDrop: true,
		},
		{
			name: "drops privateUtterance event when caller user id is empty",
			event: events.Event{
				Topic:   privateTopic,
				Payload: map[string]any{"forUserId": userAlice},
			},
			caller:   "",
			wantDrop: true,
		},
		{
			name: "cluster owner bypasses filter (cross-user delivery allowed)",
			event: events.Event{
				Topic:   privateTopic,
				Payload: map[string]any{"forUserId": userAlice},
			},
			caller:   userBob,
			access:   &auth.AccessContext{Role: auth.RoleOwner},
			wantDrop: false,
		},
		{
			name: "ignores non-privateUtterance events regardless of payload",
			event: events.Event{
				Topic:   otherTopic,
				Payload: map[string]any{"forUserId": userAlice},
			},
			caller:   userBob,
			wantDrop: false,
		},
		{
			name: "trims whitespace on both sides before comparing",
			event: events.Event{
				Topic:   privateTopic,
				Payload: map[string]any{"forUserId": "  " + userAlice + "  "},
			},
			caller:   userAlice + " ",
			wantDrop: false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := shouldDropPrivateUtteranceForCaller(tc.event, tc.caller, tc.access)
			if got != tc.wantDrop {
				t.Fatalf("got drop=%v, want %v", got, tc.wantDrop)
			}
		})
	}
}

func TestIsPrivateUtteranceTopic(t *testing.T) {
	t.Parallel()

	cases := []struct {
		topic string
		want  bool
	}{
		{"graph.node.created.acme.v1:cognition:privateUtterance", true},
		{"graph.node.created._system.v1:cognition:privateUtterance", true},
		{"graph.node.created.acme.v1:cognition:utterance", false},
		{"graph.node.updated.acme.v1:cognition:privateUtterance", true},
		{"graph.node.created.acme.v1:cognition:participant", false},
		{"telemetry.foo", false},
		{"", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.topic, func(t *testing.T) {
			t.Parallel()
			got := isPrivateUtteranceTopic(tc.topic)
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
