package readiness

import (
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

func ago(d time.Duration) time.Time { return fixedNow.Add(-d) }

func laneNamed(t *testing.T, lanes []LaneReport, name string) LaneReport {
	t.Helper()
	for _, l := range lanes {
		if l.Name == name {
			return l
		}
	}
	t.Fatalf("no %q lane in %+v", name, lanes)
	return LaneReport{}
}

func liveSlot(t *testing.T, l LaneReport) bool {
	t.Helper()
	for _, s := range l.Slots {
		if s.Name == InferenceLiveSlot {
			return s.Present
		}
	}
	t.Fatalf("lane %q carries no %q slot: %+v", l.Name, InferenceLiveSlot, l)
	return false
}

// A machine offering a qualifying model is CONFIGURED whether or not it is
// awake, and `live` is the only thing its sleep changes. That split is the
// whole of D3: configuration opens the OS, presence decides a call.
func TestAQualifyingModelIsConfiguredEvenWhenAsleep(t *testing.T) {
	machine := RegistrationFacts{
		ConnectedNodeId: "agent-1",
		Labels:          map[string]string{"model:llama3.1:8b": "ctx=8192,structured=true"},
		LastSeenAt:      ago(OnlineWindow + time.Minute),
	}
	lanes := InferenceLanes(InferenceInput{Registrations: []RegistrationFacts{machine}, Now: fixedNow})
	local := laneNamed(t, lanes, InferenceLaneLocal)
	if !local.Complete {
		t.Fatalf("a machine advertising ctx=8192,structured=true is a configured door: %+v", local)
	}
	if liveSlot(t, local) {
		t.Errorf("a lastSeenAt outside the online window must not report live")
	}
	if !InferenceConfigured(lanes) {
		t.Errorf("one complete lane configures the module")
	}
	if InferenceLive(lanes) {
		t.Errorf("no door is open right now, so InferenceLive must be false")
	}

	machine.LastSeenAt = ago(OnlineWindow / 2)
	lanes = InferenceLanes(InferenceInput{Registrations: []RegistrationFacts{machine}, Now: fixedNow})
	if !liveSlot(t, laneNamed(t, lanes, InferenceLaneLocal)) {
		t.Errorf("a heartbeat inside the window is live")
	}
	if !InferenceLive(lanes) {
		t.Errorf("InferenceLive did not follow the lane's own slot")
	}
}

// Every capability defaults to false, the same direction ModelAttributes
// takes: a model that never claimed structured output is not a door, because
// the platform's own prompts parse their answers rather than reading them.
func TestAModelBelowTheFloorIsNotADoor(t *testing.T) {
	for name, value := range map[string]string{
		"no structured output":     "ctx=32768",
		"context below the floor":  "ctx=4096,structured=true",
		"context unstated":         "structured=true",
		"structured spelled wrong": "ctx=32768,structured=probably",
		"garbled":                  "not-a-pair",
		"empty":                    "",
	} {
		lanes := InferenceLanes(InferenceInput{
			Registrations: []RegistrationFacts{{
				Labels:          map[string]string{"model:m": value},
				ConnectedNodeId: "agent-1",
				LastSeenAt:      fixedNow,
			}},
			Now: fixedNow,
		})
		if laneNamed(t, lanes, InferenceLaneLocal).Complete {
			t.Errorf("%s (%q) opened the local door", name, value)
		}
	}
}

// Exactly at the floor is IN. 8192 is the smallest window in which the
// platform's own structured prompts fit at all, so a machine AT it is usable.
func TestTheFloorIsInclusive(t *testing.T) {
	lanes := InferenceLanes(InferenceInput{
		Registrations: []RegistrationFacts{{
			Labels:          map[string]string{"model:m": "ctx=8192,structured=1"},
			ConnectedNodeId: "agent-1",
			LastSeenAt:      fixedNow,
		}},
		Now: fixedNow,
	})
	if !laneNamed(t, lanes, InferenceLaneLocal).Complete {
		t.Errorf("a model exactly at MinContextWindow=%d was refused", MinContextWindow)
	}
}

// Revocation is a decision, and a revoked machine still beating is exactly
// the case that must read as no door at all -- not as a configured door that
// is merely not live.
func TestARevokedMachineIsNoDoor(t *testing.T) {
	lanes := InferenceLanes(InferenceInput{
		Registrations: []RegistrationFacts{{
			Labels: map[string]string{
				"model:llama3.1:8b": "ctx=8192,structured=true",
				"app:claude-code":   "2.1",
			},
			Apps:            []RegistrationApp{{Id: "claude-code", SignedIn: true, Allowed: true}},
			ConnectedNodeId: "agent-1",
			LastSeenAt:      fixedNow,
			RevokedAt:       ago(time.Hour),
		}},
		Now: fixedNow,
	})
	if laneNamed(t, lanes, InferenceLaneLocal).Complete {
		t.Error("a revoked registration opened the local door")
	}
	if laneNamed(t, lanes, InferenceLaneApp).Complete {
		t.Error("a revoked registration opened the app door")
	}
	if InferenceConfigured(lanes) {
		t.Error("a cluster whose only machine is revoked is not configured")
	}
}

// The app door reads the DERIVED label, which component/worker emits only for
// an app that is known, allowed and signed in. Nothing here re-decides that.
func TestTheAppDoorReadsTheDerivedLabel(t *testing.T) {
	lanes := InferenceLanes(InferenceInput{
		Registrations: []RegistrationFacts{{
			Labels:          map[string]string{"app:claude-code": "2.1"},
			ConnectedNodeId: "agent-1",
			LastSeenAt:      fixedNow,
		}},
		Now: fixedNow,
	})
	if !laneNamed(t, lanes, InferenceLaneApp).Complete {
		t.Fatal("an app: label did not open the app door")
	}
	// A bare prefix is not a label. `app:` with nothing after it names no app,
	// and reading it as one would open the door on a malformed row.
	lanes = InferenceLanes(InferenceInput{
		Registrations: []RegistrationFacts{{
			Labels:          map[string]string{"app:": "2.1"},
			ConnectedNodeId: "agent-1",
			LastSeenAt:      fixedNow,
		}},
		Now: fixedNow,
	})
	if laneNamed(t, lanes, InferenceLaneApp).Complete {
		t.Error("a bare app: prefix opened the app door")
	}
}

// The inventory fallback, for a registration written before app labels
// existed. It applies the same three conditions the label derivation does.
func TestTheAppInventoryFallbackNeedsKnownAllowedAndSignedIn(t *testing.T) {
	cases := []struct {
		name string
		app  RegistrationApp
		want bool
	}{
		{"known, allowed, signed in", RegistrationApp{Id: "claude-code", SignedIn: true, Allowed: true}, true},
		{"codex too", RegistrationApp{Id: "codex", SignedIn: true, Allowed: true}, true},
		{"not signed in", RegistrationApp{Id: "claude-code", SignedIn: false, Allowed: true}, false},
		{"not allowed by the machine", RegistrationApp{Id: "claude-code", SignedIn: true, Allowed: false}, false},
		{"an id this engine cannot drive", RegistrationApp{Id: "some-future-app", SignedIn: true, Allowed: true}, false},
	}
	for _, c := range cases {
		lanes := InferenceLanes(InferenceInput{
			Registrations: []RegistrationFacts{{Apps: []RegistrationApp{c.app}, ConnectedNodeId: "agent-1", LastSeenAt: fixedNow}},
			Now:           fixedNow,
		})
		if got := laneNamed(t, lanes, InferenceLaneApp).Complete; got != c.want {
			t.Errorf("%s: app door complete=%v, want %v", c.name, got, c.want)
		}
	}
}

// Federation is STRUCTURAL PRESENCE (D3), so it is configured and live
// together: there is no machine to be asleep and no stream to be held.
func TestFederationIsConfiguredAndLiveTogether(t *testing.T) {
	lanes := InferenceLanes(InferenceInput{FederationConfigured: true, Now: fixedNow})
	fed := laneNamed(t, lanes, InferenceLaneFederation)
	if !fed.Complete {
		t.Fatalf("federation lane is not complete: %+v", fed)
	}
	if !liveSlot(t, fed) {
		t.Errorf("federation is live whenever it is configured")
	}
	if !InferenceConfigured(lanes) {
		t.Errorf("federation alone configures the module")
	}
}

// Three lanes ALWAYS, in the order the default chain tries them. A client
// renders this list, so an absent lane and a lane that is not complete are
// different answers and only the second one is true of a fresh cluster.
func TestThreeLanesAlwaysInChainOrder(t *testing.T) {
	lanes := InferenceLanes(InferenceInput{Now: fixedNow})
	names := make([]string, 0, len(lanes))
	for _, l := range lanes {
		names = append(names, l.Name)
	}
	if len(names) != 3 || names[0] != InferenceLaneLocal || names[1] != InferenceLaneApp || names[2] != InferenceLaneFederation {
		t.Fatalf("lanes are %v, want [local app federation]", names)
	}
	for _, l := range lanes {
		if len(l.Slots) != 1 || l.Slots[0].Name != InferenceLiveSlot {
			t.Errorf("lane %q carries %+v; each carries exactly the live slot", l.Name, l.Slots)
		}
	}
	if InferenceConfigured(lanes) {
		t.Errorf("a cluster with no machines and no federation is not configured")
	}
}

// operatorLabels merge OVER labels for routing, and the same merge decides a
// door: an operator who pinned `model:` on a machine has said it serves it,
// and the router would honour that.
func TestOperatorLabelsAreMergedOverReported(t *testing.T) {
	lanes := InferenceLanes(InferenceInput{
		Registrations: []RegistrationFacts{{
			Labels:          map[string]string{"model:m": "ctx=1024"},
			OperatorLabels:  map[string]string{"model:m": "ctx=8192,structured=true"},
			ConnectedNodeId: "agent-1",
			LastSeenAt:      fixedNow,
		}},
		Now: fixedNow,
	})
	if !laneNamed(t, lanes, InferenceLaneLocal).Complete {
		t.Errorf("the operator's label did not win the merge")
	}
}

// One machine asleep and another awake: the door is configured, and it is
// live because SOMETHING is answering. `live` is a fact about the lane, not
// about the last machine walked.
func TestOneAwakeMachineMakesTheLaneLive(t *testing.T) {
	lanes := InferenceLanes(InferenceInput{
		Registrations: []RegistrationFacts{
			{Labels: map[string]string{"model:m": "ctx=8192,structured=true"}, ConnectedNodeId: "", LastSeenAt: ago(time.Hour)},
			{Labels: map[string]string{"model:m": "ctx=8192,structured=true"}, ConnectedNodeId: "agent-1", LastSeenAt: fixedNow},
		},
		Now: fixedNow,
	})
	local := laneNamed(t, lanes, InferenceLaneLocal)
	if !local.Complete || !liveSlot(t, local) {
		t.Fatalf("one awake machine did not make the lane live: %+v", local)
	}
}

// A machine that has NEVER been heard from is offline, not online-since-the-
// epoch. Without this the zero time would sit far outside the window and read
// as offline by accident rather than by rule -- the same answer for the wrong
// reason, which stops being the same answer the moment somebody changes the
// window.
func TestAMachineNeverHeardFromIsNotLive(t *testing.T) {
	lanes := InferenceLanes(InferenceInput{
		Registrations: []RegistrationFacts{{
			Labels: map[string]string{"model:m": "ctx=8192,structured=true"},
		}},
		Now: fixedNow,
	})
	local := laneNamed(t, lanes, InferenceLaneLocal)
	if !local.Complete {
		t.Error("a machine that has not checked in is still a configured door")
	}
	if liveSlot(t, local) {
		t.Error("a registration with no lastSeenAt reported live")
	}
}

// A lastSeenAt in the FUTURE is clock skew between the agent replica that
// wrote it and whoever is asking. It stays live, deliberately: a skewed clock
// must not make a live machine disappear.
func TestClockSkewDoesNotHideALiveMachine(t *testing.T) {
	lanes := InferenceLanes(InferenceInput{
		Registrations: []RegistrationFacts{{
			Labels:          map[string]string{"model:m": "ctx=8192,structured=true"},
			ConnectedNodeId: "agent-1",
			LastSeenAt:      fixedNow.Add(time.Minute),
		}},
		Now: fixedNow,
	})
	if !liveSlot(t, laneNamed(t, lanes, InferenceLaneLocal)) {
		t.Error("a lastSeenAt a minute in the future read as offline")
	}
}

func TestHeartbeatAloneIsNotLive(t *testing.T) {
	lanes := InferenceLanes(InferenceInput{
		Registrations: []RegistrationFacts{{
			Labels:     map[string]string{"model:m": "ctx=8192,structured=true"},
			LastSeenAt: fixedNow,
		}},
		Now: fixedNow,
	})
	local := laneNamed(t, lanes, InferenceLaneLocal)
	if !local.Complete {
		t.Fatal("labels still configure the door")
	}
	if liveSlot(t, local) {
		t.Fatal("lastSeenAt without connectedNodeId must not mark the lane live")
	}
}

func TestConnectedNodeIdWithoutBeatIsLive(t *testing.T) {
	lanes := InferenceLanes(InferenceInput{
		Registrations: []RegistrationFacts{{
			Labels:          map[string]string{"model:m": "ctx=8192,structured=true"},
			ConnectedNodeId: "agent-ddwcc",
		}},
		Now: fixedNow,
	})
	if !liveSlot(t, laneNamed(t, lanes, InferenceLaneLocal)) {
		t.Fatal("connectedNodeId with no lastSeenAt flush yet must still read live")
	}
}
