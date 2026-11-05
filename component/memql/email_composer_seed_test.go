package memql

import "testing"

// TestEmailComposerAgentSeedIsTheRunsActingAgent: an email composition run
// (component/campaigns/compose_run.go) acts as "emailComposerAgent-<owner>", a
// specialist. That is the row the per-user seed materializes for every user: the
// same id the materializer computes, the role the composition's tools require,
// a system agent no person sees in chat, and no provider of its own, so the
// composition's route decides where its model call runs.
func TestEmailComposerAgentSeedIsTheRunsActingAgent(t *testing.T) {
	reg := NewSeedRegistry()
	if _, err := LoadUnifiedSeeds(nil, reg); err != nil {
		t.Fatalf("LoadUnifiedSeeds: %v", err)
	}
	def, ok := reg.Get("emailComposerAgent")
	if !ok {
		t.Fatal("no emailComposerAgent seed")
	}
	if def.Scope != "perUser" || def.UseConcept != "agent" {
		t.Fatalf("emailComposerAgent seed: scope %q, concept %q", def.Scope, def.UseConcept)
	}
	if got := deterministicPerUserSeedId(def, "v1:identity:user:u-1"); got != "emailComposerAgent-u-1" {
		t.Errorf("materialized id = %q, want the run's emailComposerAgent-u-1", got)
	}
	args := buildArgsFromBody(def.Body, def.UseConcept, "emailComposerAgent-u-1", "v1:identity:user:u-1")
	for field, want := range map[string]string{
		"name": "MemQL Email Composer", "role": "specialist", "roleSlug": "system-email-composer", "kind": "system",
	} {
		if args[field] != want {
			t.Errorf("%s = %v, want %q", field, args[field], want)
		}
	}
	if _, has := args["providerConfig"]; has {
		t.Errorf("the composer names a provider of its own: %v", args["providerConfig"])
	}
}
