package calls

import "testing"

func TestSimulationOperationScopeAndPayloadBinding(t *testing.T) {
	r := AccountRequest{Action: "simulate_topup", IdempotencyKey: "one-stable-operation", AmountCents: 1000}
	first, digest := SimulationOperation("site-a", "key-a", r)
	second, repeated := SimulationOperation("site-a", "key-a", r)
	if len(first) != 64 || len(digest) != 64 || first != second || digest != repeated {
		t.Fatal("the same durable request must retain both identifiers")
	}
	for _, scope := range [][2]string{{"site-b", "key-a"}, {"site-a", "key-b"}, {"key-a", "site-a"}} {
		changed, samePayload := SimulationOperation(scope[0], scope[1], r)
		if changed == first || samePayload != digest {
			t.Fatal("operation identity must be scoped, with a separately stable payload")
		}
	}
	for _, changed := range []AccountRequest{
		{Action: r.Action, IdempotencyKey: r.IdempotencyKey, AmountCents: 2000},
		{Action: "activate_pass", IdempotencyKey: r.IdempotencyKey, Plan: "day"},
		{Action: "activate_pass", IdempotencyKey: r.IdempotencyKey, Plan: "month"},
	} {
		sameID, changedPayload := SimulationOperation("site-a", "key-a", changed)
		if sameID != first || changedPayload == digest {
			t.Fatal("changed payload must conflict within the same operation")
		}
	}
	r.IdempotencyKey = "another-operation"
	other, samePayload := SimulationOperation("site-a", "key-a", r)
	if other == first || samePayload != digest {
		t.Fatal("distinct operation must not collide with a prior receipt")
	}
}
