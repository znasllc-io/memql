package memql

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestLoadVoiceSessionStartState_CombinesLegs pins the #1429 combine: every
// leg's result lands in the ack state, with the channel-mode layering applied
// per channel independently (override wins; valid agent-record control is the
// fallback; "mirror_user" is the floor).
func TestLoadVoiceSessionStartState_CombinesLegs(t *testing.T) {
	state := loadVoiceSessionStartState(voiceSessionStartLoads{
		audioOverride: func() (string, bool) { return "always_on", true },
		videoOverride: func() (string, bool) { return "", false }, // no override row
		agentRecord: func() agentRecordFields {
			return agentRecordFields{
				persona: agentPersonaFields{
					name:        "Sofia",
					role:        "Concierge",
					description: "Helpful",
					personality: "Warm",
				},
				audioControl: "always_off", // must LOSE to the audio override
				videoControl: "always_off", // must WIN (no video override)
			}
		},
		avatarPersona: func() string { return "face-123" },
	})

	assert.Equal(t, "always_on", state.audioMode, "override row must win over the agent-record control")
	assert.Equal(t, "always_off", state.videoMode, "agent-record control must apply when no override row matches")
	assert.Equal(t, "Sofia", state.persona.name)
	assert.Equal(t, "Concierge", state.persona.role)
	assert.Equal(t, "Helpful", state.persona.description)
	assert.Equal(t, "Warm", state.persona.personality)
	assert.Equal(t, "face-123", state.avatarPersonaId)
}

// TestLoadVoiceSessionStartState_FailedLegsFallBack pins the per-leg
// degradation: a failed override leg (ok=false) plus a failed agent-record
// leg (zero value) must still produce a complete ack state -- "mirror_user"
// modes and empty persona -- never an error or a missing field.
func TestLoadVoiceSessionStartState_FailedLegsFallBack(t *testing.T) {
	state := loadVoiceSessionStartState(voiceSessionStartLoads{
		audioOverride: func() (string, bool) { return "", false },
		videoOverride: func() (string, bool) { return "", false },
		agentRecord:   func() agentRecordFields { return agentRecordFields{} },
		avatarPersona: func() string { return "" },
	})

	assert.Equal(t, "mirror_user", state.audioMode)
	assert.Equal(t, "mirror_user", state.videoMode)
	assert.Equal(t, agentPersonaFields{}, state.persona)
	assert.Equal(t, "", state.avatarPersonaId)
}

// TestLoadVoiceSessionStartState_NilLegsFallBack guards the nil-closure path
// (defensive -- the handler always supplies all three).
func TestLoadVoiceSessionStartState_NilLegsFallBack(t *testing.T) {
	state := loadVoiceSessionStartState(voiceSessionStartLoads{})
	assert.Equal(t, "mirror_user", state.audioMode)
	assert.Equal(t, "mirror_user", state.videoMode)
	assert.Equal(t, agentPersonaFields{}, state.persona)
}

// TestLoadVoiceSessionStartState_LegsRunConcurrently proves the four legs
// overlap (the point of #1429: the sequential engine round-trips -> one
// parallel stage). Each leg signals it started and then blocks until released;
// a watcher releases everyone only once ALL FOUR have started. Sequential
// execution would never get all four started before the watcher's timeout.
func TestLoadVoiceSessionStartState_LegsRunConcurrently(t *testing.T) {
	const legs = 4
	started := make(chan struct{}, legs)
	release := make(chan struct{})
	var sawAll atomic.Bool

	go func() {
		defer close(release) // always release so a failure doesn't hang the test
		for i := 0; i < legs; i++ {
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				return // sawAll stays false -> assertion below fails
			}
		}
		sawAll.Store(true)
	}()

	leg := func() {
		started <- struct{}{}
		<-release
	}
	done := make(chan voiceSessionStartState, 1)
	go func() {
		done <- loadVoiceSessionStartState(voiceSessionStartLoads{
			audioOverride: func() (string, bool) { leg(); return "always_on", true },
			videoOverride: func() (string, bool) { leg(); return "always_off", true },
			agentRecord: func() agentRecordFields {
				leg()
				return agentRecordFields{persona: agentPersonaFields{name: "Sofia"}}
			},
			avatarPersona: func() string { leg(); return "face-123" },
		})
	}()

	select {
	case state := <-done:
		assert.True(t, sawAll.Load(), "all four legs must be in flight at once (parallel, not sequential)")
		// And all results still land after the concurrent join.
		assert.Equal(t, "always_on", state.audioMode)
		assert.Equal(t, "always_off", state.videoMode)
		assert.Equal(t, "Sofia", state.persona.name)
		assert.Equal(t, "face-123", state.avatarPersonaId)
	case <-time.After(10 * time.Second):
		t.Fatal("loadVoiceSessionStartState did not return")
	}
}

// TestEffectiveChannelMode pins the layering table shared by session-start and
// the speak path: override > VALID control default > mirror_user. An invalid
// control value (junk in the agent record) must not leak to the wire.
func TestEffectiveChannelMode(t *testing.T) {
	cases := []struct {
		name       string
		override   string
		overrideOK bool
		control    string
		want       string
	}{
		{"override wins", "always_on", true, "always_off", "always_on"},
		{"valid control fallback", "", false, "always_off", "always_off"},
		{"invalid control falls through", "", false, "sometimes", "mirror_user"},
		{"empty everything", "", false, "", "mirror_user"},
		{"mirror_user control is valid", "", false, "mirror_user", "mirror_user"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, effectiveChannelMode(tc.override, tc.overrideOK, tc.control))
		})
	}
}
