package cognition

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/polyphon"
)

// TestBuildRoutingSchema verifies the schema produced for structured
// routing is shaped correctly and, crucially, constrains `agentId` to
// the current candidate set -- this is the guarantee that eliminates
// the "SI router returned unknown agent" failure mode entirely.
func TestBuildRoutingSchema(t *testing.T) {
	t.Run("agentId is enum-constrained when candidates exist", func(t *testing.T) {
		cands := []polyphon.AgentCandidate{
			{ID: "agent-pearl", Name: "Pearl"},
			{ID: "agent-zara", Name: "Zara"},
		}
		raw := buildRoutingSchema(cands)

		var schema map[string]any
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("schema is not valid JSON: %v", err)
		}

		props, _ := schema["properties"].(map[string]any)
		agentId, _ := props["agentId"].(map[string]any)
		enum, _ := agentId["enum"].([]any)

		if len(enum) != 2 {
			t.Fatalf("expected 2 enum values, got %d: %v", len(enum), enum)
		}
		got := map[string]bool{}
		for _, v := range enum {
			if s, ok := v.(string); ok {
				got[s] = true
			}
		}
		if !got["agent-pearl"] || !got["agent-zara"] {
			t.Errorf("enum missing candidate: %v", got)
		}
	})

	t.Run("additionalProperties is false (OpenAI strict mode requirement)", func(t *testing.T) {
		raw := buildRoutingSchema(nil)
		var schema map[string]any
		_ = json.Unmarshal(raw, &schema)
		if ap, _ := schema["additionalProperties"].(bool); ap != false {
			t.Errorf("additionalProperties=%v, want false", ap)
		}
	})

	t.Run("all required fields listed (OpenAI strict mode requirement)", func(t *testing.T) {
		raw := buildRoutingSchema(nil)
		var schema map[string]any
		_ = json.Unmarshal(raw, &schema)
		required, _ := schema["required"].([]any)
		want := []string{
			"respond", "agentId", "agentName", "reason",
			"handoff", "handoffFrom", "toolsNeeded",
			"fitScore", "turnMode",
		}
		if len(required) != len(want) {
			t.Fatalf("required = %v (len %d), want %v (len %d)", required, len(required), want, len(want))
		}
		have := map[string]bool{}
		for _, v := range required {
			if s, ok := v.(string); ok {
				have[s] = true
			}
		}
		for _, w := range want {
			if !have[w] {
				t.Errorf("required missing field %q", w)
			}
		}
	})

	t.Run("degenerate (no candidates) yields unrestricted agentId", func(t *testing.T) {
		raw := buildRoutingSchema(nil)
		var schema map[string]any
		_ = json.Unmarshal(raw, &schema)
		props, _ := schema["properties"].(map[string]any)
		agentId, _ := props["agentId"].(map[string]any)
		if _, hasEnum := agentId["enum"]; hasEnum {
			t.Errorf("expected no enum on agentId when candidates empty, got: %v", agentId)
		}
	})

	t.Run("candidates with empty IDs are skipped", func(t *testing.T) {
		cands := []polyphon.AgentCandidate{
			{ID: "agent-pearl", Name: "Pearl"},
			{ID: "", Name: "Ghost"},
			{ID: "  ", Name: "Whitespace"},
			{ID: "agent-zara", Name: "Zara"},
		}
		raw := buildRoutingSchema(cands)
		var schema map[string]any
		_ = json.Unmarshal(raw, &schema)
		props, _ := schema["properties"].(map[string]any)
		agentId, _ := props["agentId"].(map[string]any)
		enum, _ := agentId["enum"].([]any)
		if len(enum) != 2 {
			t.Errorf("expected 2 valid IDs enumerated, got %d: %v", len(enum), enum)
		}
	})
}

func TestFastPathRoute(t *testing.T) {
	jade := polyphon.AgentCandidate{ID: "agent-jade", Name: "Jade"}
	stella := polyphon.AgentCandidate{ID: "agent-stella", Name: "Stella"}
	vale := polyphon.AgentCandidate{ID: "agent-vale", Name: "Vale"}
	candidates := []polyphon.AgentCandidate{jade, stella, vale}

	cases := []struct {
		name        string
		mentions    []polyphon.Mention
		want        *expected
		lastSpeaker *polyphon.TranscriptEntry
	}{
		{
			name: "single agent addressee routes via fast path",
			mentions: []polyphon.Mention{{
				ParticipantId:   "agent-jade",
				Name:            "Jade",
				ParticipantType: "agent",
				Role:            polyphon.MentionRoleAddressee,
			}},
			want: &expected{agentId: "agent-jade", handoff: false},
		},
		{
			name: "single addressee with different previous responder flags handoff",
			mentions: []polyphon.Mention{{
				ParticipantId:   "agent-jade",
				Name:            "Jade",
				ParticipantType: "agent",
				Role:            polyphon.MentionRoleAddressee,
			}},
			lastSpeaker: &polyphon.TranscriptEntry{SpeakerId: "agent-stella", SpeakerName: "Stella", SpeakerType: "agent"},
			want:        &expected{agentId: "agent-jade", handoff: true, handoffFrom: "Stella"},
		},
		{
			name: "single addressee with same previous responder has no handoff",
			mentions: []polyphon.Mention{{
				ParticipantId:   "agent-jade",
				Name:            "Jade",
				ParticipantType: "agent",
				Role:            polyphon.MentionRoleAddressee,
			}},
			lastSpeaker: &polyphon.TranscriptEntry{SpeakerId: "agent-jade", SpeakerName: "Jade", SpeakerType: "agent"},
			want:        &expected{agentId: "agent-jade", handoff: false},
		},
		{
			name: "reference-only mention without previous responder falls through to LLM",
			mentions: []polyphon.Mention{{
				ParticipantId:   "agent-jade",
				Name:            "Jade",
				ParticipantType: "agent",
				Role:            polyphon.MentionRoleReference,
			}},
			want: nil,
		},
		{
			name: "reference-only mention WITH previous responder routes to previous responder (THE BUG CASE)",
			mentions: []polyphon.Mention{{
				ParticipantId:   "agent-stella",
				Name:            "Stella",
				ParticipantType: "agent",
				Role:            polyphon.MentionRoleReference,
			}},
			lastSpeaker: &polyphon.TranscriptEntry{SpeakerId: "agent-jade", SpeakerName: "Jade", SpeakerType: "agent"},
			want:        &expected{agentId: "agent-jade", handoff: false},
		},
		{
			name: "multiple reference mentions with previous responder routes to previous responder",
			mentions: []polyphon.Mention{
				{ParticipantId: "agent-stella", Name: "Stella", ParticipantType: "agent", Role: polyphon.MentionRoleReference},
				{ParticipantId: "agent-vale", Name: "Vale", ParticipantType: "agent", Role: polyphon.MentionRoleReference},
			},
			lastSpeaker: &polyphon.TranscriptEntry{SpeakerId: "agent-jade", SpeakerName: "Jade", SpeakerType: "agent"},
			want:        &expected{agentId: "agent-jade", handoff: false},
		},
		{
			name: "reference-only mention with previous responder not in candidates falls through to LLM",
			mentions: []polyphon.Mention{{
				ParticipantId:   "agent-stella",
				Name:            "Stella",
				ParticipantType: "agent",
				Role:            polyphon.MentionRoleReference,
			}},
			lastSpeaker: &polyphon.TranscriptEntry{SpeakerId: "agent-ghost", SpeakerName: "Ghost", SpeakerType: "agent"},
			want:        nil,
		},
		{
			name:     "no mentions at all falls through to LLM",
			mentions: nil,
			want:     nil,
		},
		{
			name: "human addressee falls through to LLM",
			mentions: []polyphon.Mention{{
				ParticipantId:   "human-alice",
				Name:            "Alice",
				ParticipantType: "human",
				Role:            polyphon.MentionRoleAddressee,
			}},
			want: nil,
		},
		{
			name: "multiple agent addressees fall through to LLM",
			mentions: []polyphon.Mention{
				{ParticipantId: "agent-jade", Name: "Jade", ParticipantType: "agent", Role: polyphon.MentionRoleAddressee},
				{ParticipantId: "agent-stella", Name: "Stella", ParticipantType: "agent", Role: polyphon.MentionRoleAddressee},
			},
			want: nil,
		},
		{
			name: "addressee for agent not in the space falls through to LLM",
			mentions: []polyphon.Mention{{
				ParticipantId:   "agent-unknown",
				Name:            "Phantom",
				ParticipantType: "agent",
				Role:            polyphon.MentionRoleAddressee,
			}},
			want: nil,
		},
		{
			name: "mixed addressee + reference still takes the fast path on single addressee",
			mentions: []polyphon.Mention{
				{ParticipantId: "agent-jade", Name: "Jade", ParticipantType: "agent", Role: polyphon.MentionRoleAddressee},
				{ParticipantId: "agent-stella", Name: "Stella", ParticipantType: "agent", Role: polyphon.MentionRoleReference},
			},
			want: &expected{agentId: "agent-jade", handoff: false},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var session *polyphon.PolyphonSession
			if tc.lastSpeaker != nil {
				session = polyphon.NewSession("test-space")
				entry := *tc.lastSpeaker
				entry.Timestamp = time.Now()
				session.AddTranscript(entry)
			}

			utterance := polyphon.Utterance{Mentions: tc.mentions}
			got := tryFastPathRoute(utterance, candidates, session)

			if tc.want == nil {
				if got != nil {
					t.Fatalf("expected fall-through to LLM, got fast-path outcome: %+v", got.Winner)
				}
				return
			}

			if got == nil {
				t.Fatalf("expected fast-path outcome for %s, got nil", tc.want.agentId)
			}
			if !got.Respond {
				t.Errorf("Respond = false, want true")
			}
			if got.Winner == nil || got.Winner.AgentId != tc.want.agentId {
				t.Errorf("winner = %v, want agentId=%s", got.Winner, tc.want.agentId)
			}
			if got.Handoff != tc.want.handoff {
				t.Errorf("Handoff = %v, want %v", got.Handoff, tc.want.handoff)
			}
			if got.HandoffFrom != tc.want.handoffFrom {
				t.Errorf("HandoffFrom = %q, want %q", got.HandoffFrom, tc.want.handoffFrom)
			}
		})
	}
}

type expected struct {
	agentId     string
	handoff     bool
	handoffFrom string
}

// TestParseRoutingResult covers the provider-independent parse path:
// raw JSON string, markdown-fenced JSON, already-parsed map, and the
// round-tripped-via-proto catch-all branch.
func TestParseRoutingResult(t *testing.T) {
	cases := []struct {
		name    string
		input   any
		want    routingResult
		wantErr bool
	}{
		{
			name:  "raw JSON string",
			input: `{"respond":true,"agentId":"agent-jade","agentName":"Jade","reason":"Jade fits","handoff":false,"toolsNeeded":false}`,
			want: routingResult{
				Respond: true, AgentId: "agent-jade", AgentName: "Jade",
				Reason: "Jade fits", Handoff: false, ToolsNeeded: false,
			},
		},
		{
			name:  "markdown-fenced JSON string",
			input: "```json\n{\"respond\":true,\"agentId\":\"agent-pearl\",\"agentName\":\"Pearl\",\"reason\":\"Accounting fit\"}\n```",
			want: routingResult{
				Respond: true, AgentId: "agent-pearl", AgentName: "Pearl",
				Reason: "Accounting fit",
			},
		},
		{
			name:  "already-parsed map",
			input: map[string]any{"respond": true, "agentId": "agent-vale", "agentName": "Vale", "reason": "IT fit", "handoff": true, "handoffFrom": "Jade", "toolsNeeded": false},
			want: routingResult{
				Respond: true, AgentId: "agent-vale", AgentName: "Vale",
				Reason: "IT fit", Handoff: true, HandoffFrom: "Jade",
			},
		},
		{
			name:  "silence decision (respond=false)",
			input: `{"respond":false,"reason":"addressed to human"}`,
			want:  routingResult{Respond: false, Reason: "addressed to human"},
		},
		{
			name:    "invalid JSON string errors",
			input:   "not json at all",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRoutingResult(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got result %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("parsed = %+v\nwant   = %+v", got, tc.want)
			}
		})
	}
}

// TestReconcileRoutingWithReason locks in the defensive safety net
// that catches the chat-router model's self-contradictions. Pattern:
// the LLM fills the reason with a correct agent name but the
// agentName field with someone else (usually the assistant).
// Each case is a realistic LLM output we've either observed in logs
// or can reasonably expect.
func TestReconcileRoutingWithReason(t *testing.T) {
	zara := polyphon.AgentCandidate{ID: "agent-zara", Name: "Zara"}
	pearl := polyphon.AgentCandidate{ID: "agent-pearl", Name: "Pearl"}
	atlas := polyphon.AgentCandidate{ID: "agent-atlas", Name: "Atlas"}
	sara := polyphon.AgentCandidate{ID: "agent-sara", Name: "Sara"}
	cands := []polyphon.AgentCandidate{zara, pearl, atlas, sara}

	cases := []struct {
		name       string
		agentName  string
		reason     string
		candidates []polyphon.AgentCandidate
		wantSwap   bool
		wantId     string
	}{
		{
			name:       "observed bug: reason names Pearl but agentName is Zara (should respond)",
			agentName:  "Zara",
			reason:     "The user greeted Pearl directly by name, so Pearl should respond.",
			candidates: cands,
			wantSwap:   true,
			wantId:     "agent-pearl",
		},
		{
			name:       "reason uses 'pick X' -- should swap",
			agentName:  "Zara",
			reason:     "Accounting domain fit -- pick Pearl",
			candidates: cands,
			wantSwap:   true,
			wantId:     "agent-pearl",
		},
		{
			name:       "reason uses 'greeted X directly' -- should swap",
			agentName:  "Zara",
			reason:     "User greeted Atlas directly in the vocative slot.",
			candidates: cands,
			wantSwap:   true,
			wantId:     "agent-atlas",
		},
		{
			name:       "reason uses 'X is the addressee' -- should swap",
			agentName:  "Zara",
			reason:     "Sara is the addressee based on the mention roles",
			candidates: cands,
			wantSwap:   true,
			wantId:     "agent-sara",
		},
		{
			name:       "reason and agentName agree -- no swap",
			agentName:  "Pearl",
			reason:     "Pearl should respond to the accounting question.",
			candidates: cands,
			wantSwap:   false,
		},
		{
			name:       "reason names no candidate -- no swap",
			agentName:  "Zara",
			reason:     "General greeting to the space; assistant welcomes.",
			candidates: cands,
			wantSwap:   false,
		},
		{
			name:       "reason mentions Pearl as a topic only, no directive pattern -- no swap",
			agentName:  "Zara",
			reason:     "User asked about Pearl but the addressee is Zara.",
			candidates: cands,
			wantSwap:   false,
		},
		{
			name:       "reason names an agent not in candidates -- no swap",
			agentName:  "Zara",
			reason:     "Phantom should respond.",
			candidates: cands,
			wantSwap:   false,
		},
		{
			name:       "case-insensitive reason match",
			agentName:  "Zara",
			reason:     "PEARL SHOULD RESPOND (shouting).",
			candidates: cands,
			wantSwap:   true,
			wantId:     "agent-pearl",
		},
		{
			name:       "empty reason -- no swap",
			agentName:  "Zara",
			reason:     "",
			candidates: cands,
			wantSwap:   false,
		},
		{
			name:       "empty candidates -- no swap",
			agentName:  "Zara",
			reason:     "Pearl should respond",
			candidates: nil,
			wantSwap:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			routing := &routingResult{
				Respond:   true,
				AgentName: tc.agentName,
				Reason:    tc.reason,
			}
			got, swapped := reconcileRoutingWithReason(routing, tc.candidates)
			if swapped != tc.wantSwap {
				t.Fatalf("swapped = %v, want %v (got candidate %+v)", swapped, tc.wantSwap, got)
			}
			if tc.wantSwap && got.ID != tc.wantId {
				t.Errorf("swapped to %s, want %s", got.ID, tc.wantId)
			}
		})
	}
}
