package agent

import (
	"testing"
	"time"
)

// The dispatcher and idle watchdog must agree on what counts as a human in a
// polyphon room: the voice-agent's own identity (the GA agent concept id --
// including a terminating predecessor's ghost during a rollout) and the
// avatar vendor participant are machinery, everything else is a human (#1378).
func TestIsHumanParticipantIdentity(t *testing.T) {
	cases := []struct {
		name     string
		identity string
		want     bool
	}{
		{"SPA participant id", "v1:cognition:participant:9dc3b323", true},
		{"user id", "v1:identity:user:1c65cb0c", true},
		{"guest", "guest:invite-abc", true},
		{"voice-agent (GA identity)", "v1:agents:agent:a9f3b7c2", false},
		{"avatar vendor participant", "avatar-agent", false},
		{"empty", "", false},
		{"whitespace", "   ", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isHumanParticipantIdentity(tc.identity); got != tc.want {
				t.Fatalf("isHumanParticipantIdentity(%q) = %v, want %v", tc.identity, got, tc.want)
			}
		})
	}
}

func TestIdleTeardownAfter(t *testing.T) {
	env := func(vals map[string]string) func(string) string {
		return func(k string) string { return vals[k] }
	}
	cases := []struct {
		name string
		raw  string
		want time.Duration
	}{
		{"unset uses default", "", defaultIdleTeardown},
		{"positive override", "120", 120 * time.Second},
		{"zero falls back", "0", defaultIdleTeardown},
		{"negative falls back", "-5", defaultIdleTeardown},
		{"garbage falls back", "soon", defaultIdleTeardown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := idleTeardownAfter(env(map[string]string{"MEMQL_VOICE_IDLE_TEARDOWN_SECONDS": tc.raw}))
			if got != tc.want {
				t.Fatalf("idleTeardownAfter(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}
