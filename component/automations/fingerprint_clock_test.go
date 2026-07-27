package automations

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
)

func ownerEvaluator() *Evaluator {
	e := NewEvaluator()
	bindActorEnvelope(auth.ContextWithAccess(context.Background(),
		&auth.AccessContext{UserId: "v1:identity:user:u-1", Role: auth.RoleOwner}), e)
	return e
}

func fingerprintJSON(t *testing.T, e *Evaluator) string {
	t.Helper()
	b, err := json.Marshal(e.ContextFingerprint())
	if err != nil {
		t.Fatalf("marshal fingerprint: %v", err)
	}
	return string(b)
}

// TestFingerprintIgnoresWallClockCustoms is the regression evidence for
// memql#2823: two fingerprints taken at different instants, with identical
// inputs, must be equal.
//
// `auth.ActorEnvelopeMap` stamps `now` at RFC3339 NANO, and #2801 bound that
// envelope on every automation evaluator. Since ContextFingerprint copied all
// customs into the cache key, the function-step cache went from "rarely hits"
// (a pre-existing second-granularity `timestamp` custom) to a guaranteed miss.
func TestFingerprintIgnoresWallClockCustoms(t *testing.T) {
	first := fingerprintJSON(t, ownerEvaluator())
	time.Sleep(2 * time.Millisecond)
	second := fingerprintJSON(t, ownerEvaluator())

	if first != second {
		t.Errorf("fingerprints differ across instants with identical inputs, so the cache key can never hit:\n  first  = %s\n  second = %s", first, second)
	}

	// The executor's own second-granularity clock seed is the other source.
	withStamp := ownerEvaluator()
	withStamp.SetCustom("timestamp", time.Now().UTC().Format(time.RFC3339))
	later := ownerEvaluator()
	later.SetCustom("timestamp", time.Now().UTC().Add(90*time.Second).Format(time.RFC3339))
	if fingerprintJSON(t, withStamp) != fingerprintJSON(t, later) {
		t.Error("the `timestamp` custom still discriminates the fingerprint; a wall-clock reading is never a meaningful cache key component")
	}
}

// TestFingerprintStillDiscriminatesRealInputs is the other half, and the one
// that matters more: stripping the clock must not strip anything that
// genuinely identifies the work.
//
// This is why the strip is surgical rather than a recursive sweep for
// clock-shaped keys -- `event` is a custom too, so a recursive strip would
// drop `event.payload.timestamp`, a real business field. Removing a genuine
// input from a cache key is a correctness bug; leaving a clock in one is only
// a slow one.
func TestFingerprintStillDiscriminatesRealInputs(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(e *Evaluator)
		mutate2 func(e *Evaluator)
	}{
		{
			name:    "actor identity",
			mutate:  func(e *Evaluator) {},
			mutate2: func(e *Evaluator) { bindNoCallerActorEnvelope(e) },
		},
		{
			name:   "a business field NAMED timestamp inside the event payload",
			mutate: func(e *Evaluator) { e.SetCustom("event", map[string]any{"payload": map[string]any{"timestamp": "A"}}) },
			mutate2: func(e *Evaluator) {
				e.SetCustom("event", map[string]any{"payload": map[string]any{"timestamp": "B"}})
			},
		},
		{
			name:    "a business field NAMED now inside the event payload",
			mutate:  func(e *Evaluator) { e.SetCustom("event", map[string]any{"payload": map[string]any{"now": "A"}}) },
			mutate2: func(e *Evaluator) { e.SetCustom("event", map[string]any{"payload": map[string]any{"now": "B"}}) },
		},
		{
			name:    "bound args",
			mutate:  func(e *Evaluator) { e.SetCustom("args", map[string]any{"spaceId": "s-1"}) },
			mutate2: func(e *Evaluator) { e.SetCustom("args", map[string]any{"spaceId": "s-2"}) },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := ownerEvaluator()
			tc.mutate(a)
			b := ownerEvaluator()
			tc.mutate2(b)
			if fingerprintJSON(t, a) == fingerprintJSON(t, b) {
				t.Errorf("%s does not change the fingerprint; the cache would serve one input's result for another", tc.name)
			}
		})
	}
}

// TestFingerprintKeepsTheRestOfTheActorEnvelope pins that only `now` is
// dropped: the identity fields are exactly what SHOULD discriminate a cached
// result, so over-stripping the envelope would be a correctness bug.
func TestFingerprintKeepsTheRestOfTheActorEnvelope(t *testing.T) {
	fp := ownerEvaluator().ContextFingerprint()
	custom, ok := fp["custom"].(map[string]any)
	if !ok {
		t.Fatalf("fingerprint has no custom map: %#v", fp)
	}
	actor, ok := custom["actor"].(map[string]any)
	if !ok {
		t.Fatalf("fingerprint has no actor envelope: %#v", custom)
	}
	if _, present := actor["now"]; present {
		t.Error("actor.now is still in the fingerprint; it is a nanosecond clock reading and guarantees a cache miss")
	}
	for _, field := range []string{"userId", "role", "isClusterOwner", "identityId", "primaryEmail"} {
		if _, present := actor[field]; !present {
			t.Errorf("actor.%s was dropped from the fingerprint; actor identity must discriminate a cached result", field)
		}
	}
}
