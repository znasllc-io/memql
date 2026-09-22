package memql

import (
	"strings"
	"testing"
)

func TestFleetUnavailableOmitsRevokedAndForeignShareNoise(t *testing.T) {
	err := &FleetUnavailable{
		ModelId: "qwen3-embedding:0.6b",
		Total:   5,
		Considered: map[string]string{
			"v1:worker:registration:active": "does not offer model qwen3-embedding:0.6b",
			"v1:worker:registration:0ba":    "revoked",
			"v1:worker:registration:6ec":    "revoked",
			"v1:worker:registration:24ad":   "Neither consent is present. The owner has not shared it and the machine's policy.yaml does not set inference.serve: cluster. Both are needed.",
		},
		LastError: "worker_disconnected",
	}
	msg := err.Error()
	if strings.Contains(msg, "revoked") && !strings.Contains(msg, "revoked registration(s) omitted") {
		t.Fatalf("raw revoked lines must be omitted, got %q", msg)
	}
	if strings.Contains(msg, "24ad") || strings.Contains(msg, "Both are needed") {
		t.Fatalf("foreign share refusal must be summarised away when actionable reasons remain, got %q", msg)
	}
	if !strings.Contains(msg, "does not offer model") {
		t.Fatalf("actionable reason must remain, got %q", msg)
	}
	if !strings.Contains(msg, "not shared with you") {
		t.Fatalf("want foreign-private summary, got %q", msg)
	}
}

// TestFleetUnavailableNeverNamesAForeignMachineEvenAsTheOnlySignal is epic
// memql#5327, design D12, and it REPLACES a test that pinned the opposite.
//
// The old one was called ...KeepsShareRefusalWhenOnlySignal and asserted that
// when every machine considered was somebody else's private one, their raw ids
// came back -- on the reasoning that they were "the only repair the operator
// has". Half of that was right: the caller does need to be told something, and
// silence would be worse. The other half was not. An id is not a repair. It
// names a row the caller cannot read, belonging to a person they do not learn
// the name of, and the remedy is identical for every one of them.
//
// What it WAS is an enumeration primitive: a list of other users' machine ids
// handed to anybody whose call ran out of options, which alongside finding H-3
// -- a sharing ledger any signed-in caller could ask about any machine -- was
// the second half of a way to learn who had been using them.
//
// So the sentence stays and the ids go. The assertions below are the whole of
// that: the explanation must survive, the count must survive, and no id may.
func TestFleetUnavailableNeverNamesAForeignMachineEvenAsTheOnlySignal(t *testing.T) {
	err := &FleetUnavailable{
		ModelId: "qwen3-embedding:0.6b",
		Total:   2,
		Considered: map[string]string{
			"v1:worker:registration:24ad": "Neither consent is present. Both are needed.",
			"v1:worker:registration:9f1":  "the owner has not shared it",
		},
	}
	msg := err.Error()
	for _, id := range []string{"24ad", "9f1"} {
		if strings.Contains(msg, id) {
			t.Fatalf("a refusal must never name another user's machine, got %q", msg)
		}
	}
	if !strings.Contains(msg, "2 machines") {
		t.Fatalf("the COUNT is what leads somewhere -- there is hardware here and none of it is open to you -- so it must survive: %q", msg)
	}
	if !strings.Contains(msg, "would have to offer") {
		t.Fatalf("the repair must survive the summarising, or the caller is told only that they failed: %q", msg)
	}
}

// TestForeignPrivateSummaryReadsForOneMachine covers the singular, because a
// refusal that says "1 machines" is the kind of thing a person stops reading.
func TestForeignPrivateSummaryReadsForOneMachine(t *testing.T) {
	got := foreignPrivateSummary(1)
	if strings.Contains(got, "1 machines") {
		t.Fatalf("the singular must read as one: %q", got)
	}
	if !strings.Contains(got, "would have to offer it") {
		t.Fatalf("the one-machine sentence must still carry the repair: %q", got)
	}
}

func TestFleetUnavailableSanitizesWrongPodStreamLastError(t *testing.T) {
	err := &FleetUnavailable{
		ModelId:   "black",
		Total:     1,
		LastError: `this replica no longer holds a stream for black`,
		Considered: map[string]string{
			"v1:worker:registration:da0617": "offline",
		},
	}
	msg := err.Error()
	if strings.Contains(msg, "no longer holds a stream") {
		t.Fatalf("wrong-pod stream miss must not surface raw, got %q", msg)
	}
	if !strings.Contains(msg, "holding replica missed") {
		t.Fatalf("want sanitized affinity hint, got %q", msg)
	}
}

func TestFleetUnavailableRegistryMissDoesNotSayRetryConnectedNodeId(t *testing.T) {
	e := &FleetUnavailable{
		ModelId:   "qwen3-embedding:0.6b",
		LastError: `holding replica registry miss for black on agent-pszjr; stream not dispatchable`,
	}
	msg := e.Error()
	if strings.Contains(msg, "retry against connectedNodeId") {
		t.Fatalf("self-holder registry miss must not tell operator to retry connectedNodeId: %s", msg)
	}
	if !strings.Contains(msg, "holding replica registry miss") {
		t.Fatalf("want registry-miss wording, got %s", msg)
	}
}
