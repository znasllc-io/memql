package polyphon

import "testing"

func TestParseMentions_AddresseeVsReference(t *testing.T) {
	participants := []ParticipantRef{
		{ID: "agent-stella", Name: "Stella", ParticipantType: "agent"},
		{ID: "agent-jade", Name: "Jade", ParticipantType: "agent"},
		{ID: "agent-sofia", Name: "Sofia", ParticipantType: "agent"},
		{ID: "human-alice", Name: "Alice", ParticipantType: "human"},
	}

	cases := []struct {
		name         string
		text         string
		wantName     string
		wantRole     MentionRole
		wantType     string
		wantPosition string
	}{
		{
			name:         "start-of-utterance @ is addressee",
			text:         "@Stella what do you think?",
			wantName:     "Stella",
			wantRole:     MentionRoleAddressee,
			wantType:     "agent",
			wantPosition: "start",
		},
		{
			name:         "greeting prefix @ is addressee",
			text:         "hey @Jade",
			wantName:     "Jade",
			wantRole:     MentionRoleAddressee,
			wantType:     "agent",
			wantPosition: "start",
		},
		{
			name:         "leading connective + @ is addressee",
			text:         "so @Stella, what now?",
			wantName:     "Stella",
			wantRole:     MentionRoleAddressee,
			wantType:     "agent",
			wantPosition: "mid",
		},
		{
			name:         "about @ is reference (the bug case)",
			text:         "can you tell me more about @Stella",
			wantName:     "Stella",
			wantRole:     MentionRoleReference,
			wantType:     "agent",
			wantPosition: "mid",
		},
		{
			name:         "regarding @ is reference",
			text:         "what's your take regarding @Jade's point?",
			wantName:     "Jade",
			wantRole:     MentionRoleReference,
			wantType:     "agent",
			wantPosition: "mid",
		},
		{
			name:         "with @ is reference",
			text:         "I spoke with @Jade yesterday",
			wantName:     "Jade",
			wantRole:     MentionRoleReference,
			wantType:     "agent",
			wantPosition: "mid",
		},
		{
			name:         "human addressee tagged participantType=human",
			text:         "@Alice what did you decide?",
			wantName:     "Alice",
			wantRole:     MentionRoleAddressee,
			wantType:     "human",
			wantPosition: "start",
		},
		{
			name:         "about @ human is still reference",
			text:         "can you tell me more about @Alice",
			wantName:     "Alice",
			wantRole:     MentionRoleReference,
			wantType:     "human",
			wantPosition: "mid",
		},
		// --- Suffix-shape addressee detection (the production bug) ---
		// User was working with Vale, then mid-utterance pivoted:
		//   "alright before we do that @Sofia can you change the appearance"
		// Without suffix detection this fell to the default reference
		// case (5-token prefix, "that" not a referent prep), and Vale
		// continued the turn instead of Sofia. The "can you ..." after
		// @Sofia is the unambiguous addressee signal.
		{
			name:         "long lead-in with @X can you... is addressee",
			text:         "alright before we do that @Sofia can you change the appearance to dark please",
			wantName:     "Sofia",
			wantRole:     MentionRoleAddressee,
			wantType:     "agent",
			wantPosition: "mid",
		},
		{
			name:         "@X please ... is addressee",
			text:         "and one more thing @Stella please send the email",
			wantName:     "Stella",
			wantRole:     MentionRoleAddressee,
			wantType:     "agent",
			wantPosition: "mid",
		},
		{
			name:         "@X what ... is addressee",
			text:         "ok cool, @Jade what do you think about this approach",
			wantName:     "Jade",
			wantRole:     MentionRoleAddressee,
			wantType:     "agent",
			wantPosition: "mid",
		},
		{
			name:         "@X, can you ... (vocative comma) is addressee",
			text:         "thanks for that, @Stella, can you also handle the X case",
			wantName:     "Stella",
			wantRole:     MentionRoleAddressee,
			wantType:     "agent",
			wantPosition: "mid",
		},
		{
			name:         "@X show me ... is addressee",
			text:         "while we're here @Jade show me the dashboard",
			wantName:     "Jade",
			wantRole:     MentionRoleAddressee,
			wantType:     "agent",
			wantPosition: "mid",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mentions, _ := ParseMentions(tc.text, participants)
			if len(mentions) != 1 {
				t.Fatalf("got %d mentions, want 1", len(mentions))
			}
			m := mentions[0]
			if m.Name != tc.wantName {
				t.Errorf("name = %q, want %q", m.Name, tc.wantName)
			}
			if m.Role != tc.wantRole {
				t.Errorf("role = %q, want %q", m.Role, tc.wantRole)
			}
			if m.ParticipantType != tc.wantType {
				t.Errorf("participantType = %q, want %q", m.ParticipantType, tc.wantType)
			}
			if m.Position != tc.wantPosition {
				t.Errorf("position = %q, want %q", m.Position, tc.wantPosition)
			}
		})
	}
}

func TestParseMentions_MultipleMentions(t *testing.T) {
	participants := []ParticipantRef{
		{ID: "agent-stella", Name: "Stella", ParticipantType: "agent"},
		{ID: "agent-jade", Name: "Jade", ParticipantType: "agent"},
	}

	// Stella addressed at start, Jade referenced after "than".
	text := "@Stella, are you jumping in because you know more than @Jade?"
	mentions, _ := ParseMentions(text, participants)

	if len(mentions) != 2 {
		t.Fatalf("got %d mentions, want 2", len(mentions))
	}

	var stella, jade *Mention
	for i := range mentions {
		switch mentions[i].Name {
		case "Stella":
			stella = &mentions[i]
		case "Jade":
			jade = &mentions[i]
		}
	}
	if stella == nil || jade == nil {
		t.Fatalf("missing one mention: stella=%v jade=%v", stella, jade)
	}
	if stella.Role != MentionRoleAddressee {
		t.Errorf("Stella role = %q, want addressee", stella.Role)
	}
	// "than @Jade" -- "than" is not in either map, so default is reference.
	if jade.Role != MentionRoleReference {
		t.Errorf("Jade role = %q, want reference", jade.Role)
	}
}
